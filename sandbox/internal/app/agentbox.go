package app

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zgiai/zgi-sandbox/internal/lifecycle"
	"github.com/zgiai/zgi-sandbox/internal/observer"
	"github.com/zgiai/zgi-sandbox/internal/runner"
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
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) == 0 {
		http.NotFound(w, r)
		return
	}
	if len(parts) >= 2 && parts[1] == "process" {
		if len(parts) == 2 {
			s.handleAgentBoxProcess(w, r)
			return
		}
		pid := parts[2]
		switch r.Method {
		case http.MethodPost:
			if len(parts) != 4 || parts[3] != "stdin" {
				http.NotFound(w, r)
				return
			}
			s.handleAgentBoxStdin(w, r, pid)
		case http.MethodDelete:
			s.handleAgentBoxKill(w, r, pid)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
		return
	}
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
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

type agentProcessRequest struct {
	Command string            `json:"command"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

func (s *Server) handleAgentBoxProcess(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	parts := splitPath(strings.TrimPrefix(r.URL.Path, "/v1/agent-boxes/"))
	if len(parts) < 2 || parts[1] != "process" {
		http.NotFound(w, r)
		return
	}
	boxID := parts[0]

	box, err := s.lifecycle.Get(boxID)
	if err != nil {
		writeKnownError(w, err)
		return
	}

	var req agentProcessRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.maxSmallJSONRequestBytes())).Decode(&req); err != nil {
		writeDecodeError(w, err)
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, "command is required", nil)
		return
	}

	if !s.agentProcessAcquire(r.Context(), w) {
		return
	}
	defer s.agentProcessRelease()

	workDir := box.RootPath
	sess, err := s.runner.StartProcess(r.Context(), runner.ProcessSpec{
		WorkDir:       workDir,
		Command:       req.Command,
		Args:          req.Args,
		Env:           req.Env,
		EnableNetwork: box.NetworkEnabled,
	})
	if err != nil {
		writeKnownError(w, err)
		return
	}
	pid := "ap_" + randToken()
	s.agentProcesses[pid] = sess
	defer func() {
		delete(s.agentProcesses, pid)
		_ = sess.Kill()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeAgentFrame(w, flusher, map[string]any{"type": "started", "pid": pid})

	streamAgentOutput(w, flusher, "stdout", sess.Stdout)
	streamAgentOutput(w, flusher, "stderr", sess.Stderr)

	exitCode := 0
	if err := sess.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}
	writeAgentFrame(w, flusher, map[string]any{"type": "exit", "code": exitCode})
	s.observer.Record("agent.process.exited", boxID, "agent process exited", observer.MetadataWithContext(r.Context(), map[string]any{"pid": pid, "exit_code": exitCode}))
}

func (s *Server) handleAgentBoxStdin(w http.ResponseWriter, r *http.Request, pid string) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	sess, ok := s.agentProcesses[pid]
	if !ok {
		writeEnvelopeWithMessage(w, http.StatusNotFound, -404, "process not found", nil)
		return
	}
	defer r.Body.Close()
	_, err := io.Copy(sess.Stdin, r.Body)
	if err != nil {
		writeEnvelopeWithMessage(w, http.StatusBadRequest, -400, err.Error(), nil)
		return
	}
	writeEnvelope(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAgentBoxKill(w http.ResponseWriter, r *http.Request, pid string) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	sess, ok := s.agentProcesses[pid]
	if !ok {
		writeEnvelopeWithMessage(w, http.StatusNotFound, -404, "process not found", nil)
		return
	}
	_ = sess.Kill()
	writeEnvelope(w, http.StatusOK, map[string]any{"killed": true, "pid": pid})
}

func writeAgentFrame(w http.ResponseWriter, flusher http.Flusher, frame map[string]any) {
	payload, _ := json.Marshal(frame)
	_, _ = fmt.Fprintf(w, "data: %s\n\n", payload)
	if flusher != nil {
		flusher.Flush()
	}
}

func streamAgentOutput(w http.ResponseWriter, flusher http.Flusher, frameType string, reader io.Reader) {
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			writeAgentFrame(w, flusher, map[string]any{"type": frameType, "data": string(buf[:n])})
		}
		if err != nil {
			return
		}
	}
}

func randToken() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "ap"
	}
	return hex.EncodeToString(buf[:])
}
