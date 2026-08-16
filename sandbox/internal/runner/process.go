package runner

import (
	"context"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// ProcessSpec describes a long-running process to start inside a runtime.
type ProcessSpec struct {
	WorkDir       string
	Command       string
	Args          []string
	Env           map[string]string
	EnableNetwork bool
}

// ProcessSession is a running process with streamed stdio. Wait blocks until
// the process exits and returns its error; Kill terminates the whole group.
type ProcessSession struct {
	Stdin  io.WriteCloser
	Stdout io.Reader
	Stderr io.Reader
	Wait   func() error
	Kill   func() error
}

func (b *processBackend) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	return startStreamedCommand(ctx, spec.Command, spec.Args, spec.WorkDir, spec.Env)
}

func startStreamedCommand(ctx context.Context, command string, args []string, dir string, env map[string]string) (*ProcessSession, error) {
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = dir
	cmd.Env = processEnv(env, nil)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	var once sync.Once
	var waitErr error
	pid := cmd.Process.Pid
	return &ProcessSession{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Wait: func() error {
			once.Do(func() {
				waitErr = cmd.Wait()
				_ = stdin.Close()
			})
			return waitErr
		},
		Kill: func() error {
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return cmd.Process.Kill()
		},
	}, nil
}

func cloneEnv(src map[string]string) map[string]string {
	out := make(map[string]string, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// StartProcess exposes the backend's streaming process API on the Service. It
// deliberately does NOT acquire the shared MaxWorkers semaphore: long-lived
// agent processes would starve short one-shot execs. The caller (app layer)
// enforces the separate agent-process pool.
func (s *Service) StartProcess(ctx context.Context, spec ProcessSpec) (*ProcessSession, error) {
	return s.backend.StartProcess(ctx, spec)
}
