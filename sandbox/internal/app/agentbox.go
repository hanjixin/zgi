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
	"sync"
	"sync/atomic"

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

	networkPolicy := ""
	if req.NetworkEnabled {
		// Agent boxes need egress to the LLM gateway + zgi-tools MCP bridge,
		// which the session profile's deny-by-default policy would block.
		networkPolicy = "agent-session"
	}
	box, err := s.lifecycle.Create(lifecycle.CreateRequest{
		RuntimeProfile: "session",
		TTLSeconds:     ttl,
		NetworkEnabled: req.NetworkEnabled,
		NetworkPolicy:  networkPolicy,
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

	// Kill the whole process group on every exit path, including ctx cancel /
	// client disconnect. exec.CommandContext only kills the direct child, so a
	// grandchild that holds the pipes open would otherwise keep the drains and
	// Wait blocked on pipe EOF and this handler would never unwind. Kill is
	// idempotent-safe.
	defer sess.Kill()
	go func() {
		<-r.Context().Done()
		_ = sess.Kill()
	}()

	s.agentProcessMu.Lock()
	s.agentProcesses[pid] = sess
	s.agentProcessMu.Unlock()
	defer func() {
		s.agentProcessMu.Lock()
		delete(s.agentProcesses, pid)
		s.agentProcessMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	fw := &agentFrameWriter{w: w, flusher: flusher}
	fw.write(map[string]any{"type": "started", "pid": pid})

	// Drain stdout and stderr concurrently. A process can keep stdout open while
	// writing more than the OS pipe buffer (~64KB) to stderr; draining
	// sequentially would block the process on stderr, keep stdout from closing,
	// and leak the process, its map entry and its semaphore slot forever.
	drainErrs := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := streamAgentOutput(fw, "stdout", sess.Stdout); err != nil {
			drainErrs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := streamAgentOutput(fw, "stderr", sess.Stderr); err != nil {
			drainErrs <- err
		}
	}()
	wg.Wait()
	close(drainErrs)

	exitCode := 0
	if err := sess.Wait(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			fw.write(map[string]any{"type": "error", "message": err.Error()})
		}
	}
	for err := range drainErrs {
		fw.write(map[string]any{"type": "error", "message": err.Error()})
		break
	}
	fw.write(map[string]any{"type": "exit", "code": exitCode})
	s.observer.Record("agent.process.exited", boxID, "agent process exited", observer.MetadataWithContext(r.Context(), map[string]any{"pid": pid, "exit_code": exitCode}))
}

func (s *Server) handleAgentBoxStdin(w http.ResponseWriter, r *http.Request, pid string) {
	if !s.authorized(r) {
		writeEnvelopeWithMessage(w, http.StatusUnauthorized, -401, "unauthorized", nil)
		return
	}
	s.agentProcessMu.RLock()
	sess, ok := s.agentProcesses[pid]
	s.agentProcessMu.RUnlock()
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
	s.agentProcessMu.RLock()
	sess, ok := s.agentProcesses[pid]
	s.agentProcessMu.RUnlock()
	if !ok {
		writeEnvelopeWithMessage(w, http.StatusNotFound, -404, "process not found", nil)
		return
	}
	_ = sess.Kill()
	writeEnvelope(w, http.StatusOK, map[string]any{"killed": true, "pid": pid})
}

// agentFrameWriter serializes writes to the SSE response so the concurrent
// stdout/stderr drain goroutines never interleave frames mid-frame. The
// response writer must not be written from two goroutines without a lock.
type agentFrameWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
	mu      sync.Mutex
}

func (fw *agentFrameWriter) write(frame map[string]any) {
	payload, _ := json.Marshal(frame)
	fw.mu.Lock()
	defer fw.mu.Unlock()
	_, _ = fmt.Fprintf(fw.w, "data: %s\n\n", payload)
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
}

// streamAgentOutput forwards reads from reader as SSE frames of frameType,
// returning nil on EOF and the read error otherwise so the caller can surface
// a post-start streaming failure to the client.
func streamAgentOutput(fw *agentFrameWriter, frameType string, reader io.Reader) error {
	buf := make([]byte, 32*1024)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			fw.write(map[string]any{"type": frameType, "data": string(buf[:n])})
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// agentPIDSeq is the collision-free fallback pid nonce used only if crypto/rand
// fails (essentially never). It keeps pids unique across concurrent process
// starts instead of colliding on the literal "ap".
var agentPIDSeq atomic.Uint64

func randToken() string {
	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("fb%08x", agentPIDSeq.Add(1))
	}
	return hex.EncodeToString(buf[:])
}
