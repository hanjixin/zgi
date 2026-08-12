// Package mcpbridge exposes ZGI's builtin tools (tools.ToolEngine) as an MCP
// (Model Context Protocol) HTTP server, so the real Agent CLIs (Claude Code /
// Codex) can call ZGI tools and skills through the agent-runner's mcp_servers.
//
// Transport: MCP "Streamable HTTP" — a single POST per JSON-RPC message. The
// server is stateless, so no session ids are required.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/zgiai/zgi/api/internal/modules/tools"
)

const (
	// SupportedProtocolVersion is echoed back during initialize.
	SupportedProtocolVersion = "2025-06-18"
	serverName               = "zgi-tools"
	serverVersion            = "0.1.0"

	// JSON-RPC error codes (MCP spec).
	errParse      = -32700
	errMethod     = -32601
	errInvalidReq = -32602
	errInternal   = -32603
)

// Server serves ZGI builtin tools over MCP.
type Server struct {
	engine  *tools.ToolEngine
	manager *tools.ToolManager
	apiKey  string // optional shared key; empty disables auth
}

func NewServer(engine *tools.ToolEngine, manager *tools.ToolManager, apiKey string) *Server {
	return &Server{engine: engine, manager: manager, apiKey: apiKey}
}

// Handler returns the MCP HTTP handler.
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.ServeHTTP) }

// ServeHTTP handles a single MCP request (Streamable HTTP transport).
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Streamable HTTP session handshake: clients (notably the Codex CLI) expect
	// an Mcp-Session-Id header; echo an existing one or mint a fresh id. The
	// server stays stateless but honours the header for client compatibility.
	sessionID := strings.TrimSpace(r.Header.Get("Mcp-Session-Id"))
	if sessionID == "" {
		sessionID = uuid.NewString()
	}
	w.Header().Set("Mcp-Session-Id", sessionID)

	if r.Method == http.MethodGet {
		// GET is used by some clients to probe the server / open an SSE stream.
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"name":     serverName,
			"version":  serverVersion,
			"protocol": map[string]interface{}{"version": SupportedProtocolVersion},
		})
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, rpcErrorJSON(nil, errInvalidReq, "method not allowed"))
		return
	}
	if !s.authorized(r) {
		writeJSON(w, http.StatusUnauthorized, rpcErrorJSON(nil, errInvalidReq, "invalid or missing X-MCP-API-Key"))
		return
	}

	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, rpcErrorJSON(nil, errParse, "invalid JSON-RPC body"))
		return
	}

	// Notifications carry no id and get no response.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		s.handleNotification(req.Method)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result, rpcErr := s.handle(r.Context(), req.Method, req.Params)
	if rpcErr != nil {
		writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: rpcErr})
		return
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result})
}

func (s *Server) authorized(r *http.Request) bool {
	if s.apiKey == "" {
		return true
	}
	got := strings.TrimSpace(r.Header.Get("X-MCP-API-Key"))
	return got != "" && got == s.apiKey
}

func (s *Server) handleNotification(method string) {
	// notifications/initialized and others need no action for a stateless server.
	_ = method
}

func (s *Server) handle(ctx context.Context, method string, params json.RawMessage) (interface{}, *rpcError) {
	switch method {
	case "initialize":
		// Echo the client's requested protocol version when we can; some clients
		// (e.g. the Codex CLI) reject a server that forces a newer version.
		version := SupportedProtocolVersion
		var init struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if len(params) > 0 && json.Unmarshal(params, &init) == nil && init.ProtocolVersion != "" {
			version = init.ProtocolVersion
		}
		return map[string]interface{}{
			"protocolVersion": version,
			"capabilities": map[string]interface{}{
				"tools": map[string]interface{}{"listChanged": false},
			},
			"serverInfo": map[string]interface{}{"name": serverName, "version": serverVersion},
		}, nil
	case "ping":
		return map[string]interface{}{}, nil
	case "tools/list":
		toolsList, err := s.listTools(ctx)
		if err != nil {
			return nil, &rpcError{Code: errInternal, Message: err.Error()}
		}
		return map[string]interface{}{"tools": toolsList}, nil
	case "tools/call":
		return s.callTool(ctx, params)
	default:
		return nil, &rpcError{Code: errMethod, Message: "method not found: " + method}
	}
}

func (s *Server) listTools(ctx context.Context) ([]map[string]interface{}, error) {
	if s.manager == nil {
		return nil, errors.New("tool manager not configured")
	}
	providers := s.manager.ListProviders(tools.ToolProviderTypeBuiltin)
	out := make([]map[string]interface{}, 0, len(providers))
	for _, p := range providers {
		entity := p.GetEntity()
		for _, te := range entity.Tools {
			name := te.Identity.Name
			if name == "" {
				continue
			}
			desc := te.Description.LLM
			if desc == "" {
				desc = te.Description.Human.Get("en_US")
			}
			out = append(out, map[string]interface{}{
				"name":        name,
				"description": desc,
				"inputSchema": buildInputSchema(te),
			})
		}
	}
	return out, nil
}

func (s *Server) callTool(ctx context.Context, params json.RawMessage) (interface{}, *rpcError) {
	var call struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	}
	if err := json.Unmarshal(params, &call); err != nil || call.Name == "" {
		return nil, &rpcError{Code: errInvalidReq, Message: "tools/call requires name and arguments"}
	}
	if s.engine == nil {
		return nil, &rpcError{Code: errInternal, Message: "tool engine not configured"}
	}
	if call.Arguments == nil {
		call.Arguments = map[string]interface{}{}
	}
	result, err := s.engine.Invoke(ctx, tools.InvokeRequest{
		ProviderType: tools.ToolProviderTypeBuiltin,
		ToolName:     call.Name,
		Parameters:   call.Arguments,
		InvokeFrom:   tools.ToolInvokeFromAgent,
	})
	if err != nil {
		return mcpToolResult("", err.Error(), true), nil
	}
	if result.Error != "" {
		return mcpToolResult("", result.Error, true), nil
	}
	text := ""
	for _, msg := range result.Messages {
		if msg.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += msg.Text
		} else if msg.Data != nil {
			if data, err := json.Marshal(msg.Data); err == nil {
				if text != "" {
					text += "\n"
				}
				text += string(data)
			}
		}
	}
	return mcpToolResult(text, "", false), nil
}

func mcpToolResult(text, errorMsg string, isErr bool) map[string]interface{} {
	content := []map[string]interface{}{}
	if text != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": text})
	}
	if errorMsg != "" {
		content = append(content, map[string]interface{}{"type": "text", "text": errorMsg})
	}
	if len(content) == 0 {
		content = append(content, map[string]interface{}{"type": "text", "text": ""})
	}
	return map[string]interface{}{"content": content, "isError": isErr}
}

func buildInputSchema(te tools.ToolEntity) map[string]interface{} {
	props := map[string]interface{}{}
	required := []string{}
	for _, p := range te.Parameters {
		prop := map[string]interface{}{"type": mcpParamType(p.Type)}
		desc := p.LLMDescription
		if desc == "" {
			desc = p.HumanDescription.Get("en_US")
		}
		if desc != "" {
			prop["description"] = desc
		}
		if len(p.Options) > 0 {
			enum := make([]string, 0, len(p.Options))
			for _, o := range p.Options {
				if o.Value != "" {
					enum = append(enum, o.Value)
				}
			}
			if len(enum) > 0 {
				prop["enum"] = enum
			}
		}
		props[p.Name] = prop
		if p.Required {
			required = append(required, p.Name)
		}
	}
	schema := map[string]interface{}{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func mcpParamType(t tools.ToolParameterType) string {
	switch t {
	case tools.ToolParameterTypeNumber:
		return "number"
	case tools.ToolParameterTypeBoolean:
		return "boolean"
	default: // string, select, file
		return "string"
	}
}

// ---- JSON-RPC wire types ----

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  interface{}     `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

func rpcErrorJSON(id json.RawMessage, code int, message string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: message}}
}

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
