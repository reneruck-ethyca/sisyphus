package main

import (
	"context"
	"fmt"
	"strings"
)

// ConflictError means a worktree can't be synced without a human (or hook).
type ConflictError struct{ Reason string } // "dirty" | "diverged"

func (e *ConflictError) Error() string { return "worktree conflict: " + e.Reason }

// parseWorktreeList finds the worktree path for branch in `git worktree list --porcelain`.
func parseWorktreeList(porcelain, branch string) string {
	for _, block := range strings.Split(porcelain, "\n\n") {
		var path string
		for _, line := range strings.Split(block, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			}
			if line == "branch refs/heads/"+branch {
				return path
			}
		}
	}
	return ""
}

func findWorktree(ctx context.Context, repoDir, branch string) (string, error) {
	out, err := run(ctx, repoDir, "git", "worktree", "list", "--porcelain")
	if err != nil {
		return "", err
	}
	return parseWorktreeList(out, branch), nil
}

func branchExists(ctx context.Context, repoDir, branch string) bool {
	_, err := run(ctx, repoDir, "git", "rev-parse", "--verify", "--quiet", "refs/heads/"+branch)
	return err == nil
}

func fetchBranch(ctx context.Context, dir, branch string) error {
	_, err := run(ctx, dir, "git", "fetch", "--quiet", "origin",
		fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch))
	return err
}

// createWorktree checks the PR branch out via wtp, which also runs .wtp.yml hooks.
func createWorktree(ctx context.Context, repoDir, branch string) (string, error) {
	if err := fetchBranch(ctx, repoDir, branch); err != nil {
		return "", err
	}
	out, err := run(ctx, repoDir, "wtp", "add", branch, "--quiet")
	if err != nil {
		return "", err
	}
	// ponytail: --quiet prints the path, but hooks may print too; take the last line.
	lines := strings.Split(out, "\n")
	path := strings.TrimSpace(lines[len(lines)-1])
	if path == "" {
		return "", fmt.Errorf("wtp add %s: no path printed", branch)
	}
	return path, nil
}

// removeWorktree force-removes a worktree we created; wtp names worktrees after the branch.
func removeWorktree(ctx context.Context, repoDir, branch string, deleteBranch bool) error {
	args := []string{"remove", "-f"}
	if deleteBranch {
		args = append(args, "--with-branch", "--force-branch")
	}
	_, err := run(ctx, repoDir, "wtp", append(args, branch)...)
	return err
}

func headSHA(ctx context.Context, dir string) (string, error) {
	return run(ctx, dir, "git", "rev-parse", "HEAD")
}

// syncWorktree brings dir up to origin/branch and returns the new HEAD.
// Fast-forward when possible; hard-reset only if HEAD is still exactly where we
// last synced it (i.e. a force-push and no local work). Anything else is a conflict.
func syncWorktree(ctx context.Context, dir, branch, syncedSHA string) (string, error) {
	if err := fetchBranch(ctx, dir, branch); err != nil {
		return "", err
	}
	// Untracked files don't count: wtp hooks copy untracked config into worktrees.
	status, err := run(ctx, dir, "git", "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "", &ConflictError{Reason: "dirty"}
	}
	remote := "origin/" + branch
	if _, err := run(ctx, dir, "git", "merge", "--ff-only", "--quiet", remote); err == nil {
		return headSHA(ctx, dir)
	}
	head, err := headSHA(ctx, dir)
	if err != nil {
		return "", err
	}
	if syncedSHA == "" || head != syncedSHA {
		return "", &ConflictError{Reason: "diverged"}
	}
	if _, err := run(ctx, dir, "git", "reset", "--hard", "--quiet", remote); err != nil {
		return "", err
	}
	return headSHA(ctx, dir)
}
