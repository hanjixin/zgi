// Claude Code adapter — drives the real Claude Code CLI through the official
// @anthropic-ai/claude-agent-sdk (query()). Streams normalized events.
import { randomUUID } from 'node:crypto';
import { EventEmitter } from 'node:events';
import { PassThrough, Writable } from 'node:stream';
import type { Readable } from 'node:stream';

import { query } from '@anthropic-ai/claude-agent-sdk';

import type { AdapterDeps, McpServerConfig, PermissionDecision, RunRequest } from '../protocol.js';
import type { Box, ProcessHandle } from '../sandboxClient.js';

export interface AdapterResult {
  sessionId: string | null;
}

export interface SpawnedProcess {
  stdin: Writable;
  stdout: Readable;
  readonly killed: boolean;
  readonly exitCode: number | null;
  readonly signalCode?: NodeJS.Signals | null;
  kill(signal: NodeJS.Signals): boolean;
  on(event: 'exit' | 'error', listener: (...args: never[]) => void): void;
  once(event: 'exit' | 'error', listener: (...args: never[]) => void): void;
  off(event: 'exit' | 'error', listener: (...args: never[]) => void): void;
}

/**
 * Bridge a boxed process handle to the SpawnedProcess shape the
 * claude-agent-sdk expects for its `spawnClaudeCodeProcess` option. The handle
 * already owns the CLI's stdio; exit/error events are forwarded from
 * `handle.exited` to the SDK through the backing EventEmitter.
 */
export function toSpawnedProcess(handle: ProcessHandle): SpawnedProcess {
  const ee = new EventEmitter();
  let killed = false;
  const spawned: Omit<SpawnedProcess, 'exitCode'> & {
    exitCode: number | null;
    emit(event: 'exit' | 'error', ...args: unknown[]): boolean;
  } = {
    stdin: handle.stdin,
    stdout: handle.stdout,
    get killed() {
      return killed;
    },
    exitCode: handle.exitCode,
    signalCode: null,
    kill(signal: NodeJS.Signals): boolean {
      killed = true;
      handle.kill();
      return true;
    },
    on(event, listener) {
      ee.on(event, listener as (...args: any[]) => void);
      return spawned;
    },
    once(event, listener) {
      ee.once(event, listener as (...args: any[]) => void);
      return spawned;
    },
    off(event, listener) {
      ee.off(event, listener as (...args: any[]) => void);
      return spawned;
    },
    emit(event, ...args) {
      return ee.emit(event, ...args);
    },
  };
  handle.exited.then((code) => {
    spawned.exitCode = code;
    ee.emit('exit', code, null);
  }).catch((err) => {
    ee.emit('error', err as Error);
  });
  return spawned;
}

/**
 * Lazily wrap an in-flight `openProcess` promise as a `SpawnedProcess` for the
 * claude-agent-sdk's `spawnClaudeCodeProcess` option. The SDK calls the spawn
 * hook synchronously and expects a usable `stdin`/`stdout` immediately, but the
 * boxed process handle only resolves once the sandbox has started the CLI and
 * streamed its `started` frame. So stdin writes made before resolution are
 * queued and replayed in order; stdout is an eagerly-created PassThrough that
 * the handle's stdout is piped into once ready. Exit/error are forwarded from
 * `handle.exited` (or from a rejected handle promise) through the backing
 * EventEmitter, and stderr is drained to `process.stderr` so its buffer cannot
 * grow unbounded.
 */
export function toLazySpawnedProcess(
  handlePromise: Promise<ProcessHandle>,
  signal?: AbortSignal,
): SpawnedProcess {
  const ee = new EventEmitter();
  let handle: ProcessHandle | null = null;
  let killed = false;
  let killQueued = false;
  let exitCode: number | null = null;

  // stdout is created eagerly so the SDK can start reading before the boxed
  // process is ready; the handle's stdout is piped in once it resolves.
  const stdout = new PassThrough();

  interface QueuedWrite {
    chunk: Buffer;
    cb: (err?: Error | null) => void;
  }
  const queuedWrites: QueuedWrite[] = [];
  let queuedEnd: ((err?: Error | null) => void) | null = null;

  const stdin = new Writable({
    write(chunk, _encoding, cb) {
      const buf = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
      if (handle) {
        handle.stdin.write(buf, (err) => cb(err ?? null));
        return;
      }
      queuedWrites.push({ chunk: buf, cb });
    },
    final(cb) {
      if (handle) {
        handle.stdin.end(() => cb());
        return;
      }
      queuedEnd = cb;
    },
    destroy(err, cb) {
      if (handle) handle.stdin.destroy(err ?? undefined);
      cb(err ?? null);
    },
  });

  const flush = (h: ProcessHandle): void => {
    for (const w of queuedWrites.splice(0)) {
      h.stdin.write(w.chunk, (err) => w.cb(err ?? null));
    }
    if (queuedEnd) {
      const endCb = queuedEnd;
      queuedEnd = null;
      h.stdin.end(() => endCb());
    }
  };

  const failPending = (err: Error): void => {
    for (const w of queuedWrites.splice(0)) w.cb(err);
    if (queuedEnd) {
      const endCb = queuedEnd;
      queuedEnd = null;
      endCb(err);
    }
  };

  const spawned: Omit<SpawnedProcess, 'exitCode'> & {
    exitCode: number | null;
  } = {
    stdin,
    stdout,
    get killed() {
      return killed;
    },
    get exitCode() {
      return exitCode;
    },
    signalCode: null,
    kill(_signal: NodeJS.Signals): boolean {
      killed = true;
      if (handle) {
        handle.kill();
      } else {
        killQueued = true;
      }
      return true;
    },
    on(event, listener) {
      ee.on(event, listener as (...args: any[]) => void);
      return spawned;
    },
    once(event, listener) {
      ee.once(event, listener as (...args: any[]) => void);
      return spawned;
    },
    off(event, listener) {
      ee.off(event, listener as (...args: any[]) => void);
      return spawned;
    },
  };

  handlePromise.then(
    (h) => {
      handle = h;
      // Drain stderr to the host log so the boxed CLI's stderr stays observable
      // and its Readable buffer cannot grow without bound over a long session.
      h.stderr.on('error', () => {});
      h.stderr.pipe(process.stderr, { end: false });
      // Forward stdout into the eagerly-created PassThrough the SDK reads from.
      h.stdout.on('error', (err) => stdout.destroy(err as Error));
      h.stdout.pipe(stdout);

      flush(h);
      if (killQueued) h.kill();

      h.exited.then(
        (code) => {
          exitCode = code;
          ee.emit('exit', code, null);
        },
        (err) => {
          ee.emit('error', err as Error);
        },
      );
    },
    (err) => {
      failPending(err as Error);
      ee.emit('error', err as Error);
    },
  );

  if (signal) {
    if (signal.aborted) {
      void spawned.kill('SIGTERM');
    } else {
      signal.addEventListener('abort', () => {
        void spawned.kill('SIGTERM');
      }, { once: true });
    }
  }

  return spawned;
}

/**
 * Run one Claude Code turn.
 */
export async function runClaude(req: RunRequest, deps: AdapterDeps): Promise<AdapterResult> {
  const { emit, emitSession, abortController, pending } = deps;
  const allowed = new Set(req.allowedTools);
  const disallowed = new Set(req.disallowedTools);
  let sessionId: string | null = req.sessionId || null;

  const canUseTool = async (toolName: string, input: Record<string, unknown>) => {
    if (allowed.has(toolName)) return { behavior: 'allow' as const };
    if (disallowed.has(toolName)) return { behavior: 'deny' as const };
    if (req.permissionMode === 'bypassPermissions') return { behavior: 'allow' as const };

    // Ask for approval. The SDK turn is suspended while this promise is pending.
    const correlationId = randomUUID();
    emit('permission_request', {
      correlation_id: correlationId,
      tool: toolName,
      input,
      reason: `tool ${toolName} requires approval`,
    });
    const decision = await new Promise<PermissionDecision | null>((resolve) => {
      const timer = setTimeout(() => resolve(null), req.askTimeoutMs || 300_000);
      const onAbort = (): void => {
        clearTimeout(timer);
        resolve(null);
      };
      abortController.signal.addEventListener('abort', onAbort, { once: true });
      pending.set(correlationId, (value) => {
        clearTimeout(timer);
        abortController.signal.removeEventListener('abort', onAbort);
        resolve(value);
      });
    });
    pending.delete(correlationId);
    if (decision?.decision === 'approve') return { behavior: 'allow' as const };
    return { behavior: 'deny' as const, updatedPermissions: [{ tool: toolName, mode: 'dontAsk' }] };
  };

  // When the run is sandboxed and a box manager is wired in, ensure the box
  // exists up front so the CLI has a workspace to run in. The actual process
  // spawn is deferred to `spawnClaudeCodeProcess`, which the SDK invokes with
  // the real protocol flags (command/args/env) the CLI needs to speak the SDK.
  const boxManager = deps.boxManager;
  let box: Box | null = null;
  if (req.sandbox && boxManager) {
    box = await boxManager.ensure(req.sessionId, req);
  }

  const options: Record<string, unknown> = {
    cwd: req.cwd,
    allowedTools: req.allowedTools,
    disallowedTools: req.disallowedTools,
    permissionMode: req.permissionMode || 'acceptEdits',
    canUseTool,
    abortController,
    env: req.env,
  };
  if (box && boxManager) {
    options.spawnClaudeCodeProcess = (spawnOpts: {
      command: string;
      args: string[];
      env?: Record<string, string>;
    }) =>
      toLazySpawnedProcess(
        boxManager.openProcess(box, {
          command: spawnOpts.command,
          args: spawnOpts.args,
          env: { ...req.env, ...(spawnOpts.env ?? {}) },
        }),
      );
  }
  if (req.model) options.model = req.model;
  if (req.resume) options.resume = req.resume;
  if (req.systemPrompt) options.systemPrompt = req.systemPrompt;
  if (req.mcpServers?.length) {
    options.mcpServers = Object.fromEntries(req.mcpServers.map((s) => [s.name, toClaudeMcp(s)]));
  }

  const q = query({ prompt: req.prompt, options: options as never });

  try {
    for await (const msg of q) {
      switch (msg.type) {
        case 'system':
          sessionId = msg.session_id || sessionId;
          emitSession(sessionId);
          break;
        case 'assistant':
          for (const block of (msg.message?.content ?? []) as Array<{ type?: string; text?: string; id?: string; name?: string; input?: unknown }>) {
            if (block.type === 'text' && typeof block.text === 'string') {
              emit('text', { text: block.text });
            } else if (block.type === 'tool_use') {
              emit('tool_use', { id: block.id, tool: block.name, input: block.input });
            }
          }
          break;
        case 'user':
          for (const block of (msg.message?.content ?? []) as Array<{ type?: string; tool_use_id?: string; toolName?: string; content?: unknown; is_error?: boolean }>) {
            if (block.type === 'tool_result') {
              const content = block.content;
              const output = typeof content === 'string' ? content : JSON.stringify(content ?? '');
              emit('tool_result', {
                id: block.tool_use_id,
                tool: block.toolName || '',
                output,
                is_error: Boolean(block.is_error),
              });
            }
          }
          break;
        case 'result':
          emit('done', {
            subtype: (msg as { subtype?: string; is_error?: boolean }).subtype === 'error' || (msg as { is_error?: boolean }).is_error ? 'error' : 'success',
            usage: (msg as { modelUsage?: unknown; usage?: unknown }).modelUsage ?? (msg as { usage?: unknown }).usage ?? null,
            cost: (msg as { total_cost_usd?: number }).total_cost_usd,
          });
          break;
        default:
          break;
      }
    }
  } catch (err) {
    if (abortController.signal.aborted) {
      emit('done', { subtype: 'cancelled' });
    } else {
      emit('error', { message: err instanceof Error ? err.message : String(err) });
    }
  }

  // Resolve any still-pending approval requests as denied so callers don't hang.
  for (const [, resolve] of pending) resolve(null);
  pending.clear();

  return { sessionId };
}

/** Map the runner's MCP config to the claude-agent-sdk mcpServers shape. */
function toClaudeMcp(s: McpServerConfig): Record<string, unknown> {
  switch (s.type) {
    case 'http':
      return { type: 'http', url: s.url, headers: s.headers, alwaysLoad: true };
    case 'sse':
      return { type: 'sse', url: s.url, headers: s.headers, alwaysLoad: true };
    default:
      return { type: 'stdio', command: s.command, args: s.args, env: s.env };
  }
}
