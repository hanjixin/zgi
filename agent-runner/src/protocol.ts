// Normalized event protocol shared between the runner and the ZGI control plane.
// Every message is a single JSON object written as one SSE `data:` line.

export type AgentType = 'claude' | 'codex';

export interface RunRequest {
  agentType: AgentType;
  /** Control-plane session id; used as the run registry key and for stop/permission. */
  sessionId: string;
  prompt: string;
  cwd: string;
  env: Record<string, string>;
  model?: string;
  /** Session-level system prompt injected into the Agent CLI. */
  systemPrompt?: string;
  allowedTools: string[];
  disallowedTools: string[];
  permissionMode: string;
  approvalPolicy: string;
  sandboxMode: string;
  /** SDK session id to resume a previous conversation. */
  resume?: string;
  /** How long to wait for an interactive approval before auto-denying. */
  askTimeoutMs: number;
  /** MCP servers exposed to the Agent CLI. */
  mcpServers?: McpServerConfig[];
  /** ZGI LLM gateway base URL; when set, codex/claude route model calls through it. */
  gatewayUrl?: string;
  /** When set, the Agent CLI process runs inside a zgi-sandbox agent box. */
  sandbox?: SandboxConfig;
}

export interface SandboxConfig {
  url: string;
  api_key?: string;
}

/** A remote/stdio MCP server the Agent CLI may connect to. */
export interface McpServerConfig {
  name: string;
  type: 'stdio' | 'http' | 'sse';
  url?: string;
  command?: string;
  args?: string[];
  headers?: Record<string, string>;
  env?: Record<string, string>;
}

export interface NormalizedEvent {
  type: string;
  [key: string]: unknown;
}

export interface PermissionDecision {
  decision: 'approve' | 'reject';
  reason?: string;
}

export interface AdapterDeps {
  emit: (type: string, payload?: Record<string, unknown>) => void;
  emitSession: (agentSessionId: string | null) => void;
  abortController: AbortController;
  /** Map of pending approval correlation ids to their resolvers (owned by the app). */
  pending: Map<string, (value: PermissionDecision | null) => void>;
}

export function ev(type: string, payload: Record<string, unknown> = {}): NormalizedEvent {
  return { type, ...payload };
}

export function sse(event: NormalizedEvent): string {
  return `data: ${JSON.stringify(event)}\n\n`;
}

// Validate a `POST /v1/agents/run` body and return a normalized request object,
// or throw an Error describing what is invalid.
export function parseRunRequest(body: unknown): RunRequest {
  const raw = (body ?? {}) as Record<string, unknown>;
  const agentType = String(raw.agent_type || '');
  if (agentType !== 'claude' && agentType !== 'codex') {
    throw new Error(`unsupported agent_type "${agentType}" (expected claude|codex)`);
  }
  const prompt = String(raw.prompt ?? '');
  if (!prompt.trim()) {
    throw new Error('prompt is required');
  }
  return {
    agentType,
    sessionId: String(raw.session_id || ''),
    prompt,
    cwd: String(raw.cwd || process.cwd()),
    env: normalizeEnv(raw.env),
    model: raw.model ? String(raw.model) : undefined,
    systemPrompt: raw.system_prompt ? String(raw.system_prompt) : undefined,
    allowedTools: toStrArray(raw.allowed_tools),
    disallowedTools: toStrArray(raw.disallowed_tools),
    permissionMode: String(raw.permission_mode || ''),
    approvalPolicy: String(raw.approval_policy || ''),
    sandboxMode: String(raw.sandbox_mode || ''),
    resume: raw.resume ? String(raw.resume) : undefined,
    askTimeoutMs: Number(raw.ask_timeout_ms || 300_000),
    mcpServers: parseMcpServers(raw.mcp_servers),
    gatewayUrl: raw.gateway_url ? String(raw.gateway_url) : undefined,
    sandbox: parseSandbox(raw.sandbox),
  };
}

function parseSandbox(value: unknown): SandboxConfig | undefined {
  if (!value || typeof value !== 'object') return undefined;
  const raw = value as Record<string, unknown>;
  const url = String(raw.url || '');
  if (!url) return undefined;
  return { url, api_key: raw.api_key ? String(raw.api_key) : undefined };
}

function parseMcpServers(value: unknown): McpServerConfig[] | undefined {
  if (!Array.isArray(value)) return undefined;
  const out: McpServerConfig[] = [];
  for (const raw of value) {
    if (!raw || typeof raw !== 'object') continue;
    const s = raw as Record<string, unknown>;
    const name = String(s.name || '');
    if (!name) continue;
    const type = s.type === 'stdio' || s.type === 'sse' || s.type === 'http' ? s.type : 'http';
    out.push({
      name,
      type,
      url: s.url ? String(s.url) : undefined,
      command: s.command ? String(s.command) : undefined,
      args: Array.isArray(s.args) ? s.args.map(String) : undefined,
      headers: normalizeEnv(s.headers),
      env: normalizeEnv(s.env),
    });
  }
  return out.length ? out : undefined;
}

function normalizeEnv(env: unknown): Record<string, string> {
  if (!env || typeof env !== 'object') return {};
  const out: Record<string, string> = {};
  for (const [k, v] of Object.entries(env)) {
    if (typeof v === 'string' && k) out[k] = v;
  }
  return out;
}

function toStrArray(value: unknown): string[] {
  if (!Array.isArray(value)) return [];
  return value.filter((x): x is string => typeof x === 'string' && x.length > 0);
}
