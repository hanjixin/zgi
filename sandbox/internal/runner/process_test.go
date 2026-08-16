package runner

import (
	"context"
	"io"
	"testing"
)

func TestProcessBackendStartProcessStreamsStdio(t *testing.T) {
	backend := newProcessBackend()
	sess, err := backend.StartProcess(context.Background(), ProcessSpec{
		Command: "/bin/sh",
		Args:    []string{"-c", "cat; echo DONE"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	defer func() { _ = sess.Kill() }()

	if _, err := io.WriteString(sess.Stdin, "hello\n"); err != nil {
		t.Fatalf("write stdin: %v", err)
	}
	_ = sess.Stdin.Close()

	out, err := io.ReadAll(sess.Stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	if got, want := string(out), "hello\nDONE\n"; got != want {
		t.Fatalf("stdout = %q, want %q", got, want)
	}
	if err := sess.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
}

func TestProcessBackendStartProcessKillsProcessGroup(t *testing.T) {
	backend := newProcessBackend()
	sess, err := backend.StartProcess(context.Background(), ProcessSpec{
		Command: "/bin/sh",
		Args:    []string{"-c", "sleep 60"},
	})
	if err != nil {
		t.Fatalf("StartProcess: %v", err)
	}
	if err := sess.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if err := sess.Wait(); err == nil {
		t.Fatal("expected non-nil error after Kill")
	}
}
