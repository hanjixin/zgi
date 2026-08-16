package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/zgiai/zgi-sandbox/internal/lifecycle"
	"github.com/zgiai/zgi-sandbox/internal/observer"
)

type agentBoxCreateRequest struct {
	TTLSeconds     int               `json:"ttl_seconds,omitempty"`
	NetworkEnabled bool              `json:"network_enabled,omitempty"`
	WorkspaceSeed  map[string]string `json:"workspace_seed,omitempty"`
	OrganizationID string            `json:"organization_id,omitempty"`
	WorkspaceID    string            `json:"workspace_id,omitempty"`
	UserID         string            `json:"user_id,omitempty"`
}

func (s *Server) handleAgentBoxCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}

	var req agentBoxCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxSmallJSONRequestBytes())).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}

	ttl := req.TTLSeconds
	if ttl <= 0 {
		ttl = s.config.AgentBoxTTLSeconds
	}

	box, err := s.lifecycle.Create(lifecycle.CreateRequest{
		RuntimeProfile: "session",
		TTLSeconds:     ttl,
		NetworkEnabled: req.NetworkEnabled,
		OrganizationID: req.OrganizationID,
		WorkspaceID:    req.WorkspaceID,
		UserID:         req.UserID,
	})
	if err != nil {
		writeKnownError(w, err)
		return
	}

	for name, content := range req.WorkspaceSeed {
		if err := writeAgentBoxSeed(box.RootPath, name, content); err != nil {
			writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, err.Error(), nil)
			return
		}
	}

	workspacePath := "/tmp/workspace"
	if !s.policy.NetworkPolicyEnforced() {
		workspacePath = box.RootPath
	}

	s.observer.Record("agent.box.created", box.ID, "agent box created", observer.MetadataWithContext(r.Context(), map[string]any{
		"runtime_profile": "session",
		"ttl_seconds":     ttl,
		"network_enabled": req.NetworkEnabled,
	}))

	writeEnvelope(w, http.StatusOK, map[string]any{
		"box_id":         box.ID,
		"workspace_path": workspacePath,
	})
}

func writeAgentBoxSeed(root string, name string, content string) error {
	switch name {
	case "CLAUDE.md", "AGENTS.md":
	default:
		return fmt.Errorf("unsupported seed file: %s", name)
	}
	if strings.ContainsAny(name, "/\\") {
		return errors.New("unsupported seed file name")
	}
	path := filepath.Join(root, name)
	return os.WriteFile(path, []byte(content), 0o644)
}

func (s *Server) handleAgentBoxByID(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		box, err := s.lifecycle.Get(id)
		if err != nil {
			writeKnownError(w, err)
			return
		}
		writeEnvelope(w, http.StatusOK, map[string]any{"id": box.ID, "status": string(box.Status), "root_path": box.RootPath})
	case http.MethodDelete:
		if err := s.lifecycle.Delete(id); err != nil {
			writeKnownError(w, err)
			return
		}
		writeEnvelope(w, http.StatusOK, map[string]any{"deleted": true, "box_id": id})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
