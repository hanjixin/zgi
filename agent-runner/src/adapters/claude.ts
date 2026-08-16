// Claude Code adapter — drives the real Claude Code CLI through the official
// @anthropic-ai/claude-agent-sdk (query()). Streams normalized events.
import { randomUUID } from 'node:crypto';
import { EventEmitter } from 'node:events';
import type { Readable, Writable } from 'node:stream';

import { query } from '@anthropic-ai/claude-agent-sdk';

import type { AdapterDeps, McpServerConfig, PermissionDecision, RunRequest } from '../protocol.js';
import type { ProcessHandle } from '../sandboxClient.js';

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

  // When the run is sandboxed and a box manager is wired in, spawn the CLI
  // inside the session's box and hand the boxed handle to the SDK. The box
  // workspace is the cwd, so the SDK's own cwd is cleared.
  const boxManager = deps.boxManager;
  let boxed: ProcessHandle | null = null;
  if (req.sandbox && boxManager) {
    const box = await boxManager.ensure(req.sessionId, req);
    boxed = await boxManager.openProcess(box, {
      command: 'claude',
      args: [],
      env: req.env,
    });
  }

  const options: Record<string, unknown> = {
    cwd: boxed ? '' : req.cwd,
    allowedTools: req.allowedTools,
    disallowedTools: req.disallowedTools,
    permissionMode: req.permissionMode || 'acceptEdits',
    canUseTool,
    abortController,
    env: req.env,
  };
  if (boxed) {
    options.spawnClaudeCodeProcess = () => toSpawnedProcess(boxed);
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
