package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime"
	"github.com/zgiai/zgi/api/internal/capabilities/agentruntime/workspace"
)

// AgentType values understood by the agent-runner.
const (
	AgentTypeClaude = "claude"
	AgentTypeCodex  = "codex"
)

// Options configures a CliDriver instance (one per runtime type).
type Options struct {
	AgentType       string // "claude" | "codex"
	Enabled         bool
	RunnerURL       string
	Model           string
	PermissionMode  string // claude: default|acceptEdits|bypassPermissions|plan
	SandboxMode     string // codex: read-only|workspace-write|danger-full-access
	ApprovalPolicy  string // codex: never|on-request|on-failure|untrusted
	AllowedTools    []string
	DisallowedTools []string
	APIKey          string // ANTHROPIC_API_KEY for claude, OPENAI_API_KEY for codex
	WorkspaceRoot   string // root dir for agent workspaces (filesystem)
	AskTimeoutMS    int
	McpServers      []agentruntime.McpServerConfig // global MCP servers (per-agent ones come via ChatRequest)

	// LLMGatewayURL, when set, routes codex/claude model calls through the ZGI
	// LLM gateway (base URL, e.g. http://127.0.0.1:2670). The gateway key is
	// resolved per-organization via GatewayKeyResolver; usage meters through
	// that key's quota.
	LLMGatewayURL         string
	GatewayKeyResolver    agentruntime.GatewayKeyResolver

	WorkspaceSvc workspace.Service
	Governance   *agentruntime.GovernanceApprovalService
}

// CliDriver implements agentruntime.Driver by driving the real Agent CLI
// (Claude Code / Codex) through the agent-runner service.
type CliDriver struct {
	agentType string
	enabled   bool
	client    *RunnerClient
	opts      Options
}

func NewDriver(opts Options) *CliDriver {
	if opts.AskTimeoutMS <= 0 {
		opts.AskTimeoutMS = 300_000
	}
	return &CliDriver{
		agentType: opts.AgentType,
		enabled:   opts.Enabled,
		client:    NewRunnerClient(opts.RunnerURL),
		opts:      opts,
	}
}

func (d *CliDriver) RuntimeType() agentruntime.RuntimeType {
	if d.agentType == AgentTypeCodex {
		return agentruntime.RuntimeTypeCodex
	}
	return agentruntime.RuntimeTypeClaudeCode
}

func (d *CliDriver) Chat(ctx context.Context, req agentruntime.ChatRequest) (*agentruntime.ChatResponse, error) {
	if err := d.ensureEnabled(); err != nil {
		return nil, err
	}
	var finalAnswer strings.Builder
	result, err := d.ChatStream(ctx, req,
		func(chunk string) error {
			finalAnswer.WriteString(chunk)
			return nil
		},
		func(agentruntime.StreamEvent) error { return nil },
	)
	if err != nil {
		return nil, err
	}
	if finalAnswer.Len() > 0 {
		result.Answer = finalAnswer.String()
	}
	return result, nil
}

func (d *CliDriver) ChatStream(ctx context.Context, req agentruntime.ChatRequest, onChunk func(string) error, onEvent func(agentruntime.StreamEvent) error) (*agentruntime.ChatResponse, error) {
	if err := d.ensureEnabled(); err != nil {
		return nil, err
	}
	start := time.Now()
	sessionID := req.ConversationIDOrDefault()
	// Anchor all stream events of this turn to one message so the console
	// timeline can associate skill_call_* events with the streaming message.
	messageID := req.MessageID
	if messageID == uuid.Nil {
		messageID = uuid.New()
	}
	toolStartedAt := map[string]time.Time{} // runner tool call id -> start time
	cwd, err := d.ensureWorkspaceDir(ctx, req)
	if err != nil {
		return nil, err
	}

	// Resume a prior conversation when a runner session id was persisted.
	resume, _ := d.loadAgentSession(ctx, req.AgentID, sessionID)

	// When the LLM gateway is configured, resolve the organization's gateway
	// API key so codex/claude authenticate against the gateway (usage meters
	// through that key's quota) instead of their own external credentials.
	gatewayKey := ""
	if d.opts.LLMGatewayURL != "" {
		if d.opts.GatewayKeyResolver == nil {
			return nil, errors.New("llm gateway configured but gateway key resolver is missing")
		}
		var err error
		gatewayKey, err = d.opts.GatewayKeyResolver.ResolveGatewayKey(ctx, req.TenantID)
		if err != nil {
			return nil, fmt.Errorf("resolve llm gateway key: %w", err)
		}
	}

	runReq := RunRequest{
		AgentType:       d.agentType,
		SessionID:       sessionID.String(),
		Prompt:          req.UserMessage,
		Cwd:             cwd,
		Env:             d.buildEnv(req, gatewayKey),
		Model:           d.resolveModel(req),
		GatewayURL:      d.opts.LLMGatewayURL,
		SystemPrompt:    req.SystemPrompt,
		AllowedTools:    d.opts.AllowedTools,
		DisallowedTools: d.opts.DisallowedTools,
		PermissionMode:  d.opts.PermissionMode,
		SandboxMode:     d.opts.SandboxMode,
		ApprovalPolicy:  d.opts.ApprovalPolicy,
		Resume:          resume,
		AskTimeoutMS:    d.opts.AskTimeoutMS,
		McpServers:      d.resolveMcpServers(req),
	}

	stream, err := d.client.Run(ctx, runReq)
	if err != nil {
		return nil, err
	}
	defer stream.Close()

	agentSessionID := ""
	eventCount := 0
	terminalStatus := "completed"
	lastError := ""
	var answerBuilder strings.Builder

	for {
		evt, err := stream.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			terminalStatus = "error"
			lastError = err.Error()
			break
		}
		eventCount++
		switch evt.Type {
		case "session_started":
			agentSessionID = evt.AgentSessionID
		case "text":
			if evt.Text != "" {
				answerBuilder.WriteString(evt.Text)
				if onChunk != nil {
					_ = onChunk(evt.Text)
				}
			}
		case "tool_use":
			if evt.ID != "" {
				toolStartedAt[evt.ID] = time.Now()
			}
			d.emitEvent(onEvent, EventSkillCallStart, ToolCallStartPayload{
				ConversationID:   sessionID.String(),
				MessageID:        messageID.String(),
				SkillID:          evt.Tool,
				ToolName:         evt.Tool,
				Arguments:        evt.Input,
				ArgumentsSummary: evt.Input,
				Status:           "running",
				CreatedAt:        time.Now().Unix(),
			})
		case "tool_result":
			status := "success"
			if evt.IsError {
				status = "error"
			}
			var durationMS int64
			if evt.ID != "" {
				if started, ok := toolStartedAt[evt.ID]; ok {
					durationMS = time.Since(started).Milliseconds()
					delete(toolStartedAt, evt.ID)
				}
			}
			resultPayload := map[string]interface{}{
				"ok":        !evt.IsError,
				"tool":      evt.Tool,
				"output":    evt.Output,
				"is_error":  evt.IsError,
			}
			resultJSON, _ := json.Marshal(resultPayload)
			d.emitEvent(onEvent, EventSkillCallEnd, ToolCallEndPayload{
				ConversationID: sessionID.String(),
				MessageID:      messageID.String(),
				SkillID:        evt.Tool,
				ToolName:       evt.Tool,
				Status:         status,
				DurationMS:     durationMS,
				Result:         resultJSON,
				CreatedAt:      time.Now().Unix(),
			})
		case "command_exec":
			d.emitEvent(onEvent, EventCommandLogged, CommandLoggedPayload{
				Command:  evt.Command,
				ExitCode: intValue(evt.ExitCode),
				Stdout:   evt.Output,
			})
		case "file_change":
			kind := "update"
			if changes, ok := evtChangesKind(evt.Changes); ok {
				kind = changes
			}
			d.emitEvent(onEvent, EventFileChangeLogged, FileChangeLoggedPayload{Path: firstChangePath(evt.Changes), Kind: kind})
		case "permission_request":
			d.handlePermissionRequest(ctx, sessionID, evt, onEvent)
		case "permission_result":
			// Informational; surfaced through the approval flow already.
		case "status":
			// reserved
		case "done":
			switch evt.Subtype {
			case "error":
				terminalStatus = "error"
				lastError = evt.Message
			case "cancelled":
				terminalStatus = "cancelled"
			default:
				terminalStatus = "completed"
			}
		case "error":
			terminalStatus = "error"
			lastError = evt.Message
		default:
			// Unknown event type; ignore.
		}
	}

	// Persist the runner session id so a later turn can resume this conversation.
	if agentSessionID != "" {
		checkpoint, _ := json.Marshal(map[string]string{"agent_session_id": agentSessionID})
		_ = d.persistSnapshot(ctx, req.AgentID, sessionID, terminalStatus, checkpoint)
	}

	if lastError != "" {
		_ = onChunk("\n\n[agent error] " + lastError)
	}

	return &agentruntime.ChatResponse{
		MessageID:        uuid.New(),
		ConversationID:   sessionID,
		Answer:           answerBuilder.String(),
		Status:           terminalStatus,
		StreamEventCount: eventCount,
		DurationMS:       time.Since(start).Milliseconds(),
	}, nil
}

func (d *CliDriver) Stop(ctx context.Context, req agentruntime.StopRequest) error {
	if !d.enabled {
		return agentruntime.ErrRuntimeDisabled
	}
	if req.ConversationID == uuid.Nil {
		return errors.New("conversation id is required to stop a run")
	}
	return d.client.Stop(ctx, req.ConversationID.String())
}

func (d *CliDriver) LoadSession(ctx context.Context, agentID, conversationID uuid.UUID) (*agentruntime.SessionState, error) {
	if d.opts.WorkspaceSvc == nil {
		return nil, agentruntime.ErrSessionNotFound
	}
	snap, err := d.opts.WorkspaceSvc.LoadSessionSnapshot(ctx, conversationID)
	if err != nil {
		return nil, agentruntime.ErrSessionNotFound
	}
	now := time.Now()
	return &agentruntime.SessionState{
		ID:             conversationID,
		AgentID:        agentID,
		ConversationID: conversationID,
		RuntimeType:    d.RuntimeType(),
		Status:         snap.Status,
		State:          snap.RuntimeState,
		LastCheckpoint: snap.Checkpoint,
		LastActiveAt:   &now,
		CreatedAt:      now,
		UpdatedAt:      now,
	}, nil
}

func (d *CliDriver) SaveSession(ctx context.Context, state *agentruntime.SessionState) error {
	if state == nil {
		return errors.New("nil session state")
	}
	if d.opts.WorkspaceSvc == nil {
		return agentruntime.ErrDriverNotConfigured
	}
	return d.persistSnapshot(ctx, state.AgentID, state.ConversationID, state.Status, state.LastCheckpoint)
}

func (d *CliDriver) ensureEnabled() error {
	if !d.enabled {
		return agentruntime.ErrRuntimeDisabled
	}
	if d.opts.RunnerURL == "" {
		return fmt.Errorf("%w: agent runner url is not configured", agentruntime.ErrDriverNotConfigured)
	}
	return nil
}

// ensureWorkspaceDir returns a stable per-agent workspace directory, creating it
// if needed and seeding the Agent CLI's memory file. The runner executes the
// Agent CLI with this as its working dir.
func (d *CliDriver) ensureWorkspaceDir(ctx context.Context, req agentruntime.ChatRequest) (string, error) {
	root := d.opts.WorkspaceRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "zgi-agents")
	}
	dir := filepath.Join(root, req.AgentID.String())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create agent workspace: %w", err)
	}
	if err := d.seedMemoryFile(dir, req.SystemPrompt); err != nil {
		return "", err
	}
	return dir, nil
}

// seedMemoryFile writes the Agent CLI's project memory file (CLAUDE.md for
// Claude Code, AGENTS.md for Codex) when absent, so the agent starts with the
// runtime system prompt and any product-level instructions. Existing files are
// left untouched to preserve user/project memory.
func (d *CliDriver) seedMemoryFile(dir, systemPrompt string) error {
	fileName := "CLAUDE.md"
	if d.agentType == AgentTypeCodex {
		fileName = "AGENTS.md"
	}
	path := filepath.Join(dir, fileName)
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	content := "# ZGI Coding Agent\n\nOperate on this repository and follow the runtime instructions below.\n"
	if strings.TrimSpace(systemPrompt) != "" {
		content += "\n## Runtime Instructions\n\n" + strings.TrimSpace(systemPrompt) + "\n"
	}
	return os.WriteFile(path, []byte(content), 0o644)
}

// resolveMcpServers combines the globally configured MCP servers (e.g. the ZGI
// tools bridge) with the per-agent MCP servers from the chat request.
func (d *CliDriver) resolveMcpServers(req agentruntime.ChatRequest) []agentruntime.McpServerConfig {
	merged := append([]agentruntime.McpServerConfig{}, d.opts.McpServers...)
	merged = append(merged, req.McpServers...)
	// Stamp every MCP server with the caller identity so user-scoped ZGI tools
	// (memory, knowledge, ...) resolve the right account/tenant. The headers
	// ride the CLI's MCP http_headers to the ZGI mcpbridge.
	sessionID := req.ConversationIDOrDefault()
	for i := range merged {
		if merged[i].Headers == nil {
			merged[i].Headers = map[string]string{}
		}
		merged[i].Headers["X-Zgi-User-Id"] = req.UserID.String()
		merged[i].Headers["X-Zgi-Tenant-Id"] = req.TenantID.String()
		if req.WorkspaceID != nil {
			merged[i].Headers["X-Zgi-Workspace-Id"] = req.WorkspaceID.String()
		}
		merged[i].Headers["X-Zgi-Agent-Id"] = req.AgentID.String()
		merged[i].Headers["X-Zgi-Conversation-Id"] = sessionID.String()
		if len(req.EnabledSkillIDs) > 0 {
			merged[i].Headers["X-Zgi-Enabled-Skills"] = strings.Join(req.EnabledSkillIDs, ",")
		}
	}
	return merged
}

func (d *CliDriver) buildEnv(req agentruntime.ChatRequest, gatewayKey string) map[string]string {
	env := map[string]string{}
	// Route codex/claude through the ZGI LLM gateway using the org's API key.
	// Usage meters through that key's quota (gateway QuotaSubjectType=api_key).
	if d.opts.LLMGatewayURL != "" && gatewayKey != "" {
		env["ZGI_LLM_GATEWAY_URL"] = d.opts.LLMGatewayURL
		switch d.agentType {
		case AgentTypeClaude:
			env["ANTHROPIC_BASE_URL"] = strings.TrimRight(d.opts.LLMGatewayURL, "/") + "/anthropic"
			env["ANTHROPIC_API_KEY"] = gatewayKey
		case AgentTypeCodex:
			env["OPENAI_API_KEY"] = gatewayKey
		}
		return env
	}
	if d.agentType == AgentTypeClaude && d.opts.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = d.opts.APIKey
	}
	if d.agentType == AgentTypeCodex && d.opts.APIKey != "" {
		env["OPENAI_API_KEY"] = d.opts.APIKey
	}
	return env
}

func (d *CliDriver) resolveModel(req agentruntime.ChatRequest) string {
	if req.ModelName != "" {
		return req.ModelName
	}
	return d.opts.Model
}

func (d *CliDriver) loadAgentSession(ctx context.Context, agentID, conversationID uuid.UUID) (string, error) {
	if d.opts.WorkspaceSvc == nil {
		return "", nil
	}
	snap, err := d.opts.WorkspaceSvc.LoadSessionSnapshot(ctx, conversationID)
	if err != nil {
		return "", nil // no prior session
	}
	var state map[string]string
	if err := json.Unmarshal(snap.Checkpoint, &state); err != nil {
		return "", nil
	}
	return state["agent_session_id"], nil
}

func (d *CliDriver) persistSnapshot(ctx context.Context, agentID, conversationID uuid.UUID, status string, checkpoint []byte) error {
	if d.opts.WorkspaceSvc == nil {
		return nil
	}
	if checkpoint == nil {
		checkpoint = []byte(`{}`)
	}
	return d.opts.WorkspaceSvc.SaveSessionSnapshot(ctx, workspace.SessionSnapshot{
		SessionID:      conversationID,
		AgentID:        agentID,
		ConversationID: conversationID,
		RuntimeType:    string(d.RuntimeType()),
		Status:         status,
		Checkpoint:     checkpoint,
	})
}

// handlePermissionRequest surfaces an approval request to the frontend and
// auto-resolves it via the governance policy (approve safe tools, deny risky
// ones) so the run never hangs on a missing interactive approval UI.
func (d *CliDriver) handlePermissionRequest(ctx context.Context, sessionID uuid.UUID, evt *RunnerEvent, onEvent func(agentruntime.StreamEvent) error) {
	if evt.CorrelationID == "" {
		return
	}
	d.emitEvent(onEvent, EventApprovalRequired, ApprovalRequiredPayload{
		ToolName:      evt.Tool,
		Arguments:     evt.Input,
		Reason:        evt.Reason,
		CorrelationID: evt.CorrelationID,
	})

	decision := "approve"
	if d.opts.Governance != nil {
		mapped := mapRunnerTool(d.agentType, evt.Tool)
		if _, needsApproval := d.opts.Governance.RequiresApproval(mapped, evt.Input); needsApproval {
			decision = "reject"
		}
	}
	_ = d.client.ResolvePermission(ctx, sessionID.String(), PermissionRequest{
		CorrelationID: evt.CorrelationID,
		Decision:      decision,
		Reason:        "auto-decided by governance policy",
	})
}

func (d *CliDriver) emitEvent(onEvent func(agentruntime.StreamEvent) error, eventType string, payload interface{}) {
	if onEvent == nil {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = onEvent(agentruntime.StreamEvent{
		ID:        uuid.New(),
		EventType: eventType,
		Payload:   data,
		CreatedAt: time.Now(),
	})
}

// mapRunnerTool maps a real Agent CLI tool name to the canonical governance
// tool name used by ManifestForTool.
func mapRunnerTool(agentType, tool string) string {
	switch agentType {
	case AgentTypeClaude:
		switch tool {
		case "Bash":
			return agentruntime.ToolShellRun
		case "Edit":
			return agentruntime.ToolFilesEdit
		case "Write":
			return agentruntime.ToolFilesWrite
		case "Read":
			return agentruntime.ToolFilesRead
		case "Grep":
			return agentruntime.ToolGrep
		case "Glob":
			return agentruntime.ToolGlob
		case "WebFetch":
			return agentruntime.ToolWebFetch
		case "WebSearch":
			return agentruntime.ToolWebSearch
		case "Agent", "Task":
			return agentruntime.ToolSubagent
		}
	}
	return strings.ToLower(strings.TrimSpace(tool))
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func evtChangesKind(changes json.RawMessage) (string, bool) {
	if len(changes) == 0 {
		return "", false
	}
	var list []struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(changes, &list); err != nil || len(list) == 0 {
		return "", false
	}
	if list[0].Kind != "" {
		return list[0].Kind, true
	}
	return "", false
}

func firstChangePath(changes json.RawMessage) string {
	if len(changes) == 0 {
		return ""
	}
	var list []struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(changes, &list); err != nil || len(list) == 0 {
		return ""
	}
	return list[0].Path
}
