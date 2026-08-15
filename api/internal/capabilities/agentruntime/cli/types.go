package cli

import (
	"encoding/json"

	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
)

// Event type strings emitted to the frontend SSE stream. They match the event
// names the console already handles for coding agents.
const (
	EventMessageStart      = "message_start"
	EventMessageChunk      = "message"
	EventMessageEnd        = "message_end"
	EventSkillCallStart    = "skill_call_start"
	EventSkillCallEnd      = "skill_call_end"
	EventApprovalRequired  = "approval_required"
	EventCommandLogged     = "command_logged"
	EventFileChangeLogged  = "file_change_logged"
)

// RunRequest is the body sent to the agent-runner POST /v1/agents/run.
type RunRequest struct {
	AgentType       string            `json:"agent_type"`
	SessionID       string            `json:"session_id,omitempty"`
	Prompt          string            `json:"prompt"`
	Cwd             string            `json:"cwd"`
	Env             map[string]string `json:"env,omitempty"`
	Model           string            `json:"model,omitempty"`
	SystemPrompt    string            `json:"system_prompt,omitempty"`
	AllowedTools    []string          `json:"allowed_tools,omitempty"`
	DisallowedTools []string          `json:"disallowed_tools,omitempty"`
	PermissionMode  string            `json:"permission_mode,omitempty"`
	ApprovalPolicy  string            `json:"approval_policy,omitempty"`
	SandboxMode     string            `json:"sandbox_mode,omitempty"`
	Resume          string                     `json:"resume,omitempty"`
	AskTimeoutMS    int                        `json:"ask_timeout_ms,omitempty"`
	McpServers      []agentruntime.McpServerConfig `json:"mcp_servers,omitempty"`
	// GatewayURL is the ZGI LLM gateway base URL. When set, the runner points
	// codex/claude at the gateway instead of their external provider defaults.
	GatewayURL string `json:"gateway_url,omitempty"`
}

// PermissionRequest is the body for POST /v1/agents/:sid/permission.
type PermissionRequest struct {
	CorrelationID string `json:"correlation_id"`
	Decision      string `json:"decision"` // "approve" | "reject"
	Reason        string `json:"reason,omitempty"`
}

// RunnerEvent is one normalized event parsed from the runner SSE stream.
type RunnerEvent struct {
	Type string `json:"type"`

	SessionID      string          `json:"session_id,omitempty"`
	AgentSessionID string          `json:"agent_session_id,omitempty"`
	Text           string          `json:"text,omitempty"`
	ID             string          `json:"id,omitempty"`
	Tool           string          `json:"tool,omitempty"`
	Input          json.RawMessage `json:"input,omitempty"`
	Output         string          `json:"output,omitempty"`
	IsError        bool            `json:"is_error,omitempty"`
	Command        string          `json:"command,omitempty"`
	Status         string          `json:"status,omitempty"`
	ExitCode       *int            `json:"exit_code,omitempty"`
	Changes        json.RawMessage `json:"changes,omitempty"`
	CorrelationID  string          `json:"correlation_id,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	Subtype        string          `json:"subtype,omitempty"`
	Message        string          `json:"message,omitempty"`
	Denied         bool            `json:"denied,omitempty"`
	Usage          json.RawMessage `json:"usage,omitempty"`
	Cost           float64         `json:"cost,omitempty"`
}

// ToolCallStartPayload is the skill_call_start SSE payload. It carries the
// same fields as the console's AIChatSkillCallStartEventData so the frontend
// renders agent CLI tool calls on the timeline without changes (the frontend
// drops skill_call_start events missing conversation_id/message_id/skill_id).
type ToolCallStartPayload struct {
	ConversationID   string          `json:"conversation_id"`
	MessageID        string          `json:"message_id"`
	SkillID          string          `json:"skill_id"`
	ToolName         string          `json:"tool_name"`
	Arguments        json.RawMessage `json:"arguments,omitempty"`
	ArgumentsSummary json.RawMessage `json:"arguments_summary,omitempty"`
	Status           string          `json:"status"`
	CreatedAt        int64           `json:"created_at,omitempty"`
}

// ToolCallEndPayload is the skill_call_end SSE payload (same contract as
// AIChatSkillCallEndEventData).
type ToolCallEndPayload struct {
	ConversationID string          `json:"conversation_id"`
	MessageID      string          `json:"message_id"`
	SkillID        string          `json:"skill_id"`
	ToolName       string          `json:"tool_name"`
	Status         string          `json:"status"`
	DurationMS     int64           `json:"duration_ms,omitempty"`
	Message        string          `json:"message,omitempty"`
	Result         json.RawMessage `json:"result,omitempty"`
	CreatedAt      int64           `json:"created_at,omitempty"`
}

// ApprovalRequiredPayload mirrors the approval_required SSE payload.
type ApprovalRequiredPayload struct {
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments,omitempty"`
	Reason        string          `json:"reason"`
	CorrelationID string          `json:"correlation_id"`
}

// CommandLoggedPayload mirrors the command_logged SSE payload.
type CommandLoggedPayload struct {
	Command    string `json:"command"`
	ExitCode   int    `json:"exit_code,omitempty"`
	Stdout     string `json:"stdout,omitempty"`
	Stderr     string `json:"stderr,omitempty"`
}

// FileChangeLoggedPayload mirrors the file_change_logged SSE payload.
type FileChangeLoggedPayload struct {
	Path string `json:"path"`
	Kind string `json:"kind,omitempty"`
}
