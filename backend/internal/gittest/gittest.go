// Package gittest builds throwaway git repos for tests.
package gittest

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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

// isolate keeps git from the user's config and gives commits an identity, for
// the whole test binary: the same for every test, so they may run in parallel
// (t.Setenv would forbid that).
var isolate = sync.OnceFunc(func() {
	for k, v := range map[string]string{"GIT_CONFIG_GLOBAL": "/dev/null", "GIT_AUTHOR_NAME": "t", "GIT_AUTHOR_EMAIL": "t@t",
		"GIT_COMMITTER_NAME": "t", "GIT_COMMITTER_EMAIL": "t@t"} {
		if err := os.Setenv(k, v); err != nil {
			panic(err)
		}
	}
})

// Setup returns an upstream repo and a clone with feature/x checked out.
func Setup(t *testing.T) (upstream, clone string) {
	t.Helper()
	isolate()
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
