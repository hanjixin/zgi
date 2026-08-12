// Codex adapter — drives the real Codex CLI through the official
// @openai/codex-sdk (Codex.startThread().runStreamed()). Streams normalized events.
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';

import { Codex } from '@openai/codex-sdk';

import type { AdapterDeps, McpServerConfig, RunRequest } from '../protocol.js';

export interface AdapterResult {
  sessionId: string | null;
}

/**
 * Run one Codex turn.
 *
 * Codex has no direct systemPrompt SDK option, so it is passed as a
 * config.toml override (instructions); the workspace AGENTS.md seed (Go side)
 * carries long-lived instructions independently.
 *
 * MCP servers: the Codex CLI loads them from `$CODEX_HOME/config.toml`, and
 * the SDK's `--config key=value` flattening cannot express nested mcp_servers.
 * So a session-scoped `CODEX_HOME` is prepared that copies the user config and
 * appends the `[mcp_servers.*]` sections (see prepareCodexHome).
 */
export async function runCodex(req: RunRequest, deps: AdapterDeps): Promise<AdapterResult> {
  const { emit, emitSession, abortController } = deps;

  type ConfigValue = string | number | boolean | ConfigValue[] | { [key: string]: ConfigValue };
  const config: { [key: string]: ConfigValue } = {};
  if (req.systemPrompt) config.instructions = [req.systemPrompt];

  const codexHome = await prepareCodexHome(req);
  const env: Record<string, string> = { ...(process.env as Record<string, string>), ...req.env };
  if (codexHome) env.CODEX_HOME = codexHome;

  const codex = new Codex({
    apiKey: req.env.OPENAI_API_KEY || req.env.CODEX_API_KEY || process.env.OPENAI_API_KEY,
    env,
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

/**
 * Prepare a session-scoped Codex config home that carries the user's config
 * plus the requested mcp_servers. Codex reads `$CODEX_HOME/config.toml`; the
 * SDK's `-c key=value` overrides cannot express nested MCP servers, so we merge
 * a real config.toml instead.
 *
 * Returns the config home dir, or null when no MCP servers are requested (the
 * user's real CODEX_HOME is left untouched).
 */
async function prepareCodexHome(req: RunRequest): Promise<string | null> {
  if (!req.mcpServers?.length) return null;

  const userHome = process.env.CODEX_HOME || path.join(os.homedir(), '.codex');
  const sessionHome = path.join(req.cwd, '.codex');
  await fs.mkdir(sessionHome, { recursive: true });

  // Carry the user's existing config forward (model, provider, auth) so a
  // session-scoped HOME does not silently change the agent's defaults.
  const userConfigPath = path.join(userHome, 'config.toml');
  const sessionConfigPath = path.join(sessionHome, 'config.toml');
  try {
    await fs.copyFile(userConfigPath, sessionConfigPath);
  } catch {
    // no user config file — start fresh
  }

  const mcpToml = serializeMcpServers(req.mcpServers);
  const existing = await fs.readFile(sessionConfigPath, 'utf8').catch(() => '');
  await fs.writeFile(sessionConfigPath, existing.trimEnd() + (existing.trim() ? '\n\n' : '') + mcpToml + '\n');
  return sessionHome;
}

/** Serialize MCP servers as TOML `[mcp_servers.*]` sections. */
function serializeMcpServers(servers: McpServerConfig[]): string {
  const lines: string[] = [];
  for (const s of servers) {
    lines.push(`[mcp_servers.${tomlKey(s.name)}]`);
    lines.push(`type = "${codexMcpType(s.type)}"`);
    lines.push('enabled = true');
    if (s.url) lines.push(`url = ${tomlString(s.url)}`);
    if (s.command) lines.push(`command = ${tomlString(s.command)}`);
    if (s.args?.length) lines.push(`args = [${s.args.map(tomlString).join(', ')}]`);
    if (s.headers && Object.keys(s.headers).length) lines.push(`headers = ${tomlInlineTable(s.headers)}`);
    if (s.env && Object.keys(s.env).length) lines.push(`env = ${tomlInlineTable(s.env)}`);
    lines.push('');
  }
  return lines.join('\n');
}

function codexMcpType(type: string): string {
  switch (type) {
    case 'sse':
      return 'sse';
    case 'stdio':
      return 'stdio';
    default:
      return 'streamable_http';
  }
}

function tomlKey(name: string): string {
  return name.replace(/[^A-Za-z0-9_-]/g, '_');
}

function tomlString(value: string): string {
  return JSON.stringify(value);
}

function tomlInlineTable(map: Record<string, string>): string {
  const entries = Object.entries(map).map(([k, v]) => `${tomlKey(k)} = ${tomlString(v)}`);
  return `{ ${entries.join(', ')} }`;
}
