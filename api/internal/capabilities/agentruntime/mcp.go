package agentruntime

// McpServerConfig is one MCP server exposed to an Agent CLI (stdio/http/sse).
// It is shared by the control-plane ChatRequest and the runner transport so an
// agent's configured MCP servers flow through to the real Claude Code / Codex.
type McpServerConfig struct {
	Name    string            `json:"name"`
	Type    string            `json:"type"` // stdio|http|sse
	URL     string            `json:"url,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}
