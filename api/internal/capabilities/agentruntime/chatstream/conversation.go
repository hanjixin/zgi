package chatstream

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"

	agentruntime "github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	runtimemodel "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/model"
)

// ensureConversation returns the conversation for this turn, creating it when
// the request carries no conversation id or the referenced one is not found.
func (ic *Interceptor) ensureConversation(ctx context.Context, rc RunContext, req agentruntime.ChatRequest) (*runtimemodel.Conversation, error) {
	if req.ConversationID != nil {
		conversation, err := ic.repos.Conversation.GetScoped(ctx, *req.ConversationID, rc.OrganizationID, rc.AccountID)
		if err == nil {
			return conversation, nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, err
		}
	}
	conversationType := runtimemodel.ConversationTypeChat
	title := firstRunes(rc.Query, 80)
	if title == "" {
		title = "New conversation"
	}
	conversation := &runtimemodel.Conversation{
		ID:               uuid.New(),
		OrganizationID:   rc.OrganizationID,
		WorkspaceID:      rc.WorkspaceID,
		AccountID:        rc.AccountID,
		CallerType:       orDefault(rc.CallerType, runtimemodel.ConversationCallerAgent),
		CallerID:         rc.CallerID,
		ConversationType: conversationType,
		Title:            title,
		Status:           runtimemodel.ConversationStatusNormal,
		RuntimeStatus:    runtimemodel.ConversationRuntimeStatusIdle,
		Source:           orDefault(rc.Source, runtimemodel.ConversationSourceConsole),
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}
	if err := ic.repos.Conversation.Create(ctx, conversation); err != nil {
		return nil, fmt.Errorf("create chat conversation: %w", err)
	}
	return conversation, nil
}

func firstRunes(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func orDefault(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
