package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
)

// fakeRunner is a scripted agent-runner for tests.
type fakeRunner struct {
	events       []string // SSE data lines (JSON) to stream
	stopCalls    int
	permissions  []PermissionRequest
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
			var req PermissionRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.permissions = append(f.permissions, req)
			w.Write([]byte(`{"code":0}`))
		default:
			http.NotFound(w, r)
		}
	})
}

func dataLine(evt map[string]interface{}) string {
	b, _ := json.Marshal(evt)
	return string(b)
}

func TestCliDriverChatStreamMapsEvents(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "session_id": "s-1", "agent_session_id": "claude-sess-1"}),
		dataLine(map[string]interface{}{"type": "text", "text": "let me look"}),
		dataLine(map[string]interface{}{"type": "tool_use", "id": "call-1", "tool": "Bash", "input": map[string]interface{}{"command": "ls"}}),
		dataLine(map[string]interface{}{"type": "tool_result", "id": "call-1", "tool": "Bash", "output": "a.go", "is_error": false}),
		dataLine(map[string]interface{}{"type": "permission_request", "correlation_id": "corr-1", "tool": "Bash", "reason": "approval" }),
		dataLine(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	driver := NewDriver(Options{
		AgentType:      AgentTypeClaude,
		Enabled:        true,
		RunnerURL:      srv.URL,
		PermissionMode: "default",
		Governance:     agentruntime.NewGovernanceApprovalService(),
	})

	var chunks []string
	var events []agentruntime.StreamEvent
	resp, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:     uuid.New(),
		UserID:      uuid.New(),
		TenantID:    uuid.New(),
		UserMessage: "inspect the repo",
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
	if strings.Join(chunks, "") != "let me look" {
		t.Fatalf("chunks = %q, want %q", chunks, "let me look")
	}
	assertEvent(t, events, EventSkillCallStart)
	assertEvent(t, events, EventSkillCallEnd)
	assertEvent(t, events, EventApprovalRequired)

	// Bash maps to shell_run, which governance allows → auto-approved.
	if len(runner.permissions) != 1 || runner.permissions[0].Decision != "approve" {
		t.Fatalf("expected auto-approve for shell_run, got %#v", runner.permissions)
	}
}

func TestCliDriverAutoDeniesRiskyPermission(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "agent_session_id": "x"}),
		dataLine(map[string]interface{}{"type": "permission_request", "correlation_id": "corr-2", "tool": "Edit", "reason": "edit" }),
		dataLine(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	driver := NewDriver(Options{
		AgentType:  AgentTypeClaude,
		Enabled:    true,
		RunnerURL:  srv.URL,
		Governance: agentruntime.NewGovernanceApprovalService(),
	})
	_, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:     uuid.New(),
		UserID:      uuid.New(),
		TenantID:    uuid.New(),
		UserMessage: "edit a file",
	}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	// Edit maps to files_edit, which governance requires approval for → deny.
	if len(runner.permissions) != 1 || runner.permissions[0].Decision != "reject" {
		t.Fatalf("expected reject for files_edit, got %#v", runner.permissions)
	}
}

func TestCliDriverStopCallsRunner(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "agent_session_id": "y"}),
		dataLine(map[string]interface{}{"type": "text", "text": "working"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	driver := NewDriver(Options{AgentType: AgentTypeCodex, Enabled: true, RunnerURL: srv.URL})
	conversationID := uuid.New()
	if err := driver.Stop(context.Background(), agentruntime.StopRequest{ConversationID: conversationID}); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if runner.stopCalls != 1 {
		t.Fatalf("stopCalls = %d, want 1", runner.stopCalls)
	}
}

func TestCliDriverDisabledReturnsErr(t *testing.T) {
	driver := NewDriver(Options{AgentType: AgentTypeClaude, Enabled: false})
	if _, err := driver.Chat(context.Background(), agentruntime.ChatRequest{UserMessage: "hi"}); err != agentruntime.ErrRuntimeDisabled {
		t.Fatalf("disabled driver error = %v, want ErrRuntimeDisabled", err)
	}
}

func TestCliDriverRuntimeType(t *testing.T) {
	claude := NewDriver(Options{AgentType: AgentTypeClaude})
	if claude.RuntimeType() != agentruntime.RuntimeTypeClaudeCode {
		t.Fatalf("claude driver runtime = %s, want claude-code", claude.RuntimeType())
	}
	codex := NewDriver(Options{AgentType: AgentTypeCodex})
	if codex.RuntimeType() != agentruntime.RuntimeTypeCodex {
		t.Fatalf("codex driver runtime = %s, want codex", codex.RuntimeType())
	}
}

func assertEvent(t *testing.T, events []agentruntime.StreamEvent, want string) {
	t.Helper()
	for _, evt := range events {
		if evt.EventType == want {
			return
		}
	}
	t.Fatalf("expected event %q, got %v", want, eventTypes(events))
}

func eventTypes(events []agentruntime.StreamEvent) []string {
	out := make([]string, 0, len(events))
	for _, evt := range events {
		out = append(out, evt.EventType)
	}
	return out
}
