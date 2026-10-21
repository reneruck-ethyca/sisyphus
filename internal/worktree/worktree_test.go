package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/gittest"
	"github.com/reneruck/waterboy/internal/proc"
)

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
	up, clone := gittest.Setup(t)
	old := gittest.Git(t, clone, "rev-parse", "HEAD")
	want := gittest.Commit(t, up, "b.txt", "three")
	got, err := Sync(context.Background(), clone, "feature/x", old)
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncForcePushResets(t *testing.T) {
	up, clone := gittest.Setup(t)
	synced := gittest.Git(t, clone, "rev-parse", "HEAD")
	gittest.Git(t, up, "commit", "-q", "--amend", "-m", "amended")
	want := gittest.Git(t, up, "rev-parse", "HEAD")
	got, err := Sync(context.Background(), clone, "feature/x", synced)
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncDirtyConflicts(t *testing.T) {
	up, clone := gittest.Setup(t)
	gittest.Commit(t, up, "b.txt", "three")
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("local edit"), 0o644)
	_, err := Sync(context.Background(), clone, "feature/x", gittest.Git(t, clone, "rev-parse", "HEAD"))
	wantConflict(t, err, "dirty")
}

func TestSyncIgnoresUntracked(t *testing.T) {
	up, clone := gittest.Setup(t)
	want := gittest.Commit(t, up, "b.txt", "three")
	os.WriteFile(filepath.Join(clone, ".env"), []byte("copied by wtp hook"), 0o644)
	got, err := Sync(context.Background(), clone, "feature/x", "")
	if err != nil || got != want {
		t.Fatalf("got %q %v want %q", got, err, want)
	}
}

func TestSyncDivergedConflicts(t *testing.T) {
	up, clone := gittest.Setup(t)
	synced := gittest.Git(t, clone, "rev-parse", "HEAD")
	gittest.Commit(t, clone, "c.txt", "local work")
	gittest.Git(t, up, "commit", "-q", "--amend", "-m", "amended")
	_, err := Sync(context.Background(), clone, "feature/x", synced)
	wantConflict(t, err, "diverged")
}

func TestSyncReusedNeverResets(t *testing.T) {
	up, clone := gittest.Setup(t)
	before := gittest.Git(t, clone, "rev-parse", "HEAD")
	gittest.Git(t, up, "commit", "-q", "--amend", "-m", "amended")
	_, err := Sync(context.Background(), clone, "feature/x", "")
	wantConflict(t, err, "diverged")
	if after := gittest.Git(t, clone, "rev-parse", "HEAD"); after != before {
		t.Fatalf("reused worktree was moved: %s -> %s", before, after)
	}
}

func TestValidBranch(t *testing.T) {
	for name, want := range map[string]bool{"feature/x": true, "ENG-1": true, "--force": false, "-x": false, "a..b": false, "": false} {
		if got := ValidBranch(context.Background(), name); got != want {
			t.Errorf("%q: got %v want %v", name, got, want)
		}
	}
}

// Review #1: a no-op ff while the user is ahead must not record their commit as
// synced, or a later force-push would hard-reset it away.
func TestSyncLocalAheadNeverReset(t *testing.T) {
	up, clone := gittest.Setup(t)
	synced := gittest.Git(t, clone, "rev-parse", "HEAD")
	local := gittest.Commit(t, clone, "c.txt", "local work")
	_, err := Sync(context.Background(), clone, "feature/x", synced)
	wantConflict(t, err, "ahead")
	gittest.Git(t, up, "commit", "-q", "--amend", "-m", "amended")
	_, err = Sync(context.Background(), clone, "feature/x", synced)
	wantConflict(t, err, "diverged")
	if head := gittest.Git(t, clone, "rev-parse", "HEAD"); head != local {
		t.Fatalf("local commit lost: HEAD %s want %s", head, local)
	}
}

// Review #2: an incoming file colliding with an untracked local file must not
// be resolved by reset --hard.
func TestSyncUntrackedCollision(t *testing.T) {
	up, clone := gittest.Setup(t)
	synced := gittest.Git(t, clone, "rev-parse", "HEAD")
	os.WriteFile(filepath.Join(clone, "new.txt"), []byte("MINE"), 0o644)
	gittest.Commit(t, up, "new.txt", "THEIRS")
	_, err := Sync(context.Background(), clone, "feature/x", synced)
	wantConflict(t, err, "untracked-collision")
	if b, _ := os.ReadFile(filepath.Join(clone, "new.txt")); string(b) != "MINE" {
		t.Fatalf("untracked file clobbered: %q", b)
	}
}

// Review #5: a hung command must not block a tick forever.
func TestRunTimeout(t *testing.T) {
	old := proc.Timeout
	proc.Timeout = 100 * time.Millisecond
	defer func() { proc.Timeout = old }()
	start := time.Now()
	if _, err := proc.Run(context.Background(), "", "sleep", "5"); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
}

func TestResetAndCommonDir(t *testing.T) {
	ctx := context.Background()
	_, clone := gittest.Setup(t)
	head := gittest.Commit(t, clone, ".gitignore", "deps/")
	gittest.Commit(t, clone, "b.txt", "reviewer commit")
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("edited"), 0o644)
	os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(clone, "deps"), 0o755)
	os.WriteFile(filepath.Join(clone, "deps", "lib"), []byte("x"), 0o644)

	if err := Reset(ctx, clone, head); err != nil {
		t.Fatal(err)
	}
	if got, _ := HeadSHA(ctx, clone); got != head {
		t.Fatalf("HEAD %s, want %s", got, head)
	}
	if st := gittest.Git(t, clone, "status", "--porcelain", "--ignored"); st != "!! deps/" {
		t.Fatalf("status after reset: %q", st)
	}
	dir, err := CommonDir(ctx, clone)
	if want, _ := filepath.EvalSymlinks(filepath.Join(clone, ".git")); err != nil || dir != want {
		t.Fatalf("CommonDir = %q, %v; want %q", dir, err, want)
	}
}
