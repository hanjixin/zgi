package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zgiai/zgi/api/internal/modules/tools"
	"github.com/zgiai/zgi/api/internal/modules/tools/builtin"
)

// fakeProvider is a minimal builtin tool provider for tests.
type fakeProvider struct{}

func (fakeProvider) GetProviderType() tools.ToolProviderType { return tools.ToolProviderTypeBuiltin }
func (fakeProvider) GetEntity() tools.ToolProviderEntity {
	return tools.ToolProviderEntity{
		Identity: tools.ToolProviderIdentity{Name: "fake", Label: tools.I18nText{}, Description: tools.I18nText{}},
		Tools: []tools.ToolEntity{{
			Identity: tools.ToolIdentity{Name: "echo_message"},
			Description: tools.ToolDescription{LLM: "Echo back the given message"},
			Parameters: []tools.ToolParameter{{
				Name:       "message",
				Type:       tools.ToolParameterTypeString,
				Required:   true,
				LLMDescription: "the text to echo",
			}},
		}},
	}
}
func (fakeProvider) GetTool(name string) (tools.Tool, error) {
	if name == "echo_message" {
		return nil, nil // only the entity list is exercised by listTools
	}
	return nil, errNotFound
}
func (fakeProvider) GetTools() []tools.Tool { return nil }
func (fakeProvider) ValidateCredentials(context.Context, map[string]interface{}) error { return nil }

var errNotFound = http.ErrBodyNotAllowed

func newTestServer(apiKey string) (*Server, *tools.ToolManager) {
	manager := tools.NewToolManager(nil)
	_ = manager.RegisterProvider(fakeProvider{})
	return NewServer(tools.NewToolEngine(manager), manager, apiKey), manager
}

func post(t *testing.T, srv *Server, body string, header string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if header != "" {
		req.Header.Set("X-MCP-API-Key", header)
	}
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	return w
}

func TestMCPInitialize(t *testing.T) {
	srv, _ := newTestServer("")
	w := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	var resp struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
			ServerInfo      struct {
				Name string `json:"name"`
			} `json:"serverInfo"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Result.ServerInfo.Name != serverName {
		t.Fatalf("server name = %q", resp.Result.ServerInfo.Name)
	}
	if resp.Result.ProtocolVersion != SupportedProtocolVersion {
		t.Fatalf("protocol = %q", resp.Result.ProtocolVersion)
	}
}

func TestMCPToolsList(t *testing.T) {
	srv, _ := newTestServer("")
	w := post(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`, "")
	var resp struct {
		Result struct {
			Tools []map[string]interface{} `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.Result.Tools) != 1 {
		t.Fatalf("tools len = %d, want 1", len(resp.Result.Tools))
	}
	tool := resp.Result.Tools[0]
	if tool["name"] != "echo_message" {
		t.Fatalf("tool name = %v", tool["name"])
	}
	schema, ok := tool["inputSchema"].(map[string]interface{})
	if !ok {
		t.Fatalf("inputSchema missing: %#v", tool)
	}
	if schema["type"] != "object" {
		t.Fatalf("schema type = %v", schema["type"])
	}
	props, ok := schema["properties"].(map[string]interface{})
	if !ok || props["message"] == nil {
		t.Fatalf("properties missing message param: %#v", schema)
	}
}

func TestMCPToolsCallUnknownToolReturnsErrorResult(t *testing.T) {
	srv, _ := newTestServer("")
	w := post(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"does_not_exist","arguments":{}}}`, "")
	var resp struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []map[string]interface{} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Result.IsError {
		t.Fatalf("unknown tool call should be isError: %#v", resp.Result)
	}
	if len(resp.Result.Content) == 0 {
		t.Fatal("error result should carry content")
	}
}

func TestMCPUnknownMethod(t *testing.T) {
	srv, _ := newTestServer("")
	w := post(t, srv, `{"jsonrpc":"2.0","id":4,"method":"resources/list","params":{}}`, "")
	var resp struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Error == nil || resp.Error.Code != errMethod {
		t.Fatalf("expected method-not-found error, got %#v", resp)
	}
}

// TestMCPSSEHandshake verifies the Streamable HTTP GET handshake Codex's MCP
// client requires: a 200 whose Content-Type is text/event-stream (anything
// else is an "unexpected content type" failure), an Mcp-Session-Id header, and
// a stream that stays open until the client disconnects.
func TestMCPSSEHandshake(t *testing.T) {
	srv, _ := newTestServer("")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.ServeHTTP(w, req)
	}()

	// Give the handler a moment to write headers, then cancel to release it.
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not release after context cancel")
	}

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}
	if w.Header().Get("Mcp-Session-Id") == "" {
		t.Fatal("Mcp-Session-Id header not set")
	}
	if !strings.Contains(w.Body.String(), ": connected") {
		t.Fatalf("body = %q, want ': connected' comment", w.Body.String())
	}
}

// TestMCPGetProbeKeepsJSON verifies a GET that does not ask for the SSE
// transport (e.g. a browser probe) keeps the old JSON info response.
func TestMCPGetProbeKeepsJSON(t *testing.T) {
	srv, _ := newTestServer("")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("content-type = %q, want application/json", ct)
	}
}

// TestMCPSSEAuthRequired verifies the SSE handshake honours the shared key the
// same way POST does.
func TestMCPSSEAuthRequired(t *testing.T) {
	srv, _ := newTestServer("secret-key")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/event-stream")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

// captureRecorder holds the caller identity observed by a capturingTool. The
// engine forks the tool before invoking, so the recorder is shared across the
// original and every forked instance.
type captureRecorder struct {
	mu     sync.Mutex
	userID string
	convID string
	appID  string
}

// capturingTool records the caller identity passed to Invoke so tests can
// assert the mcpbridge forwards the X-Zgi-* headers into the tool engine.
type capturingTool struct {
	*builtin.BuiltinTool
	rec *captureRecorder
}

func (t *capturingTool) Invoke(ctx context.Context, userID string, params map[string]interface{}, conversationID *string, appID *string, messageID *string) ([]tools.ToolInvokeMessage, error) {
	t.rec.mu.Lock()
	defer t.rec.mu.Unlock()
	t.rec.userID = userID
	if conversationID != nil {
		t.rec.convID = *conversationID
	}
	if appID != nil {
		t.rec.appID = *appID
	}
	return []tools.ToolInvokeMessage{builtin.CreateTextMessage("ok")}, nil
}

func (t *capturingTool) ForkToolRuntime(runtime *tools.ToolRuntime) tools.Tool {
	return &capturingTool{BuiltinTool: t.BuiltinTool.ForkToolRuntime(runtime), rec: t.rec}
}

// TestMCPCallCarriesCallerIdentity verifies tools/call forwards the caller
// identity the Go driver stamps on the MCP server http_headers into the tool
// engine, so user-scoped tools (memory, ...) resolve the right account.
func TestMCPCallCarriesCallerIdentity(t *testing.T) {
	manager := tools.NewToolManager(nil)
	rec := &captureRecorder{}
	tool := &capturingTool{
		BuiltinTool: builtin.NewBuiltinTool(tools.ToolEntity{
			Identity:    tools.ToolIdentity{Name: "echo", Author: "test", Provider: "capture"},
			Description: tools.ToolDescription{LLM: "echo"},
		}, ""),
		rec: rec,
	}
	prov := builtin.NewBuiltinProvider(tools.ToolProviderIdentity{Name: "capture", Label: tools.I18nText{}, Description: tools.I18nText{}})
	prov.RegisterTool(tool)
	_ = manager.RegisterProvider(prov)
	srv := NewServer(tools.NewToolEngine(manager), manager, "")

	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Zgi-User-Id", "user-1")
	req.Header.Set("X-Zgi-Tenant-Id", "tenant-1")
	req.Header.Set("X-Zgi-Conversation-Id", "conv-1")
	req.Header.Set("X-Zgi-Agent-Id", "agent-1")
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}

	rec.mu.Lock()
	defer rec.mu.Unlock()
	if rec.userID != "user-1" {
		t.Fatalf("userID = %q, want user-1", rec.userID)
	}
	if rec.convID != "conv-1" {
		t.Fatalf("conversationID = %q, want conv-1", rec.convID)
	}
	if rec.appID != "agent-1" {
		t.Fatalf("appID = %q, want agent-1", rec.appID)
	}
}

func TestMCPAuthRequired(t *testing.T) {
	srv, _ := newTestServer("secret-key")
	// No key header.
	w := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status without key = %d, want 401", w.Code)
	}
	// Wrong key.
	w = post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "wrong")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status with wrong key = %d, want 401", w.Code)
	}
	// Correct key.
	w = post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "secret-key")
	if w.Code != http.StatusOK {
		t.Fatalf("status with key = %d, want 200", w.Code)
	}
}
