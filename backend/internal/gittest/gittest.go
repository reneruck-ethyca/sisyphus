// Package gittest builds throwaway git repos for tests.
package gittest

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/reneruck/sisyphus/internal/proc"
)

// Git runs git in dir and fails the test on error.
func Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := proc.Run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Commit writes file and commits it, returning the new HEAD.
func Commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	Git(t, dir, "add", file)
	Git(t, dir, "commit", "-qm", content)
	return Git(t, dir, "rev-parse", "HEAD")
}

// Setup returns an upstream repo and a clone with feature/x checked out.
func Setup(t *testing.T) (upstream, clone string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root := t.TempDir()
	upstream, clone = filepath.Join(root, "up"), filepath.Join(root, "clone")
	if err := os.Mkdir(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	Git(t, upstream, "init", "-q", "-b", "main")
	Commit(t, upstream, "a.txt", "one")
	Git(t, upstream, "checkout", "-qb", "feature/x")
	Commit(t, upstream, "a.txt", "two")
	Git(t, root, "clone", "-q", upstream, clone)
	Git(t, clone, "checkout", "-q", "feature/x")
	return upstream, clone
}
