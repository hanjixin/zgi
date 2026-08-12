package cli

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// RunnerClient talks to the agent-runner service over HTTP + SSE.
type RunnerClient struct {
	baseURL string
	http    *http.Client
}

func NewRunnerClient(baseURL string) *RunnerClient {
	return &RunnerClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 0}, // no timeout; the stream is long-lived
	}
}

// Run starts an agent run and returns a streaming reader of normalized events.
// The caller must Close the returned stream.
func (c *RunnerClient) Run(ctx context.Context, req RunRequest) (*EventStream, error) {
	if c == nil || c.baseURL == "" {
		return nil, errors.New("agent runner not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/agents/run", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("agent runner run: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("agent runner run failed (status %d): %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return newEventStream(resp.Body), nil
}

// Stop aborts a running agent by its control-plane session id.
func (c *RunnerClient) Stop(ctx context.Context, sessionID string) error {
	if c == nil || c.baseURL == "" {
		return errors.New("agent runner not configured")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/agents/"+sessionID+"/stop", nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("agent runner stop failed (status %d)", resp.StatusCode)
	}
	return nil
}

// ResolvePermission resolves a pending approval request on the runner.
func (c *RunnerClient) ResolvePermission(ctx context.Context, sessionID string, req PermissionRequest) error {
	if c == nil || c.baseURL == "" {
		return errors.New("agent runner not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/agents/"+sessionID+"/permission", bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("agent runner permission failed (status %d)", resp.StatusCode)
	}
	return nil
}

// EventStream wraps an SSE body as an iterator of RunnerEvent.
type EventStream struct {
	scanner *bufio.Scanner
	closer  io.Closer
}

func newEventStream(body io.ReadCloser) *EventStream {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	return &EventStream{scanner: scanner, closer: body}
}

// Next returns the next event. Returns io.EOF when the stream ends cleanly.
func (s *EventStream) Next() (*RunnerEvent, error) {
	for s.scanner.Scan() {
		line := strings.TrimSpace(s.scanner.Text())
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var evt RunnerEvent
		if err := json.Unmarshal([]byte(payload), &evt); err != nil {
			// Tolerate a malformed frame; skip it rather than failing the run.
			continue
		}
		return &evt, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

func (s *EventStream) Close() error {
	if s.closer != nil {
		return s.closer.Close()
	}
	return nil
}
