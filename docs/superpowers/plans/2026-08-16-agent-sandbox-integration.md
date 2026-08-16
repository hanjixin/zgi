# Agent Sandbox Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Run the Claude Code / Codex processes driven by the agent kernel inside `zgi-sandbox` agent boxes (bwrap isolation), with the Node SDK staying in agent-runner and bridging CLI stdio over HTTP+SSE.

**Architecture:** zgi-sandbox gains persistent **agent boxes** (`POST /v1/agent-boxes`) and a **streamed process channel** (SSE down for stdout/stderr/exit + `POST …/stdin` up for stdin — no WebSocket dependency). agent-runner bridges the CLIs via `spawnClaudeCodeProcess` (claude) and `codexPathOverride` (codex). The Go driver passes `sandbox:{url,api_key}` and skips its local workspace in sandbox mode.

**Tech Stack:** Go 1.26 (zgi-sandbox), Node/TS ≥20 (agent-runner), Go (api). No new third-party dependencies anywhere.

**Spec:** `docs/designs/2026-08-16-agent-sandbox-integration-design.md`

## Global Constraints

- No new third-party dependencies in any of the three codebases.
- SSE frames are `data: <json>\n\n`; stdin goes over `POST` (not WebSocket).
- Agent boxes use `RuntimeProfile: "session"`; box TTL defaults to session TTL (1800s).
- `linux-secure` is Linux-only (`//go:build linux`); macOS dev uses `processBackend` (no isolation, WARN log).
- Sandbox mode is off by default; the non-sandbox path must keep working unchanged.
- Follow each repo's existing patterns: Go uses `writeEnvelope`/`writeKnownError`/`authorized`; agent-runner uses `tsx --test`; api uses `cli.Options` + `RunRequest`.
- Commit per task with the repo's emoji-prefixed conventional style (e.g. `✨ feat(sandbox): …`).

---

### Task 1: Streaming process API on the runner backend (process backend)

**Files:**
- Create: `sandbox/internal/runner/process.go`
- Modify: `sandbox/internal/runner/runner.go:158-162` (backend interface)
- Modify: `sandbox/internal/runner/backend_secure_linux.go` (placeholder `StartProcess` so the package compiles)
- Test: `sandbox/internal/runner/process_test.go`

**Interfaces:**
- Consumes: existing `processEnv(map,map) []string`, `syscall` process-group helpers.
- Produces:
  - `type ProcessSpec struct { WorkDir string; Command string; Args []string; Env map[string]string; EnableNetwork bool }`
  - `type ProcessSession struct { Stdin io.WriteCloser; Stdout io.Reader; Stderr io.Reader; Wait func() error; Kill func() error }`
  - `backend.StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error)` added to the `backend` interface.

- [ ] **Step 1: Add the interface method + placeholder secure impl**

In `sandbox/internal/runner/runner.go`, extend the interface:

```go
type backend interface {
	Name() string
	Run(context.Context, Request, string, bool, time.Duration, int, int) (Result, error)
	ExecuteCommand(context.Context, CommandSpec) (CommandResult, error)
	StartProcess(context.Context, ProcessSpec) (*ProcessSession, error)
}
```

In `sandbox/internal/runner/backend_secure_linux.go`, add a placeholder so the package compiles (replaced by the real impl in Task 3):

```go
func (b *linuxSecureBackend) StartProcess(context.Context, ProcessSpec) (*ProcessSession, error) {
	return nil, errors.New("linux-secure StartProcess not implemented yet")
}
```

- [ ] **Step 2: Write the failing tests**

Create `sandbox/internal/runner/process_test.go`:

```go
package runner

import (
	"context"
	"io"
	"testing"
)

func TestProcessBackendStartProcessStreamsStdio(t *testing.T) {
	backend := newProcessBackend()
	sess, err := backend.StartProcess(context.Background(), ProcessSpec{
		Command: "/bin/sh",
		Args:    []string{"-c", "cat; echo DONE"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer func() { _ = sess.Kill() }()

	if _, err := io.WriteString(sess.Stdin, "hello\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = sess.Stdin.Close()

	out, err := io.ReadAll(sess.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got, want := string(out), "hello\nDONE\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestProcessBackendStartProcessKillsProcessGroup(t *testing.T) {
	backend := newProcessBackend()
	sess, err := backend.StartProcess(context.Background(), ProcessSpec{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := sess.Wait(); err == nil {
		t.Fatal("expected non-nil error after Kill")
	}
}
```

- [ ] **Step 3: Run tests to verify they fail**

Run: `cd sandbox && go test ./internal/runner/ -run 'TestProcessBackendStartProcess' -v`
Expected: FAIL — `backend` interface has no method `StartProcess` (compile error) / `newProcessBackend()` type lacks it.

- [ ] **Step 4: Implement `process.go`**

Create `sandbox/internal/runner/process.go`:

```go
package runner

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// ProcessSpec describes a long-running process to start inside a runtime.
type ProcessSpec struct {
	WorkDir       string
	Command       string
	Args          []string
	Env           map[string]string
	EnableNetwork bool
}

// ProcessSession is a running process with streamed stdio. Wait blocks until
// the process exits and returns its error; Kill terminates the whole group.
type ProcessSession struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	Wait   func() error
	Kill   func() error
}

func (b *processBackend) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	cmd := exec.CommandContext(ctx, spec.Command, spec.Args...)
	cmd.Dir = spec.WorkDir
	cmd.Env = processEnv(spec.Env, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var once sync.Once
	var waitErr error
	pid := cmd.Process.Pid
	return &ProcessSession{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Wait: func() error {
			once.Do(func() {
				waitErr = cmd.Wait()
				_ = stdin.Close()
			})
			return waitErr
		},
		Kill: func() error {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		},
	}, nil
}

// StartProcess exposes the backend's streaming process API on the Service. It
// deliberately does NOT acquire the shared MaxWorkers semaphore: long-lived
// agent processes would starve short one-shot execs. The caller (app layer)
// enforces the separate agent-process pool.
func (s *Service) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	return s.backend.StartProcess(ctx, spec)
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd sandbox && go test ./internal/runner/ -run 'TestProcessBackendStartProcess' -v`
Expected: PASS (both tests).

- [ ] **Step 6: Commit**

```bash
git add sandbox/internal/runner/process.go sandbox/internal/runner/process_test.go sandbox/internal/runner/runner.go sandbox/internal/runner/backend_secure_linux.go
git commit -m "✨ feat(sandbox): add streamed StartProcess to the runner backend"
```

---

### Task 2: Agent-box config keys

**Files:**
- Modify: `sandbox/internal/config/config.go` (struct + defaults)
- Test: `sandbox/internal/config/config_test.go`

**Interfaces:**
- Consumes: existing `getEnv*` helpers, `Config` struct.
- Produces: three new `Config` fields consumed by Task 3 (`AgentCLIDir`), Task 4 (`AgentBoxTTLSeconds`), Task 5 (`MaxAgentProcesses`).

- [ ] **Step 1: Add fields to the Config struct**

In `sandbox/internal/config/config.go`, inside the `Config` struct add:

```go
	// Agent boxes (coding-agent runtime). AgentCLIDir is a host directory
	// (e.g. /usr/local/bin) containing the claude/codex binaries, ro-bound
	// into linux-secure agent boxes at /opt/zgi/agent-cli.
	AgentBoxTTLSeconds int
	MaxAgentProcesses  int
	AgentCLIDir        string
```

- [ ] **Step 2: Wire env parsing in `config.go`**

In the config loader (near the other `getEnv*` lines), add:

```go
		AgentBoxTTLSeconds:                   getEnvInt("ZGI_SANDBOX_AGENT_BOX_TTL_SECONDS", 1800),
		MaxAgentProcesses:                    getEnvIntAllowZero("ZGI_SANDBOX_MAX_AGENT_PROCESSES", 4),
		AgentCLIDir:                          getEnv("ZGI_SANDBOX_AGENT_CLI_DIR", ""),
```

- [ ] **Step 3: Write failing defaults test**

Add to `sandbox/internal/config/config_test.go`:

```go
func TestAgentBoxConfigDefaults(t *testing.T) {
	t.Setenv("ZGI_SANDBOX_AGENT_BOX_TTL_SECONDS", "")
	t.Setenv("ZGI_SANDBOX_MAX_AGENT_PROCESSES", "")
	t.Setenv("ZGI_SANDBOX_AGENT_CLI_DIR", "")
	cfg := config.FromEnv()
	if cfg.AgentBoxTTLSeconds != 1800 {
		t.Fatalf("AgentBoxTTLSeconds default = %d, want 1800", cfg.AgentBoxTTLSeconds)
	}
	if cfg.MaxAgentProcesses != 4 {
		t.Fatalf("MaxAgentProcesses default = %d, want 4", cfg.MaxAgentProcesses)
	}
	if cfg.AgentCLIDir != "" {
		t.Fatalf("AgentCLIDir default = %q, want empty", cfg.AgentCLIDir)
	}
}
```

- [ ] **Step 4: Run test to verify it fails**

Run: `cd sandbox && go test ./internal/config/ -run TestAgentBoxConfigDefaults -v`
Expected: FAIL — `cfg.AgentBoxTTLSeconds` undefined.

- [ ] **Step 5: Implement (steps 1-2 above), run test to verify it passes**

Run: `cd sandbox && go test ./internal/config/ -run TestAgentBoxConfigDefaults -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sandbox/internal/config/config.go sandbox/internal/config/config_test.go
git commit -m "✨ feat(sandbox): add agent-box config keys"
```

---

### Task 3: linux-secure streamed StartProcess

**Files:**
- Modify: `sandbox/internal/runner/backend_secure_linux.go` (real `StartProcess`)
- Modify: `sandbox/internal/runner/secure_bwrap.go` (`secureBwrapSpec.ExtraRoBinds` + `buildSecureBwrapArgs`)
- Modify: `sandbox/internal/runner/backend_config.go` (pass `AgentCLIDir` into the secure backend)
- Test: `sandbox/internal/runner/secure_bwrap_test.go`

**Interfaces:**
- Consumes: `config.Config.AgentCLIDir` (Task 2), `ProcessSpec`/`ProcessSession` (Task 1), `buildSecureBwrapArgs`, `secureBwrapSpec`.
- Produces: `linuxSecureBackend.StartProcess` — a bwrap-isolated process whose stdio is streamed, with the agent CLI dir ro-bound at `/opt/zgi/agent-cli` and PATH prefixed with it.

- [ ] **Step 1: Extend the bwrap spec for extra read-only binds**

In `sandbox/internal/runner/secure_bwrap.go`, add a field to `secureBwrapSpec` and append the binds in `buildSecureBwrapArgs` (after the profile ro-bind, before the network toggle):

```go
type secureBwrapSpec struct {
	RootFS              string
	WorkDir             string
	Binary              string
	Args                []string
	EnableNetwork       bool
	Env                 map[string]string
	ProfileEnv          map[string]string
	ProfileHostDir      string
	ProfileContainerDir string
	ExtraRoBinds        []string
}
```

```go
	for i := 0; i+1 < len(spec.ExtraRoBinds); i += 2 {
		args = append(args, "--ro-bind", spec.ExtraRoBinds[i], spec.ExtraRoBinds[i+1])
	}
```

- [ ] **Step 2: Write the failing tests**

Add to `sandbox/internal/runner/secure_bwrap_test.go`:

```go
func TestBuildSecureBwrapArgsIncludesExtraRoBinds(t *testing.T) {
	args := buildSecureBwrapArgs(secureBwrapSpec{
		RootFS:        "/rootfs",
		WorkDir:       "/work",
		Binary:        "claude",
		EnableNetwork: false,
		ExtraRoBinds:  []string{"/usr/local/bin", "/opt/zgi/agent-cli"},
	})
	argsStr := strings.Join(args, " ")
	if !strings.Contains(argsStr, "--ro-bind /usr/local/bin /opt/zgi/agent-cli") {
		t.Fatalf("missing extra ro-bind, got: %s", argsStr)
	}
	if !strings.Contains(argsStr, "--unshare-net") {
		t.Fatalf("expected --unshare-net when network disabled, got: %s", argsStr)
	}
}
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd sandbox && go test ./internal/runner/ -run TestBuildSecureBwrapArgsIncludesExtraRoBinds -v`
Expected: FAIL — no `ExtraRoBinds` field / ro-bind absent.

- [ ] **Step 4: Implement the secure StartProcess**

Replace the placeholder in `sandbox/internal/runner/backend_secure_linux.go`:

```go
func (b *linuxSecureBackend) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	env := spec.Env
	if env == nil {
		env = map[string]string{}
	}
	var roBinds []string
	if dir := strings.TrimSpace(b.agentCLIDir); dir != "" {
		roBinds = append(roBinds, dir, "/opt/zgi/agent-cli")
		if _, ok := env["PATH"]; !ok {
			env = cloneEnv(env)
			env["PATH"] = "/opt/zgi/agent-cli:" + defaultSecurePath
		}
	}
	bwrapArgs := buildSecureBwrapArgs(secureBwrapSpec{
		RootFS:        b.rootfs,
		WorkDir:       spec.WorkDir,
		Binary:        spec.Command,
		Args:          spec.Args,
		EnableNetwork: spec.EnableNetwork,
		Env:           env,
		ExtraRoBinds:  roBinds,
	})
	return startStreamedCommand(ctx, b.bwrapBin, bwrapArgs, spec.WorkDir, env)
}
```

Add the shared `startStreamedCommand` + `cloneEnv` helpers to `process.go` (used by both backends):

```go
func startStreamedCommand(ctx context.Context, command string, args []string, dir string, env map[string]string) (*ProcessSession, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var once sync.Once
	var waitErr error
	pid := cmd.Process.Pid
	return &ProcessSession{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Wait: func() error {
			once.Do(func() {
				waitErr = cmd.Wait()
				_ = stdin.Close()
			})
			return waitErr
		},
		Kill: func() error {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		},
	}, nil
}

func cloneEnv(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}
```

Refactor `processBackend.StartProcess` (from Task 1) to delegate to `startStreamedCommand` so the group-kill/stdin-close logic lives in one place:

```go
func (b *processBackend) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	return startStreamedCommand(ctx, spec.Command, spec.Args, spec.WorkDir, spec.Env)
}
```

In `backend_config.go`, plumb `AgentCLIDir` into the secure backend struct: add a field `agentCLIDir string` to `linuxSecureBackend` and set it in `newLinuxSecureBackend` (`agentCLIDir: strings.TrimSpace(cfg.AgentCLIDir)`).

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd sandbox && go test ./internal/runner/ -run 'TestBuildSecureBwrapArgsIncludesExtraRoBinds|TestProcessBackendStartProcess' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sandbox/internal/runner/backend_secure_linux.go sandbox/internal/runner/secure_bwrap.go sandbox/internal/runner/secure_bwrap_test.go sandbox/internal/runner/backend_config.go sandbox/internal/runner/process.go
git commit -m "✨ feat(sandbox): streamed StartProcess for the linux-secure backend"
```

---

### Task 4: Agent-box create endpoint

**Files:**
- Create: `sandbox/internal/app/agentbox.go`
- Modify: `sandbox/internal/app/server.go` (route registration)
- Test: `sandbox/internal/app/agentbox_test.go`

**Interfaces:**
- Consumes: `s.lifecycle.Create(lifecycle.CreateRequest)`, `s.config.AgentBoxTTLSeconds` (Task 2), `s.policy.NetworkPolicyEnforced()`, `s.observer.Record`.
- Produces: `POST /v1/agent-boxes` returning `{ box_id, workspace_path }`.

- [ ] **Step 1: Write the failing test**

Create `sandbox/internal/app/agentbox_test.go` (uses the existing `NewServer(testConfig(t))` harness):

```go
package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentBoxCreate(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}

	body := `{"ttl_seconds": 120, "network_enabled": true, "workspace_seed": {"CLAUDE.md": "# ZGI\n"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent-boxes", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "req_agentbox")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Data struct {
			BoxID         string `json:"box_id"`
			WorkspacePath string `json:"workspace_path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Data.BoxID == "" {
		t.Fatal("expected box_id")
	}
	if envelope.Data.WorkspacePath == "" {
		t.Fatal("expected workspace_path")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sandbox && go test ./internal/app/ -run TestAgentBoxCreate -v`
Expected: FAIL — 404 (route not registered).

- [ ] **Step 3: Implement the handler**

Create `sandbox/internal/app/agentbox.go`:

```go
package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zgiai/zgi-sandbox/internal/lifecycle"
	"github.com/zgiai/zgi-sandbox/internal/observer"
)

type agentBoxCreateRequest struct {
	TTLSeconds     int               `json:"ttl_seconds,omitempty"`
	NetworkEnabled bool              `json:"network_enabled,omitempty"`
	WorkspaceSeed  map[string]string `json:"workspace_seed,omitempty"`
	OrganizationID string            `json:"organization_id,omitempty"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
}

func (s *Server) handleAgentBoxCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}

	var req agentBoxCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxSmallJSONRequestBytes())).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = s.config.AgentBoxTTLSeconds
	}

	box, err := s.lifecycle.Create(lifecycle.CreateRequest{
		RuntimeProfile: "session",
		TTLSeconds:     ttl,
		NetworkEnabled: req.NetworkEnabled,
		OrganizationID: req.OrganizationID,
		WorkspaceID:    req.WorkspaceID,
		UserID:         req.UserID,
	})
	if err != nil {
		writeKnownError(w, err)
		return
	}

	for name, content := range req.WorkspaceSeed {
		if err := writeAgentBoxSeed(box.RootPath, name, content); err != nil {
			writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, err.Error(), nil)
			return
		}
	}

	workspacePath := "/tmp/workspace"
	if !s.policy.NetworkPolicyEnforced() {
		workspacePath = box.RootPath
	}

	s.observer.Record("agent.box.created", box.ID, "agent box created", observer.MetadataWithContext(r.Context(), map[string]any{
		"runtime_profile": "session",
		"ttl_seconds":     ttl,
		"network_enabled": req.NetworkEnabled,
	}))

	writeEnvelope(w, http.StatusOK, map[string]any{
		"box_id":         box.ID,
		"workspace_path": workspacePath,
	})
}

func writeAgentBoxSeed(root string, name string, content string) error {
	switch name {
	case "CLAUDE.md", "AGENTS.md":
	default:
		return fmt.Errorf("unsupported seed file: %s", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New("unsupported seed file name")
	}
	path := filepath.Join(root, name)
	return os.WriteFile(path, []byte(content), 0o644)
}
```

In `sandbox/internal/app/server.go` `registerRoutes`, add:

```go
	s.mux.HandleFunc("/v1/agent-boxes", s.handleAgentBoxCreate)
	s.mux.HandleFunc("/v1/agent-boxes/", s.handleAgentBoxByID)
```

Also add `handleAgentBoxByID` to `agentbox.go` (supports `GET`/`DELETE`):

```go
func (s *Server) handleAgentBoxByID(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		box, err := s.lifecycle.Get(id)
		if err != nil {
			writeKnownError(w, err)
			return
		}
		writeEnvelope(w, http.StatusOK, map[string]any{"id": box.ID, "status": string(box.Status), "root_path": box.RootPath})
	case http.MethodDelete:
		if err := s.lifecycle.Delete(id); err != nil {
			writeKnownError(w, err)
			return
		}
		writeEnvelope(w, http.StatusOK, map[string]any{"deleted": true, "box_id": id})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd sandbox && go test ./internal/app/ -run TestAgentBoxCreate -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add sandbox/internal/app/agentbox.go sandbox/internal/app/agentbox_test.go sandbox/internal/app/server.go
git commit -m "✨ feat(sandbox): add agent-box create endpoint"
```

---

### Task 5: Agent-box streamed process channel

**Files:**
- Modify: `sandbox/internal/app/agentbox.go` (process handlers + registry)
- Modify: `sandbox/internal/app/server.go` (Server struct fields)
- Test: `sandbox/internal/app/agentbox_process_test.go`

**Interfaces:**
- Consumes: `s.runner` backend via `runner.StartProcess` (Task 1/3), `s.config.MaxAgentProcesses` (Task 2).
- Produces:
  - `POST /v1/agent-boxes/:id/process` — SSE stream. First frame `{type:"started",pid}` then `{type:"stdout",data}` / `{type:"stderr",data}` / `{type:"exit",code}` / `{type:"error",message}`.
  - `POST /v1/agent-boxes/:id/process/:pid/stdin` — body is raw bytes appended to the process stdin.
  - `DELETE /v1/agent-boxes/:id/process/:pid` — kill the process.

- [ ] **Step 1: Write the failing test**

Create `sandbox/internal/app/agentbox_process_test.go` (uses a real HTTP server so the SSE stream can be consumed concurrently):

```go
package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentBoxProcessChannel(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createReq, _ := json.Marshal(map[string]any{"ttl_seconds": 120})
	resp, err := http.Post(ts.URL+"/v1/agent-boxes", "application/json", bytes.NewReader(createReq))
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	var createEnvelope struct {
		Data struct {
			BoxID string `json:"box_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&createEnvelope)
	resp.Body.Close()
	boxID := createEnvelope.Data.BoxID
	if boxID == "" {
		t.Fatal("expected box_id")
	}

	// Start a process that reads 5 bytes then prints EOF and exits.
	procReq, _ := json.Marshal(map[string]any{
		"command": "/bin/sh",
		"args":    []string{"-c", "head -c 5; echo EOF"},
	})
	procResp, err := http.Post(ts.URL+"/v1/agent-boxes/"+boxID+"/process", "application/json", bytes.NewReader(procReq))
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer procResp.Body.Close()

	scanner := bufio.NewScanner(procResp.Body)
	readFrame := func() (map[string]any, error) {
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				return nil, err
			}
			return frame, nil
		}
		return nil, scanner.Err()
	}

	started, err := readFrame()
	if err != nil || started["type"] != "started" {
		t.Fatalf("expected started frame, got %v err=%v", started, err)
	}
	pid := started["pid"].(string)

	stdinReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent-boxes/"+boxID+"/process/"+pid+"/stdin", bytes.NewReader([]byte("hello")))
	stdinResp, err := http.DefaultClient.Do(stdinReq)
	if err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	io.Copy(io.Discard, stdinResp.Body)
	stdinResp.Body.Close()

	got := map[string]string{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := readFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		ftype, _ := frame["type"].(string)
		switch ftype {
		case "stdout":
			got["stdout"] += frame["data"].(string)
		case "exit":
			got["exit"] = fmt.Sprintf("%v", frame["code"])
		}
		if got["exit"] != "" {
			break
		}
	}
	if !strings.Contains(got["stdout"], "hello") || !strings.Contains(got["stdout"], "EOF") {
		t.Fatalf("stdout = %q, want it to contain hello and EOF", got["stdout"])
	}
	if got["exit"] != "0" {
		t.Fatalf("exit = %q, want 0", got["exit"])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd sandbox && go test ./internal/app/ -run TestAgentBoxProcessChannel -v`
Expected: FAIL — `head -c 5` never runs (route missing → 404 on process start).

- [ ] **Step 3: Implement the process channel**

Add to `sandbox/internal/app/agentbox.go`:

```go
type agentProcessRequest struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type activeBoxProcess struct {
	pid  string
	sess interface {
		Stdin() io.WriteCloser
	}
	done chan struct{}
}

func (s *Server) handleAgentBoxProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) < 2 || parts[1] != "process" {
		http.NotFound(w, r)
		return
	}
	boxID := parts[0]

	box, err := s.lifecycle.Get(boxID)
	if err != nil {
		writeKnownError(w, err)
		return
	}

	var req agentProcessRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxSmallJSONRequestBytes())).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, "command is required", nil)
		return
	}

	if !s.agentProcessAcquire(r.Context(), w) {
		return
	}
	defer s.agentProcessRelease()

	workDir := box.RootPath
	sess, err := s.runner.StartProcess(r.Context(), runner.ProcessSpec{
		WorkDir:       workDir,
		Command:       req.Command,
		Args:          req.Args,
		Env:           req.Env,
		EnableNetwork: box.NetworkEnabled,
	})
	if err != nil {
		writeKnownError(w, err)
		return
	}
	pid := "ap_" + randToken()
	s.agentProcesses[pid] = sess
	defer func() {
		delete(s.agentProcesses, pid)
		_ = sess.Kill()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeAgentFrame(w, flusher, map[string]any{"type": "started", "pid": pid})

	streamAgentOutput(w, flusher, "stdout", sess.Stdout)
	streamAgentOutput(w, flusher, "stderr", sess.Stderr)

	exitCode := 0
	if err := sess.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	writeAgentFrame(w, flusher, map[string]any{"type": "exit", "code": exitCode})
	s.observer.Record("agent.process.exited", boxID, "agent process exited", observer.MetadataWithContext(r.Context(), map[string]any{"pid": pid, "exit_code": exitCode}))
}
```

Supporting helpers (also in `agentbox.go`):

```go
func writeAgentFrame(w http.ResponseWriter, flusher http.Flusher, frame map[string]any) {
	payload, _ := json.Marshal(frame)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
}

func streamAgentOutput(w http.ResponseWriter, flusher http.Flusher, frameType string, reader io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			writeAgentFrame(w, flusher, map[string]any{"type": frameType, "data": string(buf[:n])})
		}
		if err != nil {
			return
		}
	}
}
```

The `handleAgentBoxByID` dispatcher must route `/v1/agent-boxes/:id/process`, `…/:id/process/:pid/stdin`, and `…/:id/process/:pid` — rewrite `handleAgentBoxByID` to delegate:

```go
func (s *Server) handleAgentBoxByID(w http.ResponseWriter, r *http.Request) {
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 2 && parts[1] == "process" {
		if len(parts) == 2 {
			s.handleAgentBoxProcess(w, r)
			return
		}
		pid := parts[2]
		switch r.Method {
		case http.MethodPost:
			if len(parts) != 4 || parts[3] != "stdin" {
				http.NotFound(w, r)
				return
			}
			s.handleAgentBoxStdin(w, r, pid)
		case http.MethodDelete:
			s.handleAgentBoxKill(w, r, pid)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	// GET / DELETE box (existing logic) …
}
```

Stdin + kill handlers:

```go
func (s *Server) handleAgentBoxStdin(w http.ResponseWriter, r *http.Request, pid string) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	sess, ok := s.agentProcesses[pid]
	if !ok {
		writeEnvelopeWithMessage(w, http.StatusNotFound, -404, "process not found", nil)
		return
	}
	defer r.Body.Close()
	_, err := io.Copy(sess.Stdin, r.Body)
	if err != nil {
		writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, err.Error(), nil)
		return
	}
	writeEnvelope(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAgentBoxKill(w http.ResponseWriter, r *http.Request, pid string) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	sess, ok := s.agentProcesses[pid]
	if !ok {
		writeEnvelopeWithMessage(w, http.StatusNotFound, -404, "process not found", nil)
		return
	}
	_ = sess.Kill()
	writeEnvelope(w, http.StatusOK, map[string]any{"killed": true, "pid": pid})
}
```

Update the `activeBoxProcess`-style registry: change the `Server` struct in `server.go` to hold `agentProcesses map[string]*runner.ProcessSession` (initialize in `NewServer`), and add the two admission helpers backed by a `chan struct{}` semaphore sized by `cfg.MaxAgentProcesses`:

```go
// in Server struct:
	agentProcesses map[string]*runner.ProcessSession
	agentProcessSem chan struct{}

// in NewServer, after the other fields:
		agentProcesses:  map[string]*runner.ProcessSession{},
		agentProcessSem: make(chan struct{}, cfg.MaxAgentProcesses),

// helpers:
func (s *Server) agentProcessAcquire(ctx context.Context, w http.ResponseWriter) bool {
	select {
	case s.agentProcessSem <- struct{}{}:
		return true
	default:
		writeEnvelopeWithMessage(w, http.StatusTooManyRequests, -429, "too many concurrent agent processes", nil)
		return false
	}
}

func (s *Server) agentProcessRelease() { <-s.agentProcessSem }
```

Also add `randToken()` (a small hex token helper) if none exists — reuse `lifecycle`'s internal token by exporting a tiny helper in `agentbox.go`:

```go
func randToken() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "ap"
	}
	return hex.EncodeToString(buf[:])
}
```

Adjust the `ProcessSession` type so `Stdin` is settable through the registry — keep it as-is (`Stdin io.WriteCloser`); the `activeBoxProcess` type above is not needed; delete it. The registry maps `pid → *runner.ProcessSession`, and the stdin/kill handlers read `sess.Stdin` / `sess.Kill`.

- [ ] **Step 4: Run test to verify it passes**

Run: `cd sandbox && go test ./internal/app/ -run TestAgentBoxProcessChannel -v`
Expected: PASS.

- [ ] **Step 5: Run the whole sandbox test suite to catch regressions**

Run: `cd sandbox && go test ./...`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add sandbox/internal/app/agentbox.go sandbox/internal/app/agentbox_process_test.go sandbox/internal/app/server.go
git commit -m "✨ feat(sandbox): add streamed agent-box process channel"
```

---

### Task 6: agent-runner sandbox client + protocol

**Files:**
- Modify: `agent-runner/src/protocol.ts` (RunRequest `sandbox` field)
- Create: `agent-runner/src/sandboxClient.ts`
- Test: `agent-runner/test/sandboxClient.test.ts`

**Interfaces:**
- Consumes: `RunRequest` shape from `protocol.ts`.
- Produces:
  - `interface SandboxConfig { url: string; api_key?: string }` (on `RunRequest.sandbox`)
  - `class SandboxClient` with `createBox(req): Promise<Box>`, `openProcess(boxId, req, signal): Promise<ProcessHandle>`, `writeStdin(boxId, pid, data): Promise<void>`, `killProcess(boxId, pid): Promise<void>`, `deleteBox(boxId): Promise<void>`
  - `interface Box { boxId: string; workspacePath: string }`
  - `interface ProcessHandle { pid: string; stdin: Writable; stdout: Readable; stderr: Readable; exitCode: number | null; exited: Promise<number | null>; kill(): void }`

- [ ] **Step 1: Add the protocol field**

In `agent-runner/src/protocol.ts`, extend `RunRequest`:

```ts
  /** ZGI LLM gateway base URL; when set, codex/claude route model calls through it. */
  gatewayUrl?: string;
  /** When set, the Agent CLI process runs inside a zgi-sandbox agent box. */
  sandbox?: SandboxConfig;
```

and add:

```ts
export interface SandboxConfig {
  url: string;
  api_key?: string;
}
```

Update `parseRunRequest` to read `raw.sandbox`:

```ts
    gatewayUrl: raw.gateway_url ? String(raw.gateway_url) : undefined,
    sandbox: parseSandbox(raw.sandbox),
```

with:

```ts
function parseSandbox(value: unknown): SandboxConfig | undefined {
  if (!value || typeof value !== 'object') return undefined;
  const raw = value as Record<string, unknown>;
  const url = String(raw.url || '');
  if (!url) return undefined;
  return { url, api_key: raw.api_key ? String(raw.api_key) : undefined };
}
```

- [ ] **Step 2: Write the failing tests**

Create `agent-runner/test/sandboxClient.test.ts`:

```ts
import assert from 'node:assert/strict';
import http from 'node:http';
import { test } from 'node:test';

import { SandboxClient } from '../src/sandboxClient.js';

function listen(fn: (req: http.IncomingMessage, res: http.ServerResponse) => void): Promise<{ port: number; close: () => Promise<void> }> {
  return new Promise((resolve) => {
    const server = http.createServer(fn);
    server.listen(0, '127.0.0.1', () => {
      const addr = server.address() as { port: number };
      resolve({
        port: addr.port,
        close: () => new Promise((res) => server.close(() => res())),
      });
    });
  });
}

test('SandboxClient.createBox posts to /v1/agent-boxes and parses the envelope', async () => {
  const fake = await listen((req, res) => {
    assert.equal(req.method, 'POST');
    assert.equal(req.url, '/v1/agent-boxes');
    res.setHeader('Content-Type', 'application/json');
    res.end(JSON.stringify({ data: { box_id: 'sbx_1', workspace_path: '/tmp/workspace' } }));
  });
  try {
    const client = new SandboxClient({ baseUrl: `http://127.0.0.1:${fake.port}` });
    const box = await client.createBox({ ttlSeconds: 120, networkEnabled: true, workspaceSeed: { 'CLAUDE.md': '# ZGI' } });
    assert.equal(box.boxId, 'sbx_1');
    assert.equal(box.workspacePath, '/tmp/workspace');
  } finally {
    await fake.close();
  }
});
```

- [ ] **Step 3: Run test to verify it fails**

Run: `cd agent-runner && npm test`
Expected: FAIL — module `../src/sandboxClient.js` not found.

- [ ] **Step 4: Implement `sandboxClient.ts`**

```ts
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

    let resolvePidReady: (() => void) | null = null;
    const pidReady = new Promise<void>((resolve) => {
      resolvePidReady = resolve;
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
```

Note: `openProcess` resolves as soon as the SSE response starts, but the boxed `pid` only arrives in the `started` frame — so the stdin `Writable` queues writes until `pid` is known (the `pidReady` promise resolves when the `started` frame is parsed).

- [ ] **Step 5: Run tests to verify they pass**

Run: `cd agent-runner && npm test`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent-runner/src/protocol.ts agent-runner/src/sandboxClient.ts agent-runner/test/sandboxClient.test.ts
git commit -m "✨ feat(agent-runner): add sandbox client and protocol config"
```

---

### Task 7: Claude adapter spawns the CLI inside the box

**Files:**
- Modify: `agent-runner/src/adapters/claude.ts`
- Test: `agent-runner/test/claudeBoxed.test.ts`

**Interfaces:**
- Consumes: `BoxManager` (Task 9), `SandboxClient`/`Box`/`ProcessHandle` (Task 6), `SpawnedProcess` contract from `@anthropic-ai/claude-agent-sdk`.
- Produces: a `spawnClaudeCodeProcess` option on the claude `query()` options that returns a `SpawnedProcess` bridged to the boxed `claude` process. `runClaude` keeps its `(req, deps)` signature and resolves the box via `deps.boxManager`.

- [ ] **Step 1: Write the failing test**

Create `agent-runner/test/claudeBoxed.test.ts`. It verifies that `buildClaudeSpawn(boxed)` returns a `SpawnedProcess` whose `stdin`/`stdout` are the boxed handle's and that `kill()` forwards to the handle:

```ts
import assert from 'node:assert/strict';
import { Readable, Writable } from 'node:stream';
import { test } from 'node:test';

import { toSpawnedProcess } from '../src/adapters/claude.js';
import type { ProcessHandle } from '../src/sandboxClient.js';

function fakeHandle(overrides: Partial<ProcessHandle> = {}): ProcessHandle {
  return {
    pid: 'p1',
    stdin: new Writable({ write(_c, _e, cb) { cb(); } }),
    stdout: Readable.from(['out']),
    stderr: Readable.from(['err']),
    exitCode: null,
    exited: Promise.resolve(null),
    kill() {},
    ...overrides,
  };
}

test('toSpawnedProcess exposes the boxed streams and forwards kill', async () => {
  let killed = 0;
  const handle = fakeHandle({ kill: () => { killed += 1; } });
  const spawned = toSpawnedProcess(handle);

  assert.equal(spawned.stdin, handle.stdin);
  assert.equal(spawned.stdout, handle.stdout);
  assert.equal(spawned.killed, false);
  spawned.kill('SIGKILL');
  assert.equal(killed, 1);
  assert.equal(spawned.exitCode, null);

  const exitCodes: Array<number | null> = [];
  spawned.on('exit', (code) => exitCodes.push(code));
  spawned.exitCode = 0;
  spawned.emit('exit', 0, null);
  assert.deepEqual(exitCodes, [0]);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent-runner && npm test`
Expected: FAIL — `toSpawnedProcess` not exported from `claude.js`.

- [ ] **Step 3: Implement**

In `agent-runner/src/adapters/claude.ts`, export a `toSpawnedProcess` helper and wire it into `runClaude`:

```ts
import { EventEmitter } from 'node:events';
import type { Readable, Writable } from 'node:stream';

import type { ProcessHandle } from '../sandboxClient.js';

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

export function toSpawnedProcess(handle: ProcessHandle): SpawnedProcess {
  const ee = new EventEmitter();
  let killed = false;
  handle.exited.then((code) => {
    ee.emit('exit', code, null);
  }).catch((err) => {
    ee.emit('error', err as Error);
  });
  return {
    stdin: handle.stdin,
    stdout: handle.stdout,
    get killed() {
      return killed;
    },
    get exitCode() {
      return handle.exitCode;
    },
    signalCode: null,
    kill(signal: NodeJS.Signals): boolean {
      killed = true;
      handle.kill();
      return true;
    },
    on(event, listener) {
      ee.on(event, listener);
      return spawned;
    },
    once(event, listener) {
      ee.once(event, listener);
      return spawned;
    },
    off(event, listener) {
      ee.off(event, listener);
      return spawned;
    },
  };
}
```

(Assign `const spawned = …` and have the chained methods return it.)

In `runClaude`, when `deps.boxManager` is present, resolve the box and open the boxed `claude` process before `query()`, then set `spawnClaudeCodeProcess` to bridge it:

```ts
export async function runClaude(req: RunRequest, deps: AdapterDeps): Promise<AdapterResult> {
  const { emit, emitSession, abortController, pending } = deps;
  const allowed = new Set(req.allowedTools);
  const disallowed = new Set(req.disallowedTools);
  let sessionId: string | null = req.sessionId || null;
  // … canUseTool unchanged …

  let boxed: ProcessHandle | null = null;
  if (deps.boxManager) {
    const box = await deps.boxManager.ensure(req.sessionId, req);
    boxed = await deps.boxManager.openProcess(box, {
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
    options.spawnClaudeCodeProcess = () => toSpawnedProcess(boxed!);
  }
  // … model/resume/systemPrompt/mcp setup unchanged …
}
```

`spawnClaudeCodeProcess` ignores the SDK's `SpawnOptions` (command/args/cwd/env) because the process already runs in the box; returning the pre-opened handle is all the SDK needs. When the boxed process exits, `toSpawnedProcess` forwards the exit/error events to the SDK through its `EventEmitter`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent-runner && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent-runner/src/adapters/claude.ts agent-runner/test/claudeBoxed.test.ts
git commit -m "✨ feat(agent-runner): bridge claude CLI into a sandbox box via spawnClaudeCodeProcess"
```

---

### Task 8: Codex adapter + bridge executable

**Files:**
- Create: `agent-runner/src/codexBridge.mjs`
- Modify: `agent-runner/src/adapters/codex.ts`
- Test: `agent-runner/test/codexBridge.test.ts`

**Interfaces:**
- Consumes: `SandboxClient`/`ProcessHandle` (Task 6).
- Produces: `src/codexBridge.mjs` — an executable the codex SDK spawns (via `codexPathOverride`) that tunnels `codex exec --experimental-json …` into the box; adapter sets `codexPathOverride` + `ZGI_*` env.

- [ ] **Step 1: Write the failing test**

Create `agent-runner/test/codexBridge.test.ts`. It verifies the bridge translates argv (dropping `node` + script) into a boxed `codex exec` command and pipes stdio, using a fake `SandboxClient`:

```ts
import assert from 'node:assert/strict';
import { PassThrough } from 'node:stream';
import { test } from 'node:test';

import { parseCodexBridgeArgs } from '../src/codexBridge.mjs';

test('parseCodexBridgeArgs strips node and script, keeps exec flags', () => {
  const args = parseCodexBridgeArgs(['node', '/zgi/codexBridge.mjs', 'exec', '--experimental-json', '--cd', '/tmp/workspace']);
  assert.deepEqual(args, ['exec', '--experimental-json', '--cd', '/tmp/workspace']);
});

test('parseCodexBridgeArgs handles a bare exec command', () => {
  assert.deepEqual(parseCodexBridgeArgs(['node', 'bridge', 'exec']), ['exec']);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent-runner && npm test`
Expected: FAIL — module `../src/codexBridge.mjs` not found.

- [ ] **Step 3: Implement `codexBridge.mjs`**

```js
// Executable that the codex SDK spawns when codexPathOverride is set. The SDK
// invokes this as: <bridge> exec --experimental-json … — we forward those argv
// into a `codex` process running inside the zgi-sandbox agent box and bridge
// stdio (SDK stdin -> box stdin; box stdout/stderr -> our stdout/stderr).
import { createSandboxClient } from './sandboxClient.js';
import { Writable } from 'node:stream';

export function parseCodexBridgeArgs(argv) {
  // argv = [node, bridgePath, ...codexArgs]
  return argv.slice(2);
}

async function main() {
  const baseUrl = process.env.ZGI_SANDBOX_URL;
  const apiKey = process.env.ZGI_SANDBOX_API_KEY;
  const boxId = process.env.ZGI_BOX_ID;
  if (!baseUrl || !boxId) {
    process.stderr.write('codexBridge: ZGI_SANDBOX_URL and ZGI_BOX_ID are required\n');
    process.exit(2);
  }
  const client = createSandboxClient({ baseUrl, apiKey });
  const args = parseCodexBridgeArgs(process.argv);
  const handle = await client.openProcess(boxId, { command: 'codex', args });

  const stdinWriter = new Writable({
    write(chunk, _enc, cb) {
      handle.stdin.write(chunk, cb);
    },
  });
  process.stdin.pipe(stdinWriter);
  handle.stdout.pipe(process.stdout);
  handle.stderr.pipe(process.stderr);
  process.on('SIGTERM', () => handle.kill());
  process.on('SIGINT', () => handle.kill());

  const code = await handle.exited;
  process.exit(code ?? 1);
}

main().catch((err) => {
  process.stderr.write(`codexBridge: ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
});
```

`src/sandboxClient.ts` must export a `createSandboxClient` factory:

```ts
export function createSandboxClient(opts: SandboxClientOptions): SandboxClient {
  return new SandboxClient(opts);
}
```

In `agent-runner/src/adapters/codex.ts`, wire the bridge. Before constructing `Codex`, resolve the box via `deps.boxManager` and set `codexPathOverride` + `ZGI_*` env so the bridge knows which box to tunnel into:

```ts
import { fileURLToPath } from 'node:url';
import type { Box } from '../sandboxClient.js';

const bridgePath = fileURLToPath(new URL('../codexBridge.mjs', import.meta.url));

// in runCodex, before `new Codex(...)`:
let box: Box | null = null;
if (req.sandbox) {
  if (!deps.boxManager) throw new Error('agent sandbox configured but no box manager');
  box = await deps.boxManager.ensure(req.sessionId, req);
  req.env = {
    ...req.env,
    ZGI_SANDBOX_URL: req.sandbox.url,
    ZGI_SANDBOX_API_KEY: req.sandbox.api_key ?? '',
    ZGI_BOX_ID: box.boxId,
  };
}
// …
const codex = new Codex({
  apiKey: req.env.OPENAI_API_KEY || req.env.CODEX_API_KEY || process.env.OPENAI_API_KEY,
  env: req.env,
  config,
  codexPathOverride: req.sandbox ? bridgePath : undefined,
});
```

(Add `boxManager?: BoxManager` to `AdapterDeps` in `protocol.ts` — see Task 9. The bridge process builds its own `SandboxClient` from the `ZGI_SANDBOX_URL`/`ZGI_SANDBOX_API_KEY` env and opens the boxed `codex exec` there.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent-runner && npm test`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agent-runner/src/codexBridge.mjs agent-runner/src/sandboxClient.ts agent-runner/src/adapters/codex.ts agent-runner/test/codexBridge.test.ts
git commit -m "✨ feat(agent-runner): bridge codex CLI into a sandbox box via codexPathOverride"
```

---

### Task 9: Box registry + run wiring

**Files:**
- Modify: `agent-runner/src/app.ts`
- Modify: `agent-runner/src/protocol.ts` (`AdapterDeps.boxManager`)
- Create: `agent-runner/src/boxManager.ts`
- Test: `agent-runner/test/boxManager.test.ts`

**Interfaces:**
- Consumes: `SandboxClient` (Task 6), `RunRequest.sandbox` (Task 6).
- Produces:
  - `class BoxManager { ensure(sessionId, req): Promise<Box>; release(sessionId): void; closeAll(): Promise<void> }`
  - `AdapterDeps.boxManager?: BoxManager`

- [ ] **Step 1: Write the failing tests**

Create `agent-runner/test/boxManager.test.ts`:

```ts
import assert from 'node:assert/strict';
import { test } from 'node:test';

import { BoxManager } from '../src/boxManager.js';
import type { SandboxClient } from '../src/sandboxClient.js';

class FakeClient {
  boxes: string[] = [];
  created = 0;
  async createBox(req: unknown): Promise<{ boxId: string; workspacePath: string }> {
    this.created += 1;
    const id = `sbx_${this.created}`;
    this.boxes.push(id);
    return { boxId: id, workspacePath: `/tmp/w${id}` };
  }
  async deleteBox(boxId: string): Promise<void> {
    this.boxes = this.boxes.filter((b) => b !== boxId);
  }
}

test('BoxManager reuses the same box for a session and deletes on release', async () => {
  const client = new FakeClient() as unknown as SandboxClient;
  const manager = new BoxManager(client);
  const req = { systemPrompt: '# ZGI' } as never;

  const first = await manager.ensure('s1', req);
  const second = await manager.ensure('s1', req);
  assert.equal(first.boxId, second.boxId);
  assert.equal(client.created, 1);

  manager.release('s1');
  await manager.closeAll();
  assert.equal(client.boxes.length, 0);
});
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd agent-runner && npm test`
Expected: FAIL — `../src/boxManager.js` not found.

- [ ] **Step 3: Implement `boxManager.ts`**

```ts
import type { Box, ProcessHandle, SandboxClient } from './sandboxClient.js';
import type { RunRequest } from './protocol.js';

export class BoxManager {
  private boxes = new Map<string, Box>();

  constructor(private client: SandboxClient) {}

  async ensure(sessionId: string, req: RunRequest): Promise<Box> {
    const existing = this.boxes.get(sessionId);
    if (existing) return existing;
    const box = await this.client.createBox({
      ttlSeconds: undefined,
      networkEnabled: true,
      workspaceSeed: seedFrom(req.systemPrompt),
    });
    this.boxes.set(sessionId, box);
    return box;
  }

  openProcess(box: Box, req: { command: string; args: string[]; env?: Record<string, string> }): Promise<ProcessHandle> {
    return this.client.openProcess(box.boxId, req);
  }

  release(sessionId: string): void {
    this.boxes.delete(sessionId);
  }

  async closeAll(): Promise<void> {
    const ids = [...this.boxes.keys()];
    for (const sessionId of ids) {
      const box = this.boxes.get(sessionId);
      if (box) await this.client.deleteBox(box.boxId).catch(() => {});
    }
    this.boxes.clear();
  }
}

function seedFrom(systemPrompt?: string): Record<string, string> {
  if (!systemPrompt || !systemPrompt.trim()) return {};
  return { 'CLAUDE.md': `# ZGI Coding Agent\n\n## Runtime Instructions\n\n${systemPrompt}\n` };
}
```

In `agent-runner/src/protocol.ts`, add `boxManager` to `AdapterDeps`:

```ts
export interface AdapterDeps {
  emit: (type: string, payload?: Record<string, unknown>) => void;
  emitSession: (agentSessionId: string | null) => void;
  abortController: AbortController;
  pending: Map<string, (value: PermissionDecision | null) => void>;
  boxManager?: BoxManager;
}
```

(Import it as a **type-only** import — `import type { BoxManager } from './boxManager.js';` — because `boxManager.ts` imports `type { RunRequest }` from this file; a value import would create a runtime circular dependency.)

In `agent-runner/src/app.ts`:
- Create the `SandboxClient` + `BoxManager` when `runReq.sandbox` is set, and attach to `deps`.
- Pass the boxed handle to `runClaude` when sandboxed (open the boxed `claude` process before `query()`).
- On run close, keep the box (TTL manages cleanup) but `boxManager.closeAll()` on server shutdown.

```ts
import { SandboxClient } from './sandboxClient.js';
import { BoxManager } from './boxManager.js';

// in the /v1/agents/run handler, after parsing runReq:
let boxManager: BoxManager | undefined;
if (runReq.sandbox) {
  const client = new SandboxClient({ baseUrl: runReq.sandbox.url, apiKey: runReq.sandbox.api_key });
  boxManager = new BoxManager(client);
}
// …
const deps = { emit, emitSession, abortController: controller, pending, boxManager };
await adapter(runReq, deps);
```

The adapters resolve the box themselves via `deps.boxManager` — claude in `runClaude` (Task 7), codex in `runCodex` (Task 8) — so the `adapter` dispatch keeps its uniform `(req, deps)` signature. Do **not** open the boxed process in `app.ts`; that stays inside the adapters.

In `server.ts`, on shutdown call `boxManager.closeAll()` before exiting:

```ts
for (const sig of ['SIGINT', 'SIGTERM'] as const) {
  process.on(sig, async () => {
    for (const run of runs.values()) run.controller.abort();
    await boxManager?.closeAll();
    server.close(() => process.exit(0));
  });
}
```

(Export `boxManager` from `app.ts` alongside `runs`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd agent-runner && npm test`
Expected: PASS.

- [ ] **Step 5: Run typecheck**

Run: `cd agent-runner && npx tsc --noEmit`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agent-runner/src/app.ts agent-runner/src/server.ts agent-runner/src/protocol.ts agent-runner/src/boxManager.ts agent-runner/src/adapters/claude.ts agent-runner/src/adapters/codex.ts agent-runner/test/boxManager.test.ts
git commit -m "✨ feat(agent-runner): wire per-session sandbox boxes into agent runs"
```

---

### Task 10: Go driver — sandbox config + skip local workspace

**Files:**
- Modify: `api/config/types.go`, `api/config/load.go`, `api/config/env_keys.go`
- Modify: `api/routes/v1/agents_routers.go`
- Modify: `api/internal/capabilities/agentruntime/cli/types.go`
- Modify: `api/internal/capabilities/agentruntime/cli/driver.go`
- Test: `api/internal/capabilities/agentruntime/cli/driver_test.go`

**Interfaces:**
- Consumes: `cli.Options` (driver config), `RunRequest` (runner payload).
- Produces:
  - `cli.SandboxConfig { URL string; APIKey string }` (on `RunRequest.Sandbox`)
  - `cli.Options.SandboxURL` / `cli.Options.SandboxAPIKey`

- [ ] **Step 1: Add config keys**

In `api/config/types.go` `AgentRunnerConfig`, add:

```go
	// SandboxURL/SandboxAPIKey, when set, run the Agent CLI process inside a
	// zgi-sandbox agent box instead of on the host.
	SandboxURL    string `json:"sandbox_url,omitempty"`
	SandboxAPIKey string `json:"-"`
```

In `api/config/env_keys.go`, add:

```go
	envAgentRunnerSandboxURL    = "ZGI_AGENT_SANDBOX_URL"
	envAgentRunnerSandboxAPIKey = "ZGI_AGENT_SANDBOX_API_KEY"
```

In `api/config/load.go` `loadAgentRunnerConfig`, add:

```go
		SandboxURL:    source.string("", envAgentRunnerSandboxURL),
		SandboxAPIKey: source.string("", envAgentRunnerSandboxAPIKey),
```

- [ ] **Step 2: Add the payload type + options**

In `api/internal/capabilities/agentruntime/cli/types.go`, add a `Sandbox` field to `RunRequest` and a type:

```go
	// GatewayURL is the ZGI LLM gateway base URL. When set, the runner points
	// codex/claude at the gateway instead of their external provider defaults.
	GatewayURL string `json:"gateway_url,omitempty"`
	// Sandbox, when set, runs the Agent CLI process inside a zgi-sandbox agent box.
	Sandbox *SandboxConfig `json:"sandbox,omitempty"`
```

```go
// SandboxConfig configures the zgi-sandbox agent-box runtime.
type SandboxConfig struct {
	URL    string `json:"url"`
	APIKey string `json:"api_key,omitempty"`
}
```

In `api/internal/capabilities/agentruntime/cli/driver.go` `Options`, add:

```go
	// SandboxURL/SandboxAPIKey, when set, run the Agent CLI inside a zgi-sandbox
	// agent box instead of on the host.
	SandboxURL    string
	SandboxAPIKey string
```

- [ ] **Step 3: Write the failing test**

Add to `api/internal/capabilities/agentruntime/cli/driver_test.go` (uses the existing `fakeRunner` + `dataLine` helpers):

```go
func TestCliDriverSandboxModeForwardsConfigAndSkipsLocalWorkspace(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "agent_session_id": "s"}),
		dataLine(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	root := t.TempDir()
	agentID := uuid.New()
	driver := NewDriver(Options{
		AgentType:      AgentTypeClaude,
		Enabled:        true,
		RunnerURL:      srv.URL,
		SandboxURL:     "http://127.0.0.1:2660",
		SandboxAPIKey:  "sk-sandbox",
		WorkspaceRoot:  root,
	})
	_, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:     agentID,
		TenantID:    uuid.New(),
		UserID:      uuid.New(),
		UserMessage: "go",
	}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	if runner.lastRun.Sandbox == nil {
		t.Fatal("expected run request to carry sandbox config")
	}
	if runner.lastRun.Sandbox.URL != "http://127.0.0.1:2660" {
		t.Fatalf("sandbox.url = %q", runner.lastRun.Sandbox.URL)
	}
	if runner.lastRun.Cwd != "" {
		t.Fatalf("cwd = %q, want empty in sandbox mode", runner.lastRun.Cwd)
	}
	if _, err := os.Stat(filepath.Join(root, agentID.String())); !os.IsNotExist(err) {
		t.Fatalf("expected no workspace dir under %s in sandbox mode", root)
	}
}
```

Add `"os"` and `"path/filepath"` to the test file imports.

- [ ] **Step 4: Run test to verify it fails**

Run: `cd api && go test ./internal/capabilities/agentruntime/cli/ -run TestChatStreamSkipsLocalWorkspaceInSandboxMode -v`
Expected: FAIL — sandbox field not populated / local workspace still created.

- [ ] **Step 5: Implement**

In `driver.go`, in `ChatStream`, gate the workspace creation on sandbox mode and populate the `RunRequest.Sandbox`:

```go
	cwd, err := d.ensureWorkspaceDir(ctx, req)
	if err != nil {
		return nil, err
	}
	var sandboxCfg *SandboxConfig
	if d.opts.SandboxURL != "" {
		// In sandbox mode the agent-runner owns the box workspace; skip the
		// local workspace dir and let the runner resolve cwd to the box.
		cwd = ""
		sandboxCfg = &SandboxConfig{URL: d.opts.SandboxURL, APIKey: d.opts.SandboxAPIKey}
	}
```

and set it on `runReq`:

```go
		Sandbox:        sandboxCfg,
```

Also skip the memory-file seed in sandbox mode (the runner seeds it from `systemPrompt` on box creation) — guard `ensureWorkspaceDir` so it returns `""` without creating a dir when `SandboxURL != ""`:

```go
func (d *CliDriver) ensureWorkspaceDir(ctx context.Context, req agentruntime.ChatRequest) (string, error) {
	if d.opts.SandboxURL != "" {
		return "", nil // sandbox mode: the agent-runner owns the box workspace
	}
	// … existing behavior unchanged
}
```

In `api/routes/v1/agents_routers.go`, pass the config into both drivers:

```go
		SandboxURL:        cfg.AgentRunner.SandboxURL,
		SandboxAPIKey:     cfg.AgentRunner.SandboxAPIKey,
```

(add to both the codex and claude `cli.Options` literals).

- [ ] **Step 6: Run tests to verify they pass**

Run: `cd api && go test ./internal/capabilities/agentruntime/cli/ -v`
Expected: PASS (all driver tests).

- [ ] **Step 7: Run the api build**

Run: `cd api && go build ./...`
Expected: compiles.

- [ ] **Step 8: Commit**

```bash
git add api/config/types.go api/config/load.go api/config/env_keys.go api/routes/v1/agents_routers.go api/internal/capabilities/agentruntime/cli/types.go api/internal/capabilities/agentruntime/cli/driver.go api/internal/capabilities/agentruntime/cli/driver_test.go
git commit -m "✨ feat(agent): route agent CLI runs through zgi-sandbox agent boxes when configured"
```

---

## End-to-end manual verification

After all tasks, run the full link on macOS (process backend):

1. Start zgi-sandbox with the process backend and a Postgres (see `sandbox/docker-compose.yml`), then:
   `cd sandbox && go run ./cmd/server`
2. Start agent-runner: `cd agent-runner && npm run dev`
3. Start the api with `ZGI_AGENT_SANDBOX_URL=http://127.0.0.1:2660` and `ZGI_AGENT_RUNNER_URL=http://127.0.0.1:3001`.
4. Kick a `runtime_type=claude` agent turn with a prompt like "run `pwd && echo boxed`".
5. Expect: the console shows the agent running; `pwd` output is `/tmp/workspace` (or the box workspace dir on the process backend); a `POST /v1/agent-boxes` appears in the zgi-sandbox logs; the `CLAUDE.md` seed file exists in the box workspace.
6. On Linux with `ZGI_SANDBOX_RUNTIME_BACKEND=linux-secure` + a rootfs + `ZGI_SANDBOX_AGENT_CLI_DIR` set, the same turn runs inside bwrap; `cat /proc/1/comm` inside a bash call returns the bwrap-init, and writing outside the box workspace fails.

## Self-review notes

- **Spec coverage:** agent boxes (T4), streamed process channel (T5), SDK stays in agent-runner via `spawnClaudeCodeProcess` (T7) + `codexPathOverride` (T8), box registry/seed (T9), Go driver sandbox config + skip local workspace (T10), config keys + bwrap CLI ro-bind (T2/T3), per-agent concurrency pool (T5), observer events (T4/T5). Non-sandbox path preserved (T6/T10 gating). macOS process-backend fallback is inherent (processBackend StartProcess, T1).
- **Type consistency:** `ProcessSpec`/`ProcessSession`/`StartProcess` shared across T1/T3/T5; `SandboxClient`/`Box`/`ProcessHandle` shared across T6/T7/T8/T9; `SandboxConfig` used in `protocol.ts` (T6) and `cli.RunRequest` (T10).
- **Placeholders:** none; every code step has concrete code and a run/verify command.
