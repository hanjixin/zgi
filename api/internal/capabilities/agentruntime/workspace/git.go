package workspace

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type ShellRunner interface {
	Run(ctx context.Context, command string, args ...string) (stdout string, stderr string, exitCode int, err error)
}

type GitClient struct {
	runner ShellRunner
}

func NewGitClient(runner ShellRunner) *GitClient {
	return &GitClient{runner: runner}
}

func (g *GitClient) Clone(ctx context.Context, repoURL, targetDir string) error {
	if g.runner == nil {
		return errors.New("git runner not configured")
	}
	_, _, code, err := g.runner.Run(ctx, "git", "clone", "--depth", "1", repoURL, targetDir)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git clone exited with code %d", code)
	}
	return nil
}

func (g *GitClient) Checkout(ctx context.Context, workDir, branch string) error {
	if g.runner == nil {
		return errors.New("git runner not configured")
	}
	_, _, code, err := g.runner.Run(ctx, "git", "-C", workDir, "checkout", branch)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git checkout exited with code %d", code)
	}
	return nil
}

func (g *GitClient) Pull(ctx context.Context, workDir string) error {
	if g.runner == nil {
		return errors.New("git runner not configured")
	}
	_, _, code, err := g.runner.Run(ctx, "git", "-C", workDir, "pull", "--ff-only")
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git pull exited with code %d", code)
	}
	return nil
}

func (g *GitClient) Commit(ctx context.Context, workDir, message string) error {
	if g.runner == nil {
		return errors.New("git runner not configured")
	}
	if _, _, code, err := g.runner.Run(ctx, "git", "-C", workDir, "add", "-A"); err != nil || code != 0 {
		return fmt.Errorf("git add failed: code=%d err=%v", code, err)
	}
	msg := message
	if msg == "" {
		msg = fmt.Sprintf("auto-commit at %s", time.Now().Format(time.RFC3339))
	}
	_, _, code, err := g.runner.Run(ctx, "git", "-C", workDir, "commit", "-m", msg)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git commit exited with code %d", code)
	}
	return nil
}

func (g *GitClient) Push(ctx context.Context, workDir, remote, branch string) error {
	if g.runner == nil {
		return errors.New("git runner not configured")
	}
	args := []string{"-C", workDir, "push"}
	if remote != "" {
		args = append(args, remote)
	}
	if branch != "" {
		args = append(args, branch)
	}
	_, _, code, err := g.runner.Run(ctx, "git", args...)
	if err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("git push exited with code %d", code)
	}
	return nil
}
