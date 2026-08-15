package chatstream

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	agentruntime "github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	runtimemodel "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/model"
)

// createAssistantMessage persists the assistant message for this turn with a
// streaming status so the console timeline has a durable record even before the
// run finishes. The message id is supplied by the caller (also used as the SSE
// message_id).
func (ic *Interceptor) createAssistantMessage(ctx context.Context, rc RunContext, conversationID uuid.UUID, req agentruntime.ChatRequest, messageID uuid.UUID) error {
	modelProvider := rc.ModelProvider
	if modelProvider == "" {
		modelProvider = req.ModelProvider
	}
	message := &runtimemodel.Message{
		ID:                  messageID,
		ConversationID:      conversationID,
		Query:               rc.Query,
		Answer:              "",
		Status:              runtimemodel.MessageStatusStreaming,
		ModelProvider:       strPtr(modelProvider),
		ModelName:           orDefault(rc.ModelName, req.ModelName),
		BillingReasonSource: strPtr(runtimemodel.MessageBillingReasonSourceAIChat),
		ModelParameters:     map[string]interface{}{},
		Metadata:            map[string]interface{}{},
		CreatedAt:           time.Now(),
		UpdatedAt:           time.Now(),
	}
	if err := ic.repos.Message.Create(ctx, message); err != nil {
		return fmt.Errorf("create chat message: %w", err)
	}
	return nil
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
