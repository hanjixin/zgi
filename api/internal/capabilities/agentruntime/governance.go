package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/zgiai/zgi/api/internal/capabilities/toolgovernance"
)

// Canonical coding-agent tool names used for governance decisions.
const (
	ToolFilesRead      = "files_read"
	ToolFilesWrite     = "files_write"
	ToolFilesEdit      = "files_edit"
	ToolShellRun       = "shell_run"
	ToolGrep           = "grep"
	ToolGlob           = "glob"
	ToolCodebaseSearch = "codebase_search"
	ToolWebFetch       = "web_fetch"
	ToolWebSearch      = "web_search"
	ToolImageGen       = "image_gen"
	ToolSubagent       = "subagent"
)

// GovernanceApprovalService routes coding-agent tool approval decisions through
// the existing tool_governance policy engine instead of hardcoded per-tool
// flags. Each tool maps to a governance Manifest (effect / asset_type /
// risk_level / approval policy); toolgovernance.Decide produces the
// authoritative Decision (allowed / needs_approval / denied).
type GovernanceApprovalService struct {
	policy toolgovernance.Policy
	mu     sync.Mutex
	tokens map[uuid.UUID]*ApprovalToken
}

// ApprovalToken records a pending approval request.
type ApprovalToken struct {
	ID            uuid.UUID       `json:"id"`
	CorrelationID string          `json:"correlation_id"`
	ToolName      string          `json:"tool_name"`
	Arguments     json.RawMessage `json:"arguments"`
	Reason        string          `json:"reason"`
	Approved      bool            `json:"approved"`
	Comment       string          `json:"comment,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	ResolvedAt    *time.Time      `json:"resolved_at,omitempty"`
}

func NewGovernanceApprovalService() *GovernanceApprovalService {
	return &GovernanceApprovalService{
		policy: toolgovernance.DefaultPolicy(),
		tokens: make(map[uuid.UUID]*ApprovalToken),
	}
}

// Decide evaluates a tool call against the governance policy and returns the
// full decision (correlation id, status, reason, manifest, approval event).
func (s *GovernanceApprovalService) Decide(toolName string, args json.RawMessage) toolgovernance.Decision {
	if s == nil {
		s = NewGovernanceApprovalService()
	}
	return toolgovernance.Decide(toolgovernance.Request{
		Manifest:       ManifestForTool(toolName),
		PermissionTier: toolgovernance.PermissionTierBasic,
		ApprovalMode:   toolgovernance.ApprovalModeInteractive,
		CorrelationID:  uuid.NewString(),
	}, s.policy)
}

// RequiresApproval reports whether the governance policy requires user approval
// before this tool call may run, along with the governing reason.
func (s *GovernanceApprovalService) RequiresApproval(toolName string, args json.RawMessage) (string, bool) {
	decision := s.Decide(toolName, args)
	return decision.Reason, decision.RequiresApproval
}

func (s *GovernanceApprovalService) RequireApproval(_ context.Context, correlationID, toolName string, arguments json.RawMessage, reason string) (ApprovalToken, error) {
	if strings.TrimSpace(correlationID) == "" {
		correlationID = uuid.NewString()
	}
	token := ApprovalToken{
		ID:            uuid.New(),
		CorrelationID: correlationID,
		ToolName:      toolName,
		Arguments:     arguments,
		Reason:        reason,
		CreatedAt:     time.Now(),
	}
	s.mu.Lock()
	s.tokens[token.ID] = &token
	s.mu.Unlock()
	return token, nil
}

func (s *GovernanceApprovalService) ResolveApproval(_ context.Context, token ApprovalToken, approved bool, comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	stored, ok := s.tokens[token.ID]
	if !ok {
		return errors.New("approval token not found")
	}
	stored.Approved = approved
	stored.Comment = comment
	now := time.Now()
	stored.ResolvedAt = &now
	return nil
}

// ManifestForTool maps a coding-agent tool to the tool_governance manifest that
// drives its approval decision. Mutating/remote tools that need a human in the
// loop are declared with ApprovalPolicyAlwaysAsk; read/search/isolated sandbox
// tools are declared NeverAsk so the default policy lets them run.
func ManifestForTool(toolName string) toolgovernance.Manifest {
	switch toolName {
	case ToolFilesEdit:
		return toolgovernance.Manifest{
			ToolID:                "codex.files.edit",
			SkillID:               "codex",
			Domain:                "code",
			Effect:                toolgovernance.EffectUpdate,
			AssetType:             "file",
			RiskLevel:             toolgovernance.RiskLevelMedium,
			DefaultApprovalPolicy: toolgovernance.ApprovalPolicyAlwaysAsk,
			AuditRequired:         true,
		}
	case ToolWebFetch:
		return toolgovernance.Manifest{
			ToolID:                "codex.web.fetch",
			SkillID:               "codex",
			Domain:                "web",
			Effect:                toolgovernance.EffectRead,
			AssetType:             "url",
			RiskLevel:             toolgovernance.RiskLevelMedium,
			ExternalSideEffect:    true,
			DefaultApprovalPolicy: toolgovernance.ApprovalPolicyAlwaysAsk,
			AuditRequired:         true,
		}
	case ToolWebSearch:
		return toolgovernance.Manifest{
			ToolID:                "codex.web.search",
			SkillID:               "codex",
			Domain:                "web",
			Effect:                toolgovernance.EffectRead,
			AssetType:             "query",
			RiskLevel:             toolgovernance.RiskLevelLow,
			DefaultApprovalPolicy: toolgovernance.ApprovalPolicyNeverAsk,
		}
	case ToolShellRun:
		return toolgovernance.Manifest{
			ToolID:                "codex.shell.run",
			SkillID:               "codex",
			Domain:                "sandbox",
			Effect:                toolgovernance.EffectInvoke,
			AssetType:             "sandbox",
			RiskLevel:             toolgovernance.RiskLevelLow,
			DefaultApprovalPolicy: toolgovernance.ApprovalPolicyNeverAsk,
		}
	default:
		return toolgovernance.Manifest{
			ToolID:                "codex." + toolName,
			SkillID:               "codex",
			Domain:                "code",
			Effect:                codexToolDefaultEffect(toolName),
			AssetType:             "code",
			RiskLevel:             toolgovernance.RiskLevelLow,
			DefaultApprovalPolicy: toolgovernance.ApprovalPolicyNeverAsk,
		}
	}
}

func codexToolDefaultEffect(toolName string) toolgovernance.Effect {
	switch toolName {
	case ToolFilesRead, ToolGrep, ToolGlob, ToolCodebaseSearch:
		return toolgovernance.EffectRead
	case ToolFilesWrite, ToolImageGen:
		return toolgovernance.EffectCreate
	default:
		return toolgovernance.EffectRead
	}
}
