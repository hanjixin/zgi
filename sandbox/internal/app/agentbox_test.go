package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentBoxCreate(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}

	body := `{"ttl_seconds": 120, "network_enabled": false, "workspace_seed": {"CLAUDE.md": "# ZGI\n"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent-boxes", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "req_agentbox")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Data struct {
			BoxID         string `json:"box_id"`
			WorkspacePath string `json:"workspace_path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Data.BoxID == "" {
		t.Fatal("expected box_id")
	}
	if envelope.Data.WorkspacePath == "" {
		t.Fatal("expected workspace_path")
	}
}

func TestAgentBoxCreateWithNetworkEnabled(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}

	body := `{"network_enabled": true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/agent-boxes", strings.NewReader(body))
	req.Header.Set("X-Request-ID", "req_agentbox_net")
	rr := httptest.NewRecorder()
	server.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rr.Code, rr.Body.String())
	}
	var envelope struct {
		Data struct {
			BoxID         string `json:"box_id"`
			WorkspacePath string `json:"workspace_path"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	if envelope.Data.BoxID == "" {
		t.Fatal("expected box_id")
	}
	if envelope.Data.WorkspacePath == "" {
		t.Fatal("expected workspace_path")
	}
}
