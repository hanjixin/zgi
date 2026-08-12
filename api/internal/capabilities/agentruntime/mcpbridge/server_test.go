package mcpbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/zgiai/zgi/api/internal/modules/tools"
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
