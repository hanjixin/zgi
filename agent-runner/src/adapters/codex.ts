// Codex adapter — drives the real Codex CLI through the official
// @openai/codex-sdk (Codex.startThread().runStreamed()). Streams normalized events.
import { Codex } from '@openai/codex-sdk';

import type { AdapterDeps, RunRequest } from '../protocol.js';

export interface AdapterResult {
  sessionId: string | null;
}

/**
 * Run one Codex turn.
 */
export async function runCodex(req: RunRequest, deps: AdapterDeps): Promise<AdapterResult> {
  const { emit, emitSession, abortController } = deps;

  // Codex has no direct systemPrompt SDK option, so it is passed as a
  // config.toml override (instructions). The workspace AGENTS.md seed (Go side)
  // carries long-lived instructions independently.
  //
  // NOTE: mcp_servers are intentionally NOT passed to Codex via config
  // overrides: the SDK flattens nested objects to `--config a.b.c=...` dotted
  // paths, which the Codex config parser rejects for streamable_http servers
  // ("env is not supported for streamable_http"). Codex MCP integration should
  // write a real config.toml into the workspace instead (follow-up).
  type ConfigValue = string | number | boolean | ConfigValue[] | { [key: string]: ConfigValue };
  const config: { [key: string]: ConfigValue } = {};
  if (req.systemPrompt) config.instructions = [req.systemPrompt];
  if (req.mcpServers?.length) {
    process.stderr.write(`[agent-runner] codex: skipping ${req.mcpServers.length} mcp_servers (config-override unsupported by codex)\n`);
  }

  const codex = new Codex({
    apiKey: req.env.OPENAI_API_KEY || req.env.CODEX_API_KEY || process.env.OPENAI_API_KEY,
    env: req.env,
    config,
  });
  const thread = codex.startThread({
    model: req.model,
    workingDirectory: req.cwd,
    sandboxMode: (req.sandboxMode || 'workspace-write') as 'read-only' | 'workspace-write' | 'danger-full-access',
    approvalPolicy: (req.approvalPolicy || 'never') as 'never' | 'on-request' | 'on-failure' | 'untrusted',
    skipGitRepoCheck: true,
  });

  let sessionId: string | null = req.sessionId || null;
  const lastText = new Map<string, string>(); // item id -> last emitted text (dedup)

  try {
    const { events } = await thread.runStreamed(req.prompt, { signal: abortController.signal });
    for await (const evt of events) {
      switch (evt.type) {
        case 'thread.started':
          sessionId = evt.thread_id;
          emitSession(sessionId);
          break;
        case 'item.started':
        case 'item.updated':
        case 'item.completed': {
          const item = evt.item as { type: string; id?: string; text?: string; command?: string; aggregated_output?: string; exit_code?: number; status?: string; changes?: unknown; server?: string; tool?: string; arguments?: unknown; result?: { content?: unknown }; error?: { message?: string } };
          switch (item.type) {
            case 'agent_message':
              if (typeof item.text === 'string' && lastText.get(item.id ?? '') !== item.text) {
                lastText.set(item.id ?? '', item.text);
                emit('text', { text: item.text });
              }
              break;
            case 'command_execution':
              emit('command_exec', {
                id: item.id,
                command: item.command,
                output: item.aggregated_output,
                exit_code: item.exit_code,
                status: item.status,
              });
              break;
            case 'file_change':
              emit('file_change', { id: item.id, changes: item.changes, status: item.status });
              break;
            case 'mcp_tool_call': {
              const tool = `${item.server ?? ''}/${item.tool ?? ''}`;
              if (item.status === 'in_progress') {
                emit('tool_use', { id: item.id, tool, input: item.arguments });
              } else {
                const output = item.result?.content ? JSON.stringify(item.result.content) : item.error?.message || '';
                emit('tool_result', { id: item.id, tool, output, is_error: Boolean(item.error) });
              }
              break;
            }
            default:
              break;
          }
          break;
        }
        case 'turn.completed':
          emit('done', { subtype: 'success', usage: evt.usage });
          break;
        case 'turn.failed':
          emit('done', { subtype: 'error', message: (evt as { error?: { message?: string } }).error?.message || 'turn failed' });
          break;
        case 'error':
          emit('error', { message: (evt as { message?: string }).message });
          break;
        default:
          break;
      }
    }
    return { sessionId };
  } catch (err) {
    if (abortController.signal.aborted) {
      emit('done', { subtype: 'cancelled' });
    } else {
      emit('error', { message: err instanceof Error ? err.message : String(err) });
    }
    return { sessionId };
  }
}
