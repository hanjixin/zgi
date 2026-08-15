// Codex adapter — drives the real Codex CLI through the official
// @openai/codex-sdk (Codex.startThread().runStreamed()). Streams normalized events.
import { Codex } from '@openai/codex-sdk';

import type { AdapterDeps, McpServerConfig, RunRequest } from '../protocol.js';

export interface AdapterResult {
  sessionId: string | null;
}

type ConfigValue = string | number | boolean | ConfigValue[] | { [key: string]: ConfigValue };

/**
 * Run one Codex turn.
 *
 * Codex has no direct systemPrompt SDK option, so it is passed as a
 * config.toml override (instructions); the workspace AGENTS.md seed (Go side)
 * carries long-lived instructions independently.
 *
 * MCP servers ride the SDK's `--config key=value` overrides. The SDK flattens
 * a nested config object into dotted paths (`mcp_servers.<name>.url="…"`),
 * which the Codex CLI accepts for mcp_servers — map fields (headers / env)
 * MUST be emitted one dotted key at a time, never as an inline table (Codex's
 * parser reads an inline-table RHS as a string and aborts at startup). See
 * buildMcpServersConfig.
 */
export async function runCodex(req: RunRequest, deps: AdapterDeps): Promise<AdapterResult> {
  const { emit, emitSession, abortController } = deps;

  const config: { [key: string]: ConfigValue } = {};
  // Codex 0.147's `instructions` config is a plain string (a sequence is
  // rejected with "invalid type: sequence, expected a string"). The SDK
  // serializes a string value as `instructions="…"`, which the CLI parses fine
  // even for multi-line prompts.
  if (req.systemPrompt) config.instructions = req.systemPrompt;

  const mcpServers = buildMcpServersConfig(req.mcpServers);
  if (Object.keys(mcpServers).length) config.mcp_servers = mcpServers;

  // Route model calls through the ZGI LLM gateway (OpenAI Responses API). The
  // org API key rides the OPENAI_API_KEY env var injected by the Go driver.
  if (req.gatewayUrl) {
    config.model_providers = buildGatewayProviderConfig(req.gatewayUrl);
    config.model_provider = 'zgi';
  }

  const codex = new Codex({
    apiKey: req.env.OPENAI_API_KEY || req.env.CODEX_API_KEY || process.env.OPENAI_API_KEY,
    env: req.env,
    config,
  });
  const thread = codex.startThread({
    model: req.model,
    workingDirectory: req.cwd,
    // danger-full-access: Codex 0.125+ auto-cancels MCP tool calls under
    // managed sandbox profiles (read-only / workspace-write), so the fallback
    // mirrors the Go driver's unsandboxed setting.
    sandboxMode: (req.sandboxMode || 'danger-full-access') as 'read-only' | 'workspace-write' | 'danger-full-access',
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

/**
 * Serialize the requested MCP servers into Codex's config surface.
 *
 * The SDK flattens this nested object into `--config` dotted paths. Two rules
 * the Codex CLI enforces (see test_codex_mcp_overrides.py in the valuz-oss
 * reference for the wire-level verification):
 *
 * - Map fields (`headers` → `http_headers`, `env`) are emitted ONE dotted key
 *   at a time, never as an inline table `{ … }` — Codex's `-c` parser reads an
 *   inline table as a *string* and aborts at startup ("invalid type: string …
 *   expected a map").
 * - `env` is only valid on stdio servers. Codex rejects `env` on
 *   streamable_http servers, so remote servers carry headers via
 *   `http_headers.<KEY>` and drop `env`.
 */
/**
 * Build the Codex model provider config that routes model calls through the
 * ZGI LLM gateway (OpenAI Responses API at `<gateway>/v1`). The org API key is
 * read from the OPENAI_API_KEY env var injected by the Go driver.
 */
export function buildGatewayProviderConfig(gatewayUrl: string): Record<string, Record<string, ConfigValue>> {
  return {
    zgi: {
      name: 'ZGI LLM Gateway',
      base_url: `${gatewayUrl.replace(/\/+$/, '')}/v1`,
      wire_api: 'responses',
      env_key: 'OPENAI_API_KEY',
    },
  };
}

export function buildMcpServersConfig(servers?: McpServerConfig[]): Record<string, Record<string, ConfigValue>> {
  const out: Record<string, Record<string, ConfigValue>> = {};
  for (const s of servers ?? []) {
    const cfg: Record<string, ConfigValue> = {};
    if (s.type === 'stdio' && s.command) {
      cfg.command = s.command;
      if (s.args?.length) cfg.args = [...s.args];
      if (s.env && Object.keys(s.env).length) cfg.env = { ...s.env };
    } else if (s.url) {
      cfg.url = s.url;
      if (s.headers && Object.keys(s.headers).length) cfg.http_headers = { ...s.headers };
    }
    // Skip servers with no usable fields — an empty config object would
    // serialize to `mcp_servers.X={}` and pollute the config.
    if (Object.keys(cfg).length) out[s.name] = cfg;
  }
  return out;
}
