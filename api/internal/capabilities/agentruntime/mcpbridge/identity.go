package mcpbridge

import (
	"context"
	"net/http"
	"strings"
)

// mcpIdentity carries the caller identity the Go driver stamps onto each MCP
// server's http_headers. User-scoped ZGI tools (memory, knowledge, ...) need it
// to resolve the right account/tenant.
type mcpIdentity struct {
	UserID         string
	TenantID       string
	WorkspaceID    string
	AgentID        string
	ConversationID string
	EnabledSkills  []string
}

type mcpIdentityKey struct{}

// identityFromRequest reads the X-Zgi-* headers and attaches them to the
// request context. The mcpbridge trusts them because the endpoint is gated by
// the shared X-MCP-API-Key (only the agent-runner can reach it).
func identityFromRequest(ctx context.Context, r *http.Request) context.Context {
	id := &mcpIdentity{
		UserID:         strings.TrimSpace(r.Header.Get("X-Zgi-User-Id")),
		TenantID:       strings.TrimSpace(r.Header.Get("X-Zgi-Tenant-Id")),
		WorkspaceID:    strings.TrimSpace(r.Header.Get("X-Zgi-Workspace-Id")),
		AgentID:        strings.TrimSpace(r.Header.Get("X-Zgi-Agent-Id")),
		ConversationID: strings.TrimSpace(r.Header.Get("X-Zgi-Conversation-Id")),
	}
	if raw := strings.TrimSpace(r.Header.Get("X-Zgi-Enabled-Skills")); raw != "" {
		for _, part := range strings.Split(raw, ",") {
			if s := strings.TrimSpace(part); s != "" {
				id.EnabledSkills = append(id.EnabledSkills, s)
			}
		}
	}
	return context.WithValue(ctx, mcpIdentityKey{}, id)
}

func identityFromContext(ctx context.Context) *mcpIdentity {
	id, _ := ctx.Value(mcpIdentityKey{}).(*mcpIdentity)
	return id
}
