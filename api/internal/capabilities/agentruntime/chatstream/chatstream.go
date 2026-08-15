// Package chatstream provides the unified conversation/message persistence and
// SSE message envelope around a Driver.ChatStream call. It makes every agent
// runtime (codex/claude via the real Agent CLI, and later the business runtime)
// write its conversation + message history to the chat runtime tables the same
// way, so a reloaded console can replay history and disconnected runs leave a
// recoverable record — while live SSE streaming stays intact.
package chatstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	agentruntime "github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	runtimemodel "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/model"
	runtimerepo "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/repository"
)

// ErrPreDispatch marks a failure that happened before message_start was
// emitted (conversation/message creation). Callers may fall back to another
// runtime because nothing was streamed yet. Errors after dispatch are surfaced
// through the SSE message_end(status=error) and the caller owns the response.
var ErrPreDispatch = errors.New("chatstream: failed before stream dispatch")

// StreamWriter writes SSE frames to the agent stream. It is implemented by the
// handler's agentSSEWriter (wrapping writeAgentSSERaw).
type StreamWriter interface {
	WriteEvent(id, event string, data interface{}) error
}

// RunContext carries the persistence scope for a managed chat turn.
type RunContext struct {
	OrganizationID uuid.UUID
	WorkspaceID    *uuid.UUID
	AccountID      uuid.UUID
	AgentID        uuid.UUID
	// CallerType / CallerID identify the conversation owner (e.g.
	// runtimemodel.ConversationCallerAgent with the agent id).
	CallerType string
	CallerID   *uuid.UUID
	// Source is runtimemodel.ConversationSourceConsole for the console.
	Source        string
	ModelName     string
	ModelProvider string
	Query         string
}

// Interceptor orchestrates conversation/message persistence and the SSE
// message envelope around a Driver.ChatStream call. Drivers that implement
// agentruntime.SelfPersistingDriver manage their own records; the interceptor
// then only relays the SSE envelope.
type Interceptor struct {
	repos *runtimerepo.Repositories
}

// NewInterceptor creates a chatstream interceptor over the given repositories.
func NewInterceptor(repos *runtimerepo.Repositories) *Interceptor {
	return &Interceptor{repos: repos}
}

// Run executes a managed chat turn:
//
//  1. ensure the conversation exists (create when new),
//  2. create the assistant message with status=streaming,
//  3. emit message_start (anchored to the persisted message id),
//  4. stream the driver output while updating the message,
//  5. finalize the message (completed / error / stopped),
//  6. emit message_end.
//
// The driver's onChunk and onEvent callbacks are wired so text chunks update
// the message answer and normalized events flow straight through to SSE.
func (ic *Interceptor) Run(
	ctx context.Context,
	driver agentruntime.Driver,
	rc RunContext,
	req agentruntime.ChatRequest,
	writer StreamWriter,
) (*agentruntime.ChatResponse, error) {
	conversation, err := ic.ensureConversation(ctx, rc, req)
	if err != nil {
		return nil, fmt.Errorf("%w: ensure conversation: %v", ErrPreDispatch, err)
	}
	messageID := uuid.New()
	// Anchor every stream event of this turn to the persisted message so the
	// console timeline can associate skill_call_* / message events with it.
	req.MessageID = messageID
	// Resolve the conversation id once so message_start/message/message_end
	// share the same value (the previous bug generated a fresh uuid per event).
	req.ConversationID = &conversation.ID

	if err := ic.createAssistantMessage(ctx, rc, conversation.ID, req, messageID); err != nil {
		return nil, fmt.Errorf("%w: create message: %v", ErrPreDispatch, err)
	}
	if err := ic.repos.Conversation.StartStreaming(ctx, conversation.ID, rc.OrganizationID, rc.AccountID, messageID); err != nil {
		return nil, fmt.Errorf("%w: start streaming: %v", ErrPreDispatch, err)
	}

	_ = writer.WriteEvent(uuid.NewString(), "message_start", map[string]interface{}{
		"conversation_id": conversation.ID.String(),
		"message_id":      messageID.String(),
		"model":           rc.ModelName,
		"created_at":      time.Now().Unix(),
	})

	var answer strings.Builder
	result, err := driver.ChatStream(ctx, req,
		func(chunk string) error {
			if chunk == "" {
				return nil
			}
			answer.WriteString(chunk)
			return writer.WriteEvent("", "message", map[string]interface{}{
				"conversation_id": conversation.ID.String(),
				"message_id":      messageID.String(),
				"answer":          chunk,
			})
		},
		func(evt agentruntime.StreamEvent) error {
			return writer.WriteEvent(evt.ID.String(), evt.EventType, evt.Payload)
		},
	)

	status := runtimemodel.MessageStatusCompleted
	meta := map[string]interface{}{}
	if err != nil {
		status = runtimemodel.MessageStatusError
		meta["error"] = err.Error()
		_ = ic.repos.Message.UpdateError(ctx, messageID, err.Error())
	} else {
		_ = ic.repos.Message.UpdateCompleted(ctx, messageID, answer.String(), map[string]interface{}{})
	}
	if result != nil {
		meta["stream_event_count"] = result.StreamEventCount
	}
	if err := ic.repos.Conversation.FinishActiveMessage(ctx, conversation.ID, messageID); err != nil {
		return nil, err
	}
	_ = writer.WriteEvent("", "message_end", map[string]interface{}{
		"conversation_id": conversation.ID.String(),
		"message_id":      messageID.String(),
		"status":          status,
		"metadata":        meta,
	})

	if result == nil {
		result = &agentruntime.ChatResponse{MessageID: messageID, ConversationID: conversation.ID}
	}
	result.MessageID = messageID
	result.ConversationID = conversation.ID
	result.Answer = answer.String()
	result.Status = status
	return result, err
}
