// Client for the zgi-sandbox agent-box API (HTTP + SSE). The Agent CLI process
// runs inside a sandbox box; agent-runner bridges stdio over SSE (down) and
// POST (up). See docs/designs/2026-08-16-agent-sandbox-integration-design.md.
import { Readable, Writable } from 'node:stream';

export interface SandboxConfig {
  url: string;
  api_key?: string;
}

export interface Box {
  boxId: string;
  workspacePath: string;
}

export interface ProcessHandle {
  pid: string;
  stdin: Writable;
  stdout: Readable;
  stderr: Readable;
  exitCode: number | null;
  exited: Promise<number | null>;
  kill(): void;
}

interface SandboxClientOptions {
  baseUrl: string;
  apiKey?: string;
}

export function createSandboxClient(opts: SandboxClientOptions): SandboxClient {
  return new SandboxClient(opts);
}

export class SandboxClient {
  private baseUrl: string;
  private apiKey: string | undefined;

  constructor(opts: SandboxClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, '');
    this.apiKey = opts.apiKey;
  }

  private headers(): Record<string, string> {
    return this.apiKey ? { 'X-API-Key': this.apiKey } : {};
  }

  async createBox(req: {
    ttlSeconds?: number;
    networkEnabled?: boolean;
    workspaceSeed?: Record<string, string>;
  }): Promise<Box> {
    const res = await fetch(`${this.baseUrl}/v1/agent-boxes`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...this.headers() },
      body: JSON.stringify({
        ttl_seconds: req.ttlSeconds,
        network_enabled: req.networkEnabled,
        workspace_seed: req.workspaceSeed,
      }),
    });
    const env = (await res.json()) as { data?: { box_id?: string; workspace_path?: string } };
    const data = env.data ?? {};
    if (!res.ok || !data.box_id) {
      throw new Error(`agent box create failed (${res.status}): ${JSON.stringify(env)}`);
    }
    return { boxId: data.box_id, workspacePath: data.workspace_path ?? '' };
  }

  async deleteBox(boxId: string): Promise<void> {
    await fetch(`${this.baseUrl}/v1/agent-boxes/${boxId}`, { method: 'DELETE', headers: this.headers() });
  }

  async writeStdin(boxId: string, pid: string, data: string | Buffer): Promise<void> {
    const res = await fetch(`${this.baseUrl}/v1/agent-boxes/${boxId}/process/${pid}/stdin`, {
      method: 'POST',
      headers: this.headers(),
      body: data,
    });
    if (!res.ok) throw new Error(`agent process stdin failed (${res.status})`);
  }

  /** Send EOF to a boxed process by closing its stdin pipe. */
  async closeStdin(boxId: string, pid: string): Promise<void> {
    const res = await fetch(`${this.baseUrl}/v1/agent-boxes/${boxId}/process/${pid}/stdin/close`, {
      method: 'POST',
      headers: this.headers(),
    });
    if (!res.ok) throw new Error(`agent process stdin close failed (${res.status})`);
  }

  async killProcess(boxId: string, pid: string): Promise<void> {
    await fetch(`${this.baseUrl}/v1/agent-boxes/${boxId}/process/${pid}`, { method: 'DELETE', headers: this.headers() });
  }

  /** Open a streamed process in the box and return a handle whose stdio is bridged over HTTP+SSE. */
  async openProcess(
    boxId: string,
    req: { command: string; args: string[]; env?: Record<string, string> },
    signal?: AbortSignal,
  ): Promise<ProcessHandle> {
    const body = JSON.stringify({
      command: req.command,
      args: req.args,
      env: req.env ?? {},
    });
    const res = await fetch(`${this.baseUrl}/v1/agent-boxes/${boxId}/process`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', ...this.headers() },
      body,
      signal,
    });
    if (!res.ok || !res.body) throw new Error(`agent process start failed (${res.status})`);

    const stdout = new Readable({ read() {} });
    const stderr = new Readable({ read() {} });
    let pid = '';
    let exitCode: number | null = null;

    const reader = res.body.getReader();
    const decoder = new TextDecoder();
    let buf = '';
    let resolvePidReady!: (() => void) | null;
    const pidReady = new Promise<void>((resolve) => {
      resolvePidReady = resolve;
    });
    const exited = (async () => {
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buf += decoder.decode(value, { stream: true });
        let idx: number;
        while ((idx = buf.indexOf('\n\n')) >= 0) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          const line = frame.split('\n').find((l) => l.startsWith('data: '));
          if (!line) continue;
          let evt: Record<string, unknown>;
          try {
            evt = JSON.parse(line.slice(6));
          } catch {
            continue;
          }
          if (evt.type === 'started') {
            pid = String(evt.pid ?? '');
            resolvePidReady?.();
          }
          else if (evt.type === 'stdout') stdout.push(String(evt.data ?? ''));
          else if (evt.type === 'stderr') stderr.push(String(evt.data ?? ''));
          else if (evt.type === 'exit') {
            exitCode = typeof evt.code === 'number' ? evt.code : null;
            stdout.push(null);
            stderr.push(null);
            return exitCode;
          } else if (evt.type === 'error') {
            stdout.push(null);
            stderr.push(null);
            throw new Error(String(evt.message ?? 'agent process error'));
          }
        }
      }
      stdout.push(null);
      stderr.push(null);
      return exitCode;
    })().catch((err) => {
      stdout.destroy(err as Error);
      stderr.destroy(err as Error);
      throw err;
    });

    const stdin = new Writable({
      write: (chunk, _enc, cb) => {
        const send = (): void => {
          this.writeStdin(boxId, pid, chunk as Buffer)
            .then(() => cb())
            .catch((err) => cb(err as Error));
        };
        if (pid) send();
        else void pidReady.then(send);
      },
      // Forward EOF to the boxed process when the caller ends stdin. Codex-style
      // CLIs read their prompt from stdin until EOF before starting; Node runs
      // _final only after pending _write callbacks flush, so ordering is safe.
      final: (cb) => {
        const close = (): void => {
          this.closeStdin(boxId, pid)
            .then(() => cb())
            .catch((err) => cb(err as Error));
        };
        if (pid) close();
        else void pidReady.then(close);
      },
    });

    return {
      get pid() {
        return pid;
      },
      stdin,
      stdout,
      stderr,
      get exitCode() {
        return exitCode;
      },
      exited,
      kill: () => {
        void this.killProcess(boxId, pid);
      },
    };
  }
}
