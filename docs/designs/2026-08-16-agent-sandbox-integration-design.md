# Agent Sandbox Integration — Design Spec

**Goal:** Run the Claude Code / Codex processes driven by the ZGI agent kernel *inside* the
`zgi-sandbox` service (bwrap-isolated agent boxes), so agent shell/file operations execute in an
isolated, audited, per-agent environment instead of on the host.

**Date:** 2026-08-16

## Background & Constraints (verified against the code)

- ZGI agent execution today: `api` agentruntime kernel (`cli/driver.go`) → `agent-runner`
  (`src/adapters/claude.ts`, `codex.ts`) → real Agent CLI binary, cwd = `$TMPDIR/zgi-agents/<agent_id>`,
  running **without** OS-level isolation (codex `sandboxMode=danger-full-access`, claude bypassPermissions).
- `zgi-sandbox` is a Go HTTP service with `processBackend` (no isolation, default) and
  `linuxSecureBackend` (bwrap + prlimit + prebuilt rootfs). Its exec API (`Run`, `ExecuteCommand`)
  is **one-shot request/response with hard timeouts** — it cannot host a long-lived CLI process today.
- `claude-agent-sdk` exposes `spawnClaudeCodeProcess?: (opts) => SpawnedProcess` — an official
  extension point documented for "VMs, containers, or remote environments".
- `codex-sdk` exposes `codexPathOverride?: string` — point the SDK at a bridge executable.
- `PreToolUse` hooks **cannot** replace a tool's execution result, so "route only Bash to a remote
  service" is not viable for Claude — the whole CLI process must run inside the sandbox.

## Decisions

1. **Boundary:** whole CLI process inside the sandbox (files + commands + process tree isolated).
2. **Landing layer:** extend `zgi-sandbox` with a persistent, long-lived **agent box** capability.
3. **SDK location:** stays in `agent-runner` (Node). Isolation is about where the CLI process runs,
   not where the SDK runs. Reuses all existing adapter logic (session/resume, permission hooks,
   event normalization, MCP).
4. **Platform:** `linux-secure` (bwrap) on Linux production; `processBackend` fallback on macOS dev
   (no isolation, logging a warning). No macOS-specific isolation work.

## Architecture

```
Go API (agentruntime kernel)
   │  POST /v1/agents/run (SSE)     — driver stops building a local workspace in sandbox mode
   ▼
agent-runner (Node, host)           — owns box registry: Map<sessionId, BoxHandle>
   ├─ claude.ts : options.spawnClaudeCodeProcess → boxed CLI bridge
   ├─ codex.ts  : codexPathOverride = src/codexBridge.mjs
   └─ sandboxClient.ts : HTTP + SSE to zgi-sandbox
        │  POST /v1/agent-boxes                (create box + seed memory file)
        │  SSE  /v1/agent-boxes/:id/process    (stdout/stderr/exit down)
        │  POST /v1/agent-boxes/:id/process/:pid/stdin  (stdin up)
        ▼
zgi-sandbox (Go)
   ├─ agent-box endpoints (new) — lifecycle.Manager.Create reused, NetworkEnabled=true
   ├─ runner.backend.StartProcess (new) — boxed CLI process with streamed stdio
   └─ inside box: claude/codex + bash/file ops, workspace = box persistent dir
```

### Why SSE-down + POST-up for stdio (no WebSocket)

`go.mod` has only pgx + redis. CLI stdio is JSONL **text** — no binary frames needed. SSE down
(stdout/stderr/exit) + `POST …/stdin` up is sufficient and avoids adding a WebSocket dependency.

## zgi-sandbox changes (Go)

### New endpoints (`internal/agentbox` package or added to `internal/app`)

- `POST /v1/agent-boxes`
  - Body: `{ runtime_profile: "session", network_enabled: true, ttl_seconds, workspace_seed: { "CLAUDE.md"?: string, "AGENTS.md"?: string }, ownership: { organization_id, workspace_id, user_id, ... } }`
  - Reuses `lifecycle.Manager.Create` (applies `policy.NormalizeCreate` — TTL/org limits).
  - Writes the memory-file seeds into the box workspace dir via the existing executor file primitives.
  - Response: `{ box_id, workspace_path }` where `workspace_path` is the **in-box** cwd
    (`/tmp/workspace` for the secure backend, host `DataDir/workspaces/<id>` for the process backend).
- `GET /v1/agent-boxes/:id`, `DELETE /v1/agent-boxes/:id` — reuse `lifecycle.Get/Delete`; delete tears
  down any running process in the box.
- `POST /v1/agent-boxes/:id/process`
  - Body: `{ command, args, env, cwd }`; validates the box is active, applies command limits.
  - Opens an SSE stream (down: `stdout`, `stderr`, `exit {code}`, `error`) and registers a process
    handle keyed by a `pid`.
  - `POST /v1/agent-boxes/:id/process/:pid/stdin` — append stdin bytes (fail after process exit).
  - `DELETE /v1/agent-boxes/:id/process/:pid` — SIGKILL the process group.

### Runner backend: new `StartProcess`

- Extend `internal/runner/backend` interface with `StartProcess(ctx, ProcessSpec) (ProcessSession, error)`.
  `ProcessSpec` ≈ `{ WorkDir, Command, Args, Env, EnableNetwork, DependencyProfile }`; `ProcessSession`
  ≈ `{ Stdin io.Writer, Stdout io.Reader, Stderr io.Reader, Wait() error, Kill() error }`.
- `processBackend`: local `exec.Command` with piped stdio (macOS fallback).
- `linuxSecureBackend`: reuse `buildSecureBwrapArgs` (+ `prlimit` when configured) but pipe stdio
  instead of one-shot capture; `--unshare-net` only when `EnableNetwork=false`.
- **Separate concurrency pool** `MaxAgentProcesses` (new config) so long-lived agent processes don't
  starve short execs under the shared `MaxWorkers` semaphore; per-org limits via observer queries.
- **CLI binaries in the box:** v1 ro-binds the host `claude`/`codex` install read-only into the box
  (`--ro-bind <host-cli> /opt/zgi/agent-cli/…` + PATH) — avoids rebuilding the agent-box rootfs;
  hardening path (bake CLIs into rootfs) is deferred.
- Observer events: `agent.box.created/deleted`, `agent.process.started/exited`.

## agent-runner changes (Node)

- New `src/sandboxClient.ts`: create box, open SSE process, write stdin, delete box, TTL refresh.
- Box registry `Map<sessionId, BoxHandle>`: create on first turn, reuse on resume, delete on turn
  end/app shutdown; refresh box TTL between turns.
- `src/adapters/claude.ts`: pass `spawnClaudeCodeProcess` returning a `SpawnedProcess` backed by the
  boxed process stream (custom `Writable`/`Readable` over the SSE + stdin endpoints).
- `src/adapters/codex.ts`: set `codexPathOverride` to `src/codexBridge.mjs`, which forwards
  `exec --experimental-json …` argv into the boxed `codex` process and proxies stdio.
- Memory-file seeding moves here: seed `CLAUDE.md`/`AGENTS.md` from `systemPrompt` on box creation.

## Go API changes (`agentruntime/cli`)

- `Options` gains `SandboxURL` / `SandboxAPIKey`; `RunRequest` gains `sandbox: { url, api_key }`.
- In sandbox mode, `ensureWorkspaceDir` + `seedMemoryFile` are bypassed (cwd resolved by
  agent-runner to the box `workspace_path`); the non-sandbox path is preserved as fallback.
- `Stop` also requests box cleanup (or relies on TTL).

## Data flow (one turn)

1. Go driver assembles `RunRequest` (prompt/model/env/system_prompt/mcp/sandbox config) → POST `/v1/agents/run`.
2. agent-runner resolves the box for `session_id` (create + seed, or reuse).
3. Adapter spawns the CLI **inside the box** via the SDK extension point; SDK talks stdio over the bridge.
4. Boxed CLI reaches the LLM gateway + zgi-tools MCP bridge over shared network; bash/file ops run
   in the box workspace.
5. Events stream back over SSE → Go driver maps to ZGI events → persists `agent_session_id` for resume.
6. Turn ends: agent-runner keeps the box (TTL); Go driver persists resume id.

## Error handling

| Scenario | Behavior |
|---|---|
| Box creation fails (unreachable / quota) | `/v1/agents/run` returns a clear error prefixed `agent box: …` |
| Process stream drops mid-turn | abort turn, emit `done(error)`; box kept (TTL) for resume |
| CLI crashes inside box | `exit` event → `done(error)` with exit code |
| Box TTL expires between turns | next turn creates a fresh box (conversation lost); mitigated by session-TTL box + refresh between turns |
| macOS process backend | no isolation; WARN log at startup |
| Non-sandbox mode | unchanged code path; config switch |

## Testing

- **sandbox (Go):** `StartProcess` unit tests on `processBackend` (write stdin → read stdout → exit
  code); secure backend path on Linux CI; endpoint tests for `/v1/agent-boxes` + process channel.
- **agent-runner (TS):** `sandboxClient` unit tests; claude adapter with a stub spawn feeding fake
  CLI JSONL; `codexBridge.mjs` unit test; integration against a local zgi-sandbox (process backend)
  with a fake CLI to validate the full stdio path.
- **Go driver:** stub agent-runner to verify `RunRequest` carries sandbox config and event mapping
  is unchanged.

## Rollout

- New config switch, **default off**; non-sandbox path untouched.
- macOS (process backend) validates the full link first; then Linux enables `linux-secure`.
- Enabled per runtime type (claude / codex) independently.

## Deferred (v2)

- Per-host egress allowlists for boxed agents (v1: network on, all-or-nothing at the box level).
- Bake `claude`/`codex` into the agent-box rootfs (prod hardening) instead of ro-binding host installs.
- Persistent CLI process across turns (v1: per-turn restart + resume, preserving existing adapter model).
