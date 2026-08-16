package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAgentBoxProcessChannel(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createReq, _ := json.Marshal(map[string]any{"ttl_seconds": 120})
	resp, err := http.Post(ts.URL+"/v1/agent-boxes", "application/json", bytes.NewReader(createReq))
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	var createEnvelope struct {
		Data struct {
			BoxID string `json:"box_id"`
		} `json:"data"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&createEnvelope)
	resp.Body.Close()
	boxID := createEnvelope.Data.BoxID
	if boxID == "" {
		t.Fatal("expected box_id")
	}

	// Start a process that reads 5 bytes then prints EOF and exits.
	procReq, _ := json.Marshal(map[string]any{
		"command": "/bin/sh",
		"args":    []string{"-c", "head -c 5; echo EOF"},
	})
	procResp, err := http.Post(ts.URL+"/v1/agent-boxes/"+boxID+"/process", "application/json", bytes.NewReader(procReq))
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	defer procResp.Body.Close()

	scanner := bufio.NewScanner(procResp.Body)
	readFrame := func() (map[string]any, error) {
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame map[string]any
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame); err != nil {
				return nil, err
			}
			return frame, nil
		}
		return nil, scanner.Err()
	}

	started, err := readFrame()
	if err != nil || started["type"] != "started" {
		t.Fatalf("expected started frame, got %v err=%v", started, err)
	}
	pid := started["pid"].(string)

	stdinReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent-boxes/"+boxID+"/process/"+pid+"/stdin", bytes.NewReader([]byte("hello")))
	stdinResp, err := http.DefaultClient.Do(stdinReq)
	if err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	io.Copy(io.Discard, stdinResp.Body)
	stdinResp.Body.Close()

	got := map[string]string{}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		frame, err := readFrame()
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		ftype, _ := frame["type"].(string)
		switch ftype {
		case "stdout":
			got["stdout"] += frame["data"].(string)
		case "exit":
			got["exit"] = fmt.Sprintf("%v", frame["code"])
		}
		if got["exit"] != "" {
			break
		}
	}
	if !strings.Contains(got["stdout"], "hello") || !strings.Contains(got["stdout"], "EOF") {
		t.Fatalf("stdout = %q, want it to contain hello and EOF", got["stdout"])
	}
	if got["exit"] != "0" {
		t.Fatalf("exit = %q, want 0", got["exit"])
	}
}
