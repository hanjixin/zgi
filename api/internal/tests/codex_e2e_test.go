package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	agentruntime "github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime/cli"
)

// fakeRunner is a scripted agent-runner used to exercise the Go control-plane
// drivers end-to-end without a real Agent CLI.
type fakeRunner struct {
	events      []string
	permissions []map[string]interface{}
	stopCalls   int
}

func (f *fakeRunner) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/agents/run":
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Accel-Buffering", "no")
			flusher, _ := w.(http.Flusher)
			for _, line := range f.events {
				fmt.Fprintf(w, "data: %s\n\n", line)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case strings.HasSuffix(r.URL.Path, "/stop"):
			f.stopCalls++
			w.Write([]byte(`{"code":0}`))
		case strings.HasSuffix(r.URL.Path, "/permission"):
			var body map[string]interface{}
			_ = json.NewDecoder(r.Body).Decode(&body)
			f.permissions = append(f.permissions, body)
			w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func dl(evt map[string]interface{}) string {
	b, _ := json.Marshal(evt)
	return string(b)
}

// buildRouter wires the same router shape as routes/v1/initAgentRuntimeRouter:
// business + codex (real Codex via runner) + claude-code (real Claude Code via runner).
func buildRouter(t *testing.T, runnerURL string) (*agentruntime.Router, *spyBusinessDriver) {
	t.Helper()
	business := &spyBusinessDriver{}
	governance := agentruntime.NewGovernanceApprovalService()

	codexDriver := cli.NewDriver(cli.Options{
		AgentType:      cli.AgentTypeCodex,
		Enabled:        true,
		RunnerURL:      runnerURL,
		SandboxMode:    "workspace-write",
		ApprovalPolicy: "never",
		AllowedTools:   []string{"Read", "Grep", "Glob", "Bash"},
		WorkspaceRoot:  t.TempDir(),
		Governance:     governance,
	})
	claudeDriver := cli.NewDriver(cli.Options{
		AgentType:      cli.AgentTypeClaude,
		Enabled:        true,
		RunnerURL:      runnerURL,
		PermissionMode: "acceptEdits",
		AllowedTools:   []string{"Read", "Grep", "Glob", "Bash"},
		WorkspaceRoot:  t.TempDir(),
		Governance:     governance,
	})

	router := agentruntime.NewRouter(
		agentruntime.WithBusinessDriver(business),
		agentruntime.WithCodexDriver(codexDriver),
		agentruntime.WithClaudeCodeDriver(claudeDriver),
	)
	return router, business
}

type spyBusinessDriver struct {
	chatted bool
}

func (d *spyBusinessDriver) RuntimeType() agentruntime.RuntimeType { return agentruntime.RuntimeTypeBusiness }
func (d *spyBusinessDriver) Chat(context.Context, agentruntime.ChatRequest) (*agentruntime.ChatResponse, error) {
	d.chatted = true
	return &agentruntime.ChatResponse{Status: "completed", Answer: "business answer"}, nil
}
func (d *spyBusinessDriver) ChatStream(context.Context, agentruntime.ChatRequest, func(string) error, func(agentruntime.StreamEvent) error) (*agentruntime.ChatResponse, error) {
	d.chatted = true
	return &agentruntime.ChatResponse{Status: "completed", Answer: "business answer"}, nil
}
func (d *spyBusinessDriver) Stop(context.Context, agentruntime.StopRequest) error { return nil }
func (d *spyBusinessDriver) LoadSession(context.Context, uuid.UUID, uuid.UUID) (*agentruntime.SessionState, error) {
	return nil, agentruntime.ErrSessionNotFound
}
func (d *spyBusinessDriver) SaveSession(context.Context, *agentruntime.SessionState) error { return nil }

func TestE2ECodexRoutesToRunnerDriver(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dl(map[string]interface{}{"type": "session_started", "agent_session_id": "codex-s1"}),
		dl(map[string]interface{}{"type": "text", "text": "scanning"}),
		dl(map[string]interface{}{"type": "tool_use", "id": "c1", "tool": "apply_patch", "input": map[string]interface{}{"patch": "..."}}),
		dl(map[string]interface{}{"type": "tool_result", "id": "c1", "tool": "apply_patch", "output": "ok"}),
		dl(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()
	router, business := buildRouter(t, srv.URL)

	driver := router.Route(context.Background(), agentruntime.AgentDescriptor{RuntimeType: agentruntime.RuntimeTypeCodex})
	if driver == nil || driver.RuntimeType() != agentruntime.RuntimeTypeCodex {
		t.Fatalf("codex descriptor not routed to codex driver")
	}

	var chunks []string
	var events []agentruntime.StreamEvent
	resp, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:     uuid.New(),
		UserID:      uuid.New(),
		TenantID:    uuid.New(),
		UserMessage: "fix the bug",
	}, func(chunk string) error {
		chunks = append(chunks, chunk)
		return nil
	}, func(evt agentruntime.StreamEvent) error {
		events = append(events, evt)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("status = %q, want completed", resp.Status)
	}
	if strings.Join(chunks, "") != "scanning" {
		t.Fatalf("answer = %q", strings.Join(chunks, ""))
	}
	if business.chatted {
		t.Fatal("codex traffic unexpectedly hit business driver")
	}
	assertE2EEvent(t, events, "skill_call_start")
	assertE2EEvent(t, events, "skill_call_end")
}

func TestE2EClaudeCodeRoutesToRunnerDriver(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dl(map[string]interface{}{"type": "session_started", "agent_session_id": "claude-s1"}),
		dl(map[string]interface{}{"type": "text", "text": "on it"}),
		dl(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()
	router, _ := buildRouter(t, srv.URL)

	driver := router.Route(context.Background(), agentruntime.AgentDescriptor{RuntimeType: agentruntime.RuntimeTypeClaudeCode})
	if driver == nil || driver.RuntimeType() != agentruntime.RuntimeTypeClaudeCode {
		t.Fatalf("claude-code descriptor not routed to claude-code driver")
	}
	resp, err := driver.Chat(context.Background(), agentruntime.ChatRequest{
		AgentID:     uuid.New(),
		UserID:      uuid.New(),
		TenantID:    uuid.New(),
		UserMessage: "refactor main.go",
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Answer != "on it" || resp.Status != "completed" {
		t.Fatalf("response = %#v", resp)
	}
}

func TestE2EBusinessStillRoutesToBusinessDriver(t *testing.T) {
	srv := httptest.NewServer((&fakeRunner{}).handler())
	defer srv.Close()
	router, business := buildRouter(t, srv.URL)

	driver := router.Route(context.Background(), agentruntime.AgentDescriptor{RuntimeType: agentruntime.RuntimeTypeBusiness})
	if driver == nil || driver.RuntimeType() != agentruntime.RuntimeTypeBusiness {
		t.Fatalf("business descriptor not routed to business driver")
	}
	resp, err := driver.Chat(context.Background(), agentruntime.ChatRequest{UserMessage: "hi"})
	if err != nil {
		t.Fatalf("business chat: %v", err)
	}
	if resp.Answer != "business answer" {
		t.Fatalf("business answer = %q", resp.Answer)
	}
	if !business.chatted {
		t.Fatal("business driver not invoked")
	}
}

func TestE2EStopRoutesToRunner(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dl(map[string]interface{}{"type": "session_started", "agent_session_id": "s"}),
		dl(map[string]interface{}{"type": "text", "text": "working"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()
	router, _ := buildRouter(t, srv.URL)

	driver := router.Route(context.Background(), agentruntime.AgentDescriptor{RuntimeType: agentruntime.RuntimeTypeCodex})
	conversationID := uuid.New()
	if err := driver.Stop(context.Background(), agentruntime.StopRequest{ConversationID: conversationID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if runner.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", runner.stopCalls)
	}
}

func TestE2EUnknownRuntimeFallsBackToBusiness(t *testing.T) {
	srv := httptest.NewServer((&fakeRunner{}).handler())
	defer srv.Close()
	router, _ := buildRouter(t, srv.URL)

	driver := router.Route(context.Background(), agentruntime.AgentDescriptor{RuntimeType: agentruntime.RuntimeType("custom")})
	if driver == nil || driver.RuntimeType() != agentruntime.RuntimeTypeBusiness {
		t.Fatalf("unknown runtime not routed to business driver")
	}
}

func assertE2EEvent(t *testing.T, events []agentruntime.StreamEvent, want string) {
	t.Helper()
	for _, evt := range events {
		if evt.EventType == want {
			return
		}
	}
	t.Fatalf("expected event %q in %d events", want, len(events))
}
