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
	"sync"
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

// TestAgentBoxProcessStdinClose is a regression test for the codex bridge EOF
// relay: a boxed process whose stdin is closed via /stdin/close must see EOF
// and exit. Codex-style CLIs read their prompt from stdin to EOF before they
// start, so without forwarding the SDK's stdin close the boxed codex hangs.
func TestAgentBoxProcessStdinClose(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	boxID := createAgentBox(t, ts)

	// cat reads stdin to EOF, echoes what it read, then exits 0.
	procResp := startAgentProcess(t, ts, boxID, "/bin/cat", nil)
	defer procResp.Body.Close()

	scanner := bufio.NewScanner(procResp.Body)
	started, err := readSSEFrame(scanner)
	if err != nil || started["type"] != "started" {
		t.Fatalf("expected started frame, got %v err=%v", started, err)
	}
	pid := started["pid"].(string)

	stdinReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent-boxes/"+boxID+"/process/"+pid+"/stdin", bytes.NewReader([]byte("hello-eof")))
	stdinResp, err := http.DefaultClient.Do(stdinReq)
	if err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	io.Copy(io.Discard, stdinResp.Body)
	stdinResp.Body.Close()

	closeReq, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/agent-boxes/"+boxID+"/process/"+pid+"/stdin/close", nil)
	closeResp, err := http.DefaultClient.Do(closeReq)
	if err != nil {
		t.Fatalf("close stdin: %v", err)
	}
	if closeResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(closeResp.Body)
		closeResp.Body.Close()
		t.Fatalf("close stdin: status %d body %s", closeResp.StatusCode, body)
	}
	io.Copy(io.Discard, closeResp.Body)
	closeResp.Body.Close()

	var stdout strings.Builder
	exit := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && exit == "" {
		frame, err := readSSEFrame(scanner)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch ftype, _ := frame["type"].(string); ftype {
		case "stdout":
			stdout.WriteString(frame["data"].(string))
		case "exit":
			exit = fmt.Sprintf("%v", frame["code"])
		}
	}
	if exit == "" {
		t.Fatal("no exit frame; boxed process never saw stdin EOF")
	}
	if !strings.Contains(stdout.String(), "hello-eof") {
		t.Fatalf("stdout = %q, want it to contain hello-eof", stdout.String())
	}
	if exit != "0" {
		t.Fatalf("exit = %q, want 0", exit)
	}
}

// createAgentBox spins up an agent box and returns its box_id.
func createAgentBox(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	createReq, _ := json.Marshal(map[string]any{"ttl_seconds": 120})
	resp, err := http.Post(ts.URL+"/v1/agent-boxes", "application/json", bytes.NewReader(createReq))
	if err != nil {
		t.Fatalf("create box: %v", err)
	}
	defer resp.Body.Close()
	var createEnvelope struct {
		Data struct {
			BoxID string `json:"box_id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&createEnvelope); err != nil {
		t.Fatalf("decode create box: %v", err)
	}
	if createEnvelope.Data.BoxID == "" {
		t.Fatal("expected box_id")
	}
	return createEnvelope.Data.BoxID
}

// startAgentProcess POSTs a process to a box and returns the SSE response body.
func startAgentProcess(t *testing.T, ts *httptest.Server, boxID string, command string, args []string) *http.Response {
	t.Helper()
	procReq, _ := json.Marshal(map[string]any{"command": command, "args": args})
	resp, err := http.Post(ts.URL+"/v1/agent-boxes/"+boxID+"/process", "application/json", bytes.NewReader(procReq))
	if err != nil {
		t.Fatalf("start process: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("start process: status %d body %s", resp.StatusCode, body)
	}
	return resp
}

// readSSEFrame returns the next "data: ..." SSE frame as a JSON object.
func readSSEFrame(scanner *bufio.Scanner) (map[string]any, error) {
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

// TestAgentBoxProcessStderrWhileStdoutOpen is a regression test for the
// stdout-then-stderr serialization deadlock: a process that fills the stderr
// pipe buffer while stdout stays open must still be streamed to completion.
// The old sequential drain would block the process on stderr, keep stdout open,
// and never reach sess.Wait(), leaking the process, its map entry and its
// semaphore slot forever.
func TestAgentBoxProcessStderrWhileStdoutOpen(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	boxID := createAgentBox(t, ts)

	// Write ~320KB to stderr while keeping stdout open until the very end, then
	// print stdoutdone and exit. 320KB far exceeds the ~64KB OS pipe buffer.
	procResp := startAgentProcess(t, ts, boxID, "/bin/sh", []string{"-c",
		`i=0; while [ "$i" -lt 4000 ]; do printf "01234567890123456789012345678901234567890123456789012345678901234567890123456789\n"; i=$((i+1)); done >&2; echo stdoutdone`})
	defer procResp.Body.Close()

	scanner := bufio.NewScanner(procResp.Body)
	started, err := readSSEFrame(scanner)
	if err != nil || started["type"] != "started" {
		t.Fatalf("expected started frame, got %v err=%v", started, err)
	}

	var stdout, stderr strings.Builder
	exit := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && exit == "" {
		frame, err := readSSEFrame(scanner)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		switch ftype, _ := frame["type"].(string); ftype {
		case "stdout":
			stdout.WriteString(frame["data"].(string))
		case "stderr":
			stderr.WriteString(frame["data"].(string))
		case "exit":
			exit = fmt.Sprintf("%v", frame["code"])
		}
	}
	if exit == "" {
		t.Fatal("no exit frame; handler deadlocked on the stderr pipe")
	}
	if !strings.Contains(stdout.String(), "stdoutdone") {
		t.Fatalf("stdout = %q, want it to contain stdoutdone", stdout.String())
	}
	if stderr.Len() < 65536 {
		t.Fatalf("stderr drained only %d bytes, want > 64KB streamed", stderr.Len())
	}
	if exit != "0" {
		t.Fatalf("exit = %q, want 0", exit)
	}
}

// TestAgentBoxProcessConcurrentKillAndStdin is a regression test for the
// unsynchronized agentProcesses map: kill and stdin requests read the map on
// their own goroutines while the process handler writes it (insert/delete). A
// concurrent read/write on the old code is a fatal Go runtime panic. Run with
// -race for the strongest signal.
func TestAgentBoxProcessConcurrentKillAndStdin(t *testing.T) {
	server, err := NewServer(testConfig(t))
	if err != nil {
		t.Fatalf("expected server, got %v", err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()
	boxID := createAgentBox(t, ts)

	procResp := startAgentProcess(t, ts, boxID, "/bin/sh", []string{"-c", "sleep 5"})
	defer procResp.Body.Close()

	scanner := bufio.NewScanner(procResp.Body)
	started, err := readSSEFrame(scanner)
	if err != nil || started["type"] != "started" {
		t.Fatalf("expected started frame, got %v err=%v", started, err)
	}
	pid := started["pid"].(string)

	const n = 20
	results := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			url := ts.URL + "/v1/agent-boxes/" + boxID + "/process/" + pid
			var req *http.Request
			if i%2 == 0 {
				req, _ = http.NewRequest(http.MethodPost, url+"/stdin", strings.NewReader("hello"))
			} else {
				req, _ = http.NewRequest(http.MethodDelete, url, nil)
			}
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				// A transport error here usually means the server panicked on a
				// concurrent map access and dropped the connection.
				results[i] = -1
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			results[i] = resp.StatusCode
		}(i)
	}
	wg.Wait()

	for i, code := range results {
		if code == -1 {
			t.Fatalf("request %d failed at the transport level (server likely panicked on a concurrent map access)", i)
		}
		if code != http.StatusOK && code != http.StatusBadRequest && code != http.StatusNotFound {
			t.Fatalf("request %d: unexpected status %d", i, code)
		}
	}

	exit := ""
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && exit == "" {
		frame, err := readSSEFrame(scanner)
		if err != nil {
			t.Fatalf("read frame: %v", err)
		}
		if ftype, _ := frame["type"].(string); ftype == "exit" {
			exit = fmt.Sprintf("%v", frame["code"])
		}
	}
	if exit == "" {
		t.Fatal("no exit frame after kill")
	}
}
