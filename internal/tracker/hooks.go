package tracker

import (
	"context"
	"errors"
	"fmt"
	"github.com/reneruck/waterboy/internal/worktree"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"
)

const maxHookRuns = 3

// runHook runs a user hook via `sh -c` in dir and waits for it, killing the
// whole process group on timeout. env entries are added to the environment.
func runHook(ctx context.Context, command string, timeout time.Duration, dir string, env map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", command)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	slog.Info("hook finished", "command", command, "dir", dir, "err", err, "output", string(out))
	if err != nil {
		return fmt.Errorf("hook %q: %w", command, err)
	}
	return nil
}

// syncWithRetry runs sync; on a worktree.ConflictError it runs onConflict (if any) and
// retries, running the hook at most maxRuns times. Other errors return at once.
func syncWithRetry(sync func() error, onConflict func(reason string) error, maxRuns int) error {
	for runs := 0; ; runs++ {
		err := sync()
		var ce *worktree.ConflictError
		if err == nil || !errors.As(err, &ce) || onConflict == nil {
			return err
		}
		if runs == maxRuns {
			return fmt.Errorf("still conflicting after %d on_conflict runs: %w", maxRuns, err)
		}
		if hookErr := onConflict(ce.Reason); hookErr != nil {
			slog.Warn("on_conflict hook failed, retrying sync anyway", "err", hookErr)
		}
	}
}
