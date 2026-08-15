package agents

import (
	"errors"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	runtimedto "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/dto"
	runtimemodel "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/model"
	runtimerepo "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/repository"
	runtimeservice "github.com/zgiai/zgi/api/internal/capabilities/chatruntime/service"
	"github.com/zgiai/zgi/api/pkg/logger"
	"github.com/zgiai/zgi/api/pkg/response"
)

func (h *AgentsHandler) regenerateRuntimeMessage(c *gin.Context, runtimeCtx agentRuntimeContext) {
	messageID, ok := uuidParam(c, "message_id")
	if !ok {
		return
	}
	var req runtimedto.RegenerateMessageRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Fail(c, response.ErrInvalidParam)
		return
	}
	// Real agent runtimes (codex/claude) regenerate through their own driver,
	// resuming the CLI session so the new answer replaces the old message.
	if dispatched, err := h.tryRegenerateToCodex(c, runtimeCtx, messageID, req); err != nil {
		logger.WarnContext(c.Request.Context(), "agent runtime regen route failed, falling back to business runtime", "message_id", messageID, "error", err)
	} else if dispatched {
		return
	}
	prepared, err := h.chatRuntimeService.PrepareConfiguredRootRegeneration(c.Request.Context(), runtimeCtx.Scope, runtimeCtx.Caller, runtimeCtx.RunConfig, messageID, req)
	if err != nil {
		h.failRuntime(c, err)
		return
	}
	h.runPreparedAgentStream(c, prepared)
}

// tryRegenerateToCodex re-runs a codex/claude turn for a regenerated message,
// resuming the CLI session so the new answer replaces the old one. Returns
// (true, nil) when handled, (false, err) when the caller should fall back to
// the business runtime.
func (h *AgentsHandler) tryRegenerateToCodex(c *gin.Context, runtimeCtx agentRuntimeContext, messageID uuid.UUID, req runtimedto.RegenerateMessageRequest) (bool, error) {
	if h.runtimeRouter == nil || h.db == nil || runtimeCtx.Caller.ID == nil {
		return false, nil
	}
	ctx := c.Request.Context()
	agent, err := h.findAgentByID(ctx, *runtimeCtx.Caller.ID)
	if err != nil || agent == nil {
		return false, err
	}
	if agent.RuntimeType != string(agentruntime.RuntimeTypeCodex) &&
		agent.RuntimeType != string(agentruntime.RuntimeTypeClaudeCode) {
		return false, nil
	}
	original, err := runtimerepo.NewRepositories(h.db).Message.GetScoped(ctx, messageID, runtimeCtx.Scope.OrganizationID, runtimeCtx.Scope.AccountID)
	if err != nil || original == nil {
		return false, err
	}
	descriptor := agentruntime.AgentDescriptor{
		ID:            agent.ID,
		TenantID:      agent.TenantID,
		RuntimeType:   agentruntime.RuntimeType(agent.RuntimeType),
		RuntimeConfig: parseRuntimeConfig(agent.RuntimeConfig),
	}
	driver := h.runtimeRouter.Route(ctx, descriptor)
	if driver == nil {
		return false, nil
	}
	runtimeCfg := descriptor.RuntimeConfig
	prompt := original.Query
	if req.Query != nil && strings.TrimSpace(*req.Query) != "" {
		prompt = *req.Query
	}
	chatReq := agentruntime.ChatRequest{
		AgentID:         agent.ID,
		UserID:          runtimeCtx.Scope.AccountID,
		TenantID:        runtimeCtx.Scope.OrganizationID,
		UserMessage:     prompt,
		SystemPrompt:    runtimeStringConfig(runtimeCfg, "system_prompt", "systemPrompt"),
		ModelName:       runtimeStringConfig(runtimeCfg, "model_name", "model", "model_id"),
		McpServers:      parseMcpServersFromConfig(runtimeCfg),
		EnabledSkillIDs: parseEnabledSkillIDs(runtimeCfg),
		ConversationID:  &original.ConversationID,
	}
	if history := h.buildConversationHistory(ctx, original.ConversationID, runtimeCtx.Scope.OrganizationID, runtimeCtx.Scope.AccountID); history != "" {
		if chatReq.SystemPrompt != "" {
			chatReq.SystemPrompt += "\n\n"
		}
		chatReq.SystemPrompt += "## Previous conversation\n\n" + history
	}
	messageIDNew := uuid.New()
	chatReq.MessageID = messageIDNew
	setupAgentSSE(c)
	startEvt := agentruntime.StreamEvent{
		ID:        uuid.New(),
		EventType: "message_start",
		Payload: mustMarshalJSON(map[string]interface{}{
			"conversation_id": chatReq.ConversationIDOrDefault(),
			"message_id":      messageIDNew.String(),
			"model":           chatReq.ModelName,
			"created_at":      timeNow().Unix(),
		}),
		CreatedAt: timeNow(),
	}
	_ = writeAgentSSEEvent(c, startEvt.ID.String(), startEvt.EventType, startEvt.Payload)

	result, err := driver.ChatStream(ctx, chatReq,
		func(chunk string) error {
			return writeAgentSSE(c, "message", gin.H{
				"conversation_id": chatReq.ConversationIDOrDefault(),
				"message_id":      messageIDNew.String(),
				"answer":          chunk,
			})
		},
		func(evt agentruntime.StreamEvent) error {
			return writeAgentSSEEvent(c, evt.ID.String(), evt.EventType, evt.Payload)
		},
	)
	if err != nil {
		writeAgentSSE(c, "message_end", gin.H{
			"conversation_id": chatReq.ConversationIDOrDefault(),
			"status":          "error",
			"metadata":        gin.H{"error": err.Error()},
		})
		return true, err
	}
	if result != nil {
		writeAgentSSE(c, "message_end", gin.H{
			"conversation_id": result.ConversationID,
			"message_id":      result.MessageID,
			"status":          result.Status,
			"metadata":        gin.H{"stream_event_count": result.StreamEventCount},
		})
	}
	return true, nil
}

func (h *AgentsHandler) runPreparedAgentStream(c *gin.Context, prepared *runtimeservice.PreparedChat) {
	setupAgentSSE(c)
	_ = writeAgentSSE(c, "message_start", gin.H{
		"conversation_id": prepared.Conversation.ID.String(),
		"message_id":      prepared.Message.ID.String(),
		"parent_id":       uuidPtrToString(prepared.Message.ParentID),
		"title":           prepared.Conversation.Title,
		"model":           prepared.Message.ModelName,
		"replace":         prepared.ReplaceRoot,
		"created_at":      prepared.Message.CreatedAt.Unix(),
	})
	result, err := h.chatRuntimeService.RunPreparedStream(c.Request.Context(), prepared, func(chunk string) error {
		return writeAgentSSE(c, "message", gin.H{
			"conversation_id": prepared.Conversation.ID.String(),
			"message_id":      prepared.Message.ID.String(),
			"answer":          chunk,
		})
	}, func(event runtimeservice.StreamEvent) error {
		return writeAgentSSEEvent(c, event.ID, event.EventType, event.Payload)
	})
	if err != nil {
		status := runtimemodel.MessageStatusError
		if errors.Is(err, runtimeservice.ErrMessageStopped) {
			status = runtimemodel.MessageStatusStopped
		}
		if runtimeservice.IsFinalizedStreamError(err) {
			return
		}
		_ = writeAgentSSE(c, "error", runtimeservice.BuildStreamErrorPayload(prepared, err))
		_ = writeAgentSSE(c, "message_end", gin.H{
			"conversation_id": prepared.Conversation.ID.String(),
			"message_id":      prepared.Message.ID.String(),
			"status":          status,
			"metadata":        gin.H{},
		})
		return
	}
	writeAgentChatEnd(c, prepared, result)
}
