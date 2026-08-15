package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

type RuntimeType string

const (
	RuntimeTypeBusiness   RuntimeType = "business"
	RuntimeTypeCodex      RuntimeType = "codex"
	RuntimeTypeClaudeCode RuntimeType = "claude-code"
)

func NormalizeRuntimeType(t RuntimeType) RuntimeType {
	if t == "" {
		return RuntimeTypeBusiness
	}
	switch t {
	case RuntimeTypeBusiness, RuntimeTypeCodex, RuntimeTypeClaudeCode:
		return t
	default:
		return RuntimeTypeBusiness
	}
}

func IsValidRuntimeType(t RuntimeType) bool {
	switch t {
	case RuntimeTypeBusiness, RuntimeTypeCodex, RuntimeTypeClaudeCode:
		return true
	default:
		return false
	}
}

var (
	ErrRuntimeDisabled     = errors.New("codex runtime is disabled by feature flag")
	ErrUnsupportedRuntime  = errors.New("unsupported runtime_type")
	ErrDriverNotConfigured = errors.New("runtime driver is not configured")
	ErrSessionNotFound     = errors.New("runtime session not found")
)

type ChatRequest struct {
	AgentID         uuid.UUID       `json:"agent_id"`
	ConversationID  *uuid.UUID      `json:"conversation_id,omitempty"`
	// MessageID anchors every stream event of this turn to one message so the
	// console timeline can associate skill_call_* / message events with it.
	MessageID       uuid.UUID       `json:"message_id,omitempty"`
	UserID          uuid.UUID       `json:"user_id"`
	TenantID        uuid.UUID       `json:"tenant_id"`
	WorkspaceID     *uuid.UUID      `json:"workspace_id,omitempty"`
	UserMessage     string          `json:"user_message"`
	SystemPrompt    string          `json:"system_prompt,omitempty"`
	ModelProvider   string          `json:"model_provider,omitempty"`
	ModelName       string          `json:"model_name,omitempty"`
	ModelParameters json.RawMessage `json:"model_parameters,omitempty"`
	McpServers      []McpServerConfig `json:"mcp_servers,omitempty"`
	Metadata        json.RawMessage `json:"metadata,omitempty"`
}

type StreamEvent struct {
	ID        uuid.UUID       `json:"id"`
	EventType string          `json:"event_type"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
}

type ChatResponse struct {
	MessageID        uuid.UUID       `json:"message_id"`
	ConversationID   uuid.UUID       `json:"conversation_id"`
	Answer           string          `json:"answer"`
	Status           string          `json:"status"`
	Metadata         json.RawMessage `json:"metadata,omitempty"`
	StreamEventCount int             `json:"stream_event_count"`
	DurationMS       int64           `json:"duration_ms"`
}

type StopRequest struct {
	AgentID        uuid.UUID `json:"agent_id"`
	ConversationID uuid.UUID `json:"conversation_id"`
	UserID         uuid.UUID `json:"user_id"`
	Reason         string    `json:"reason,omitempty"`
}

type SessionState struct {
	ID            uuid.UUID       `json:"id"`
	AgentID       uuid.UUID       `json:"agent_id"`
	ConversationID uuid.UUID       `json:"conversation_id"`
	RuntimeType   RuntimeType     `json:"runtime_type"`
	Status        string          `json:"status"`
	State         json.RawMessage `json:"state,omitempty"`
	LastCheckpoint json.RawMessage `json:"last_checkpoint,omitempty"`
	LastActiveAt  *time.Time      `json:"last_active_at,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type Driver interface {
	RuntimeType() RuntimeType
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	ChatStream(ctx context.Context, req ChatRequest, onChunk func(chunk string) error, onEvent func(StreamEvent) error) (*ChatResponse, error)
	Stop(ctx context.Context, req StopRequest) error
	LoadSession(ctx context.Context, agentID, conversationID uuid.UUID) (*SessionState, error)
	SaveSession(ctx context.Context, state *SessionState) error
}

func (r ChatRequest) ConversationIDOrDefault() uuid.UUID {
	if r.ConversationID != nil {
		return *r.ConversationID
	}
	return uuid.New()
}
