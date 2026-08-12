package agentruntime

import (
	"context"

	"github.com/google/uuid"
)

type AgentDescriptor struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	RuntimeType  RuntimeType
	RuntimeConfig map[string]interface{}
}

type Router struct {
	business   Driver
	codex      Driver
	claudeCode Driver
}

type RouterOption func(*Router)

func WithBusinessDriver(d Driver) RouterOption {
	return func(r *Router) { r.business = d }
}

func WithCodexDriver(d Driver) RouterOption {
	return func(r *Router) { r.codex = d }
}

func WithClaudeCodeDriver(d Driver) RouterOption {
	return func(r *Router) { r.claudeCode = d }
}

func NewRouter(opts ...RouterOption) *Router {
	r := &Router{}
	for _, opt := range opts {
		opt(r)
	}
	return r
}

func (r *Router) Route(_ context.Context, agent AgentDescriptor) Driver {
	switch NormalizeRuntimeType(agent.RuntimeType) {
	case RuntimeTypeCodex:
		if r.codex != nil {
			return r.codex
		}
		return r.business
	case RuntimeTypeClaudeCode:
		if r.claudeCode != nil {
			return r.claudeCode
		}
		return r.business
	default:
		if r.business != nil {
			return r.business
		}
		return nil
	}
}

func (r *Router) Business() Driver    { return r.business }
func (r *Router) Codex() Driver       { return r.codex }
func (r *Router) ClaudeCode() Driver  { return r.claudeCode }
