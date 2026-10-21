// Package proc runs external commands (gh, git, wtp, docker) with a timeout.
package proc

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Timeout bounds every gh/git/wtp call so a hung network call can't stall
// the tick loop. Generous because `wtp add` runs dependency-install hooks.
var Timeout = 15 * time.Minute

// Run executes a command in dir and returns trimmed stdout; errors carry stderr.
func Run(ctx context.Context, dir, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}
