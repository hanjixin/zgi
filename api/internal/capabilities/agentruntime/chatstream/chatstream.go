// Package chatstream provides the unified conversation/message persistence and
// SSE message envelope around a Driver.ChatStream call. It makes every agent
// runtime (codex/claude via the real Agent CLI, and later the business runtime)
// write its conversation + message history to the chat runtime tables the same
// way, so a reloaded console can replay history and disconnected runs leave a
// recoverable record — while live SSE streaming stays intact.
package chatstream

import (
	"context"
	"encoding/json"
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
	var presentationSeq int64
	var streamSeq int64
	var timeline []map[string]interface{}
	result, err := driver.ChatStream(ctx, req,
		func(chunk string) error {
			if chunk == "" {
				return nil
			}
			answer.WriteString(chunk)
			presentationSeq++
			streamSeq++
			now := time.Now()
			textID := fmt.Sprintf("message:%s:text:%d", messageID.String(), presentationSeq)
			eventID := fmt.Sprintf("%d-%d", now.UnixMilli(), streamSeq)
			return writer.WriteEvent(eventID, "message", map[string]interface{}{
				"answer":                 answer.String(),
				"content_phase":          "provisional",
				"conversation_id":        conversation.ID.String(),
				"created_at":             now.Unix(),
				"created_at_ms":          now.UnixMilli(),
				"event_id":               eventID,
				"message_id":             messageID.String(),
				"presentation_id":        textID,
				"presentation_sequence":  presentationSeq,
				"presentation_version":   2,
				"segment_content":        chunk,
				"segment_id":             textID,
				"sequence":               0,
			})
		},
		func(evt agentruntime.StreamEvent) error {
			// Enrich intermediate events with the conversation/message ids so the
			// console timeline can attach them to the streaming message (the CLI
			// driver payloads carry only tool-specific fields).
			payload := evt.Payload
			if isTimelineEvent(evt.EventType) {
				payload = enrichTimelinePayload(evt.Payload, conversation.ID, messageID)
			}
			if item := timelineItemFromEvent(evt.EventType, payload, evt.CreatedAt); item != nil {
				timeline = append(timeline, item)
			}
			return writer.WriteEvent(evt.ID.String(), evt.EventType, payload)
		},
	)

	status := runtimemodel.MessageStatusCompleted
	meta := map[string]interface{}{}
	// Persist the intermediate agent events (command_logged / file_change_logged
	// / skill_call_*) alongside the answer so a reloaded console can replay the
	// timeline instead of only showing the final message.
	messageMeta := map[string]interface{}{}
	if len(timeline) > 0 {
		messageMeta[timelineMetadataKey] = timeline
	}
	if err != nil {
		status = runtimemodel.MessageStatusError
		meta["error"] = err.Error()
		messageMeta["error"] = err.Error()
		_ = ic.repos.Message.UpdateError(ctx, messageID, err.Error())
	} else {
		_ = ic.repos.Message.UpdateCompleted(ctx, messageID, answer.String(), messageMeta)
	}
	if result != nil {
		meta["stream_event_count"] = result.StreamEventCount
	}
	if err := ic.repos.Conversation.FinishActiveMessage(ctx, conversation.ID, messageID); err != nil {
		return nil, err
	}
	streamSeq++
	_ = writer.WriteEvent(fmt.Sprintf("%d-%d", time.Now().UnixMilli(), streamSeq), "message_end", map[string]interface{}{
		"conversation_id": conversation.ID.String(),
		"message_id":      messageID.String(),
		"answer":          answer.String(),
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

// timelineMetadataKey stores the agent's intermediate event timeline in the
// message metadata so the console can replay command/file/skill events after a
// reload. Kept separate from the business runtime's "runtime_timeline" format
// to avoid coupling to its invocation model.
const timelineMetadataKey = "agent_events"

// isTimelineEvent reports whether an event should be persisted to the message
// timeline metadata (and enriched with conversation/message ids for the SSE).
func isTimelineEvent(eventType string) bool {
	switch eventType {
	case "command_logged", "file_change_logged", "skill_call_start", "skill_call_end", "skill_call_error":
		return true
	}
	return false
}

// enrichTimelinePayload injects conversation_id / message_id into an
// intermediate event payload so the frontend timeline can attach it to the
// streaming message.
func enrichTimelinePayload(raw json.RawMessage, conversationID, messageID uuid.UUID) json.RawMessage {
	var payload map[string]interface{}
	if len(raw) == 0 || json.Unmarshal(raw, &payload) != nil {
		payload = map[string]interface{}{}
	}
	payload["conversation_id"] = conversationID.String()
	payload["message_id"] = messageID.String()
	enriched, err := json.Marshal(payload)
	if err != nil {
		return raw
	}
	return enriched
}

// timelineItemFromEvent converts an enriched timeline event into a compact
// metadata record, or nil for events that should not be persisted.
func timelineItemFromEvent(eventType string, payload json.RawMessage, createdAt time.Time) map[string]interface{} {
	if !isTimelineEvent(eventType) {
		return nil
	}
	var data map[string]interface{}
	if len(payload) == 0 || json.Unmarshal(payload, &data) != nil {
		data = map[string]interface{}{}
	}
	item := map[string]interface{}{"type": eventType, "created_at": createdAt.Unix()}
	for k, v := range data {
		item[k] = v
	}
	return item
}
