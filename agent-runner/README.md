# ZGI agent-runner

Embeds the **real** Agent CLIs — [Claude Code](https://github.com/anthropics/claude-code) and
[OpenAI Codex](https://github.com/openai/codex) — through their official SDKs and streams
normalized events to the ZGI control plane over SSE.

```
Go API (Agent Runtime Kernel)
   │  POST /v1/agents/run  (SSE)        │  POST /v1/agents/:sid/stop
   ▼                                    ▼  POST /v1/agents/:sid/permission
agent-runner (Node/TS, this service)
   ├─ claude adapter   @anthropic-ai/claude-agent-sdk  (query())
   ├─ codex adapter    @openai/codex-sdk               (Codex.startThread)
   └─ workspace dir = agent workspace (cwd)
```

This replaces the previous Go-native "fake loop" — the Agent CLIs are now the real execution
engine with their real tools (file editing, git, bash, search, web).

## Requirements

- Node.js >= 20
- The real CLI binaries on the runner host:
  - `claude` (Claude Code) — required for `agent_type=claude`
  - `codex` (Codex CLI) — required for `agent_type=codex`
- API keys via env: `ANTHROPIC_API_KEY` (claude) / `OPENAI_API_KEY` (codex)

## Run

```bash
npm install
npm run dev        # tsx watch, port 3001
AGENT_RUNNER_PORT=3101 npm start
AGENT_RUNNER_STUB=1 npm start   # deterministic stub adapter for demos/tests
```

## HTTP API

| Endpoint | Body | Returns |
|---|---|---|
| `POST /v1/agents/run` | `{ agent_type, session_id, prompt, cwd, env, model, system_prompt, allowed_tools, disallowed_tools, permission_mode, approval_policy, sandbox_mode, resume, ask_timeout_ms, mcp_servers }` | SSE stream of normalized events |
| `POST /v1/agents/:session_id/stop` | — | abort the run (done `cancelled`) |
| `POST /v1/agents/:session_id/permission` | `{ correlation_id, decision: approve\|reject, reason }` | resolve a pending approval |

### Normalized event protocol (SSE `data:` lines)

```
{type:"session_started", session_id, agent_session_id}
{type:"text", text}
{type:"tool_use", id, tool, input}
{type:"tool_result", id, tool, output, is_error}
{type:"command_exec", command, output, exit_code, status}
{type:"file_change", changes, status}
{type:"permission_request", correlation_id, tool, input, reason}
{type:"permission_result", tool, denied, reason}
{type:"done", subtype: "success"|"error"|"cancelled", usage, cost}
{type:"error", message}
```

## Model / memory / tools / MCP

- **Model**: `model` field → SDK model option.
- **System prompt**: `system_prompt` → Claude `options.systemPrompt`; Codex via config `instructions`.
- **Memory**: the workspace dir is seeded with `CLAUDE.md` (claude) / `AGENTS.md` (codex) by the Go
  driver before each run; both CLIs auto-load these.
- **Tools**: CLI built-ins gated by `allowed_tools` / `disallowed_tools` + `permission_mode`.
- **MCP**: `mcp_servers` array (`{name, type: stdio|http|sse, url|command, args, headers, env}`)
  → Claude `options.mcpServers`; Codex via config `mcp_servers`.

## Test

```bash
npm test
```

## Go control plane

The Go side lives in `api/internal/capabilities/agentruntime/cli/` (driver, runner HTTP client,
event mapping). Routing: `runtime_type=codex` → Codex, `runtime_type=claude-code` → Claude Code.
