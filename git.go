package main

import (
	"context"
	"fmt"
	"strings"
)

// ConflictError means a worktree can't be synced without a human (or hook).
type ConflictError struct{ Reason string } // "dirty" | "ahead" | "diverged" | "untracked-collision"

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

// validBranch rejects branch names (from GitHub, i.e. untrusted) that git
// wouldn't accept or that could be parsed as a flag by git/wtp.
func validBranch(ctx context.Context, name string) bool {
	if name == "" || strings.HasPrefix(name, "-") {
		return false
	}
	_, err := run(ctx, "", "git", "check-ref-format", "--branch", name)
	return err == nil
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
//   - behind: fast-forward (a collision with untracked files is a conflict)
//   - ahead (local unpushed commits): conflict, never recorded as synced
//   - diverged: hard-reset only if HEAD is still exactly where we last synced it
//     (a force-push with no local work); otherwise conflict.
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
	head, err := headSHA(ctx, dir)
	if err != nil {
		return "", err
	}
	remoteSHA, err := run(ctx, dir, "git", "rev-parse", remote)
	if err != nil {
		return "", err
	}
	isAncestor := func(a, b string) bool {
		_, err := run(ctx, dir, "git", "merge-base", "--is-ancestor", a, b)
		return err == nil
	}
	switch {
	case head == remoteSHA:
		return head, nil
	case isAncestor(head, remoteSHA):
		if _, err := run(ctx, dir, "git", "merge", "--ff-only", "--quiet", remote); err != nil {
			return "", &ConflictError{Reason: "untracked-collision"}
		}
		return headSHA(ctx, dir)
	case isAncestor(remoteSHA, head):
		return "", &ConflictError{Reason: "ahead"}
	case syncedSHA != "" && head == syncedSHA:
		if _, err := run(ctx, dir, "git", "reset", "--hard", "--quiet", remote); err != nil {
			return "", err
		}
		return headSHA(ctx, dir)
	default:
		return "", &ConflictError{Reason: "diverged"}
	}
}
