package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := run(context.Background(), dir, "git", args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func commit(t *testing.T, dir, file, content string) string {
	t.Helper()
	os.WriteFile(filepath.Join(dir, file), []byte(content), 0o644)
	git(t, dir, "add", file)
	git(t, dir, "commit", "-qm", content)
	return git(t, dir, "rev-parse", "HEAD")
}

// setup returns an upstream repo and a clone with feature/x checked out.
func setup(t *testing.T) (upstream, clone string) {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_AUTHOR_NAME", "t")
	t.Setenv("GIT_AUTHOR_EMAIL", "t@t")
	t.Setenv("GIT_COMMITTER_NAME", "t")
	t.Setenv("GIT_COMMITTER_EMAIL", "t@t")
	root := t.TempDir()
	upstream, clone = filepath.Join(root, "up"), filepath.Join(root, "clone")
	os.Mkdir(upstream, 0o755)
	git(t, upstream, "init", "-q", "-b", "main")
	commit(t, upstream, "a.txt", "one")
	git(t, upstream, "checkout", "-qb", "feature/x")
	commit(t, upstream, "a.txt", "two")
	git(t, root, "clone", "-q", upstream, clone)
	git(t, clone, "checkout", "-q", "feature/x")
	return upstream, clone
}

func wantConflict(t *testing.T, err error, reason string) {
	t.Helper()
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.Reason != reason {
		t.Fatalf("want ConflictError %q, got %v", reason, err)
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := "worktree /r\nHEAD 111\nbranch refs/heads/main\n\nworktree /w/feature/x\nHEAD 222\nbranch refs/heads/feature/x\n\nworktree /d\nHEAD 333\ndetached\n"
	if got := parseWorktreeList(out, "feature/x"); got != "/w/feature/x" {
		t.Errorf("got %q", got)
	}
	if got := parseWorktreeList(out, "nope"); got != "" {
		t.Errorf("got %q", got)
	}
}

func TestSyncFastForward(t *testing.T) {
	up, clone := setup(t)
	old := git(t, clone, "rev-parse", "HEAD")
	want := commit(t, up, "b.txt", "three")
	got, err := syncWorktree(context.Background(), clone, "feature/x", old)
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncForcePushResets(t *testing.T) {
	up, clone := setup(t)
	synced := git(t, clone, "rev-parse", "HEAD")
	git(t, up, "commit", "-q", "--amend", "-m", "amended")
	want := git(t, up, "rev-parse", "HEAD")
	got, err := syncWorktree(context.Background(), clone, "feature/x", synced)
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncDirtyConflicts(t *testing.T) {
	up, clone := setup(t)
	commit(t, up, "b.txt", "three")
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local edit"), 0o644)
	_, err := syncWorktree(context.Background(), clone, "feature/x", git(t, clone, "rev-parse", "HEAD"))
	wantConflict(t, err, "dirty")
}

func TestSyncIgnoresUntracked(t *testing.T) {
	up, clone := setup(t)
	want := commit(t, up, "b.txt", "three")
	os.WriteFile(filepath.Join(clone, ".env"), []byte("copied by wtp hook"), 0o644)
	got, err := syncWorktree(context.Background(), clone, "feature/x", "")
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncDivergedConflicts(t *testing.T) {
	up, clone := setup(t)
	synced := git(t, clone, "rev-parse", "HEAD")
	commit(t, clone, "c.txt", "local work")
	git(t, up, "commit", "-q", "--amend", "-m", "amended")
	_, err := syncWorktree(context.Background(), clone, "feature/x", synced)
	wantConflict(t, err, "diverged")
}

func TestSyncReusedNeverResets(t *testing.T) {
	up, clone := setup(t)
	before := git(t, clone, "rev-parse", "HEAD")
	git(t, up, "commit", "-q", "--amend", "-m", "amended")
	_, err := syncWorktree(context.Background(), clone, "feature/x", "")
	wantConflict(t, err, "diverged")
	if after := git(t, clone, "rev-parse", "HEAD"); after != before {
		t.Fatalf("reused worktree was moved: %s -> %s", before, after)
	}
}

func TestValidBranch(t *testing.T) {
	for name, want := range map[string]bool{"feature/x": true, "ENG-1": true, "--force": false, "-x": false, "a..b": false, "": false} {
		if got := validBranch(context.Background(), name); got != want {
			t.Errorf("%q: got %v want %v", name, got, want)
		}
	}
}
