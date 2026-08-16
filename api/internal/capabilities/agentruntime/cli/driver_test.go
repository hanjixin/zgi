package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	lastRun      RunRequest
}

func (f *fakeRunner) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/agents/run":
			var runReq RunRequest
			_ = json.NewDecoder(r.Body).Decode(&runReq)
			f.lastRun = runReq
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

// TestCliDriverSkillCallPayloadsMatchFrontendContract locks the skill_call_*
// payload shape the console frontend requires (AIChatSkillCallStartEventData /
// AIChatSkillCallEndEventData). The frontend silently drops events missing
// conversation_id / message_id / skill_id, so the agent kernel must emit them.
func TestCliDriverSkillCallPayloadsMatchFrontendContract(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "session_id": "s-1", "agent_session_id": "sess"}),
		dataLine(map[string]interface{}{"type": "tool_use", "id": "call-1", "tool": "Bash", "input": map[string]interface{}{"command": "ls"}}),
		dataLine(map[string]interface{}{"type": "tool_result", "id": "call-1", "tool": "Bash", "output": "a.go", "is_error": false}),
		dataLine(map[string]interface{}{"type": "tool_use", "id": "call-2", "tool": "Bash", "input": map[string]interface{}{"command": "rm -rf x"}}),
		dataLine(map[string]interface{}{"type": "tool_result", "id": "call-2", "tool": "Bash", "output": "failed", "is_error": true}),
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

	conversationID := uuid.New()
	messageID := uuid.New()
	var events []agentruntime.StreamEvent
	_, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:        uuid.New(),
		ConversationID: &conversationID,
		MessageID:      messageID,
		UserID:         uuid.New(),
		TenantID:       uuid.New(),
		UserMessage:    "go",
	}, nil, func(evt agentruntime.StreamEvent) error {
		events = append(events, evt)
		return nil
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	var starts []ToolCallStartPayload
	var ends []ToolCallEndPayload
	var startPayloads, endPayloads []map[string]interface{}
	for _, evt := range events {
		switch evt.EventType {
		case EventSkillCallStart:
			var p ToolCallStartPayload
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				t.Fatalf("start payload: %v", err)
			}
			starts = append(starts, p)
			var raw map[string]interface{}
			_ = json.Unmarshal(evt.Payload, &raw)
			startPayloads = append(startPayloads, raw)
		case EventSkillCallEnd:
			var p ToolCallEndPayload
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				t.Fatalf("end payload: %v", err)
			}
			ends = append(ends, p)
			var raw map[string]interface{}
			_ = json.Unmarshal(evt.Payload, &raw)
			endPayloads = append(endPayloads, raw)
		}
	}
	if len(starts) != 2 || len(ends) != 2 {
		t.Fatalf("starts=%d ends=%d, want 2/2", len(starts), len(ends))
	}

	// Every event must carry the identity the frontend keys the timeline on.
	for i, p := range starts {
		if p.ConversationID != conversationID.String() {
			t.Fatalf("start[%d] conversation_id = %q", i, p.ConversationID)
		}
		if p.MessageID != messageID.String() {
			t.Fatalf("start[%d] message_id = %q, want %q", i, p.MessageID, messageID)
		}
		if p.SkillID == "" || p.ToolName == "" {
			t.Fatalf("start[%d] missing skill_id/tool_name: %#v", i, p)
		}
		if p.Status != "running" {
			t.Fatalf("start[%d] status = %q, want running", i, p.Status)
		}
	}
	if ends[0].Status != "success" {
		t.Fatalf("end[0] status = %q, want success", ends[0].Status)
	}
	if ends[1].Status != "error" {
		t.Fatalf("end[1] status = %q, want error", ends[1].Status)
	}
	if ends[0].DurationMS < 0 || ends[0].ConversationID != conversationID.String() || ends[0].MessageID != messageID.String() {
		t.Fatalf("end[0] identity/duration malformed: %#v", ends[0])
	}
}

type stubGatewayKeyResolver struct{ key string }

func (s *stubGatewayKeyResolver) ResolveGatewayKey(ctx context.Context, organizationID uuid.UUID) (string, error) {
	return s.key, nil
}

// TestCliDriverRoutesModelCallsThroughGateway verifies that when the LLM
// gateway is configured, the run request carries the gateway URL and the
// org's API key (codex), so the runner points the CLI at the gateway.
func TestCliDriverRoutesModelCallsThroughGateway(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "agent_session_id": "s"}),
		dataLine(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	driver := NewDriver(Options{
		AgentType:          AgentTypeCodex,
		Enabled:            true,
		RunnerURL:          srv.URL,
		LLMGatewayURL:      "http://127.0.0.1:2670",
		GatewayKeyResolver: &stubGatewayKeyResolver{key: "sk-test"},
		Governance:         agentruntime.NewGovernanceApprovalService(),
	})
	_, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:     uuid.New(),
		TenantID:    uuid.New(),
		UserID:      uuid.New(),
		UserMessage: "hi",
	}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if runner.lastRun.GatewayURL != "http://127.0.0.1:2670" {
		t.Fatalf("gateway_url = %q", runner.lastRun.GatewayURL)
	}
	if got := runner.lastRun.Env["OPENAI_API_KEY"]; got != "sk-test" {
		t.Fatalf("OPENAI_API_KEY = %q", got)
	}
	if got := runner.lastRun.Env["ZGI_LLM_GATEWAY_URL"]; got != "http://127.0.0.1:2670" {
		t.Fatalf("ZGI_LLM_GATEWAY_URL = %q", got)
	}
}

// TestCliDriverGatewayEnvForClaude verifies claude gets the Anthropic-compatible
// gateway base URL plus the org key.
func TestCliDriverGatewayEnvForClaude(t *testing.T) {
	driver := NewDriver(Options{
		AgentType:          AgentTypeClaude,
		Enabled:            true,
		LLMGatewayURL:      "http://127.0.0.1:2670/",
		GatewayKeyResolver: &stubGatewayKeyResolver{key: "sk-claude"},
	})
	env := driver.buildEnv(agentruntime.ChatRequest{}, "sk-claude")
	if got := env["ANTHROPIC_BASE_URL"]; got != "http://127.0.0.1:2670/anthropic" {
		t.Fatalf("ANTHROPIC_BASE_URL = %q", got)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-claude" {
		t.Fatalf("ANTHROPIC_API_KEY = %q", got)
	}
}

func TestCliDriverStampsCallerIdentityOnMcpServers(t *testing.T) {
	driver := NewDriver(Options{AgentType: AgentTypeCodex})
	userID := uuid.New()
	tenantID := uuid.New()
	conversationID := uuid.New()
	servers := driver.resolveMcpServers(agentruntime.ChatRequest{
		AgentID:        uuid.New(),
		UserID:         userID,
		TenantID:       tenantID,
		ConversationID: &conversationID,
		McpServers:     []agentruntime.McpServerConfig{{Name: "zgi-tools", Type: "http", URL: "http://x/mcp"}},
	})
	if len(servers) != 1 {
		t.Fatalf("servers = %d, want 1", len(servers))
	}
	h := servers[0].Headers
	for key, want := range map[string]string{
		"X-Zgi-User-Id":         userID.String(),
		"X-Zgi-Tenant-Id":       tenantID.String(),
		"X-Zgi-Conversation-Id": conversationID.String(),
	} {
		if h[key] != want {
			t.Fatalf("%s = %q, want %q", key, h[key], want)
		}
	}
	if h["X-Zgi-Agent-Id"] == "" {
		t.Fatal("X-Zgi-Agent-Id not stamped")
	}
}

func TestCliDriverPassesModelSystemPromptAndMcpServers(t *testing.T) {
	runner := &fakeRunner{events: []string{
		dataLine(map[string]interface{}{"type": "session_started", "agent_session_id": "s"}),
		dataLine(map[string]interface{}{"type": "done", "subtype": "success"}),
	}}
	srv := httptest.NewServer(runner.handler())
	defer srv.Close()

	driver := NewDriver(Options{
		AgentType:      AgentTypeClaude,
		Enabled:        true,
		RunnerURL:      srv.URL,
		McpServers: []agentruntime.McpServerConfig{
			{Name: "zgi-tools", Type: "http", URL: "http://zgi.local/mcp", Headers: map[string]string{"X-MCP-API-Key": "k"}},
		},
	})
	agentMcp := []agentruntime.McpServerConfig{{Name: "custom", Type: "stdio", Command: "npx"}}
	_, err := driver.ChatStream(context.Background(), agentruntime.ChatRequest{
		AgentID:      uuid.New(),
		UserID:       uuid.New(),
		TenantID:     uuid.New(),
		UserMessage:  "do work",
		SystemPrompt: "You are an engineer.",
		ModelName:    "claude-opus",
		McpServers:   agentMcp,
	}, nil, nil)
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}

	req := runner.lastRun
	if req.AgentType != AgentTypeClaude {
		t.Fatalf("agent_type = %q", req.AgentType)
	}
	if req.SystemPrompt != "You are an engineer." {
		t.Fatalf("system_prompt = %q", req.SystemPrompt)
	}
	if req.Model != "claude-opus" {
		t.Fatalf("model = %q", req.Model)
	}
	// Default ZGI MCP + per-agent MCP must both be forwarded.
	if len(req.McpServers) != 2 {
		t.Fatalf("mcp_servers = %#v, want 2 (default + agent)", req.McpServers)
	}
	if req.McpServers[0].Name != "zgi-tools" || req.McpServers[1].Name != "custom" {
		t.Fatalf("mcp_servers order/names = %#v", req.McpServers)
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
