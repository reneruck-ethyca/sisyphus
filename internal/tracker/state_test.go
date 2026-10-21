package tracker

import (
	"github.com/reneruck/waterboy/internal/github"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "state.json")
	s := &State{PRs: map[string]*Tracked{"ethyca/a#1": {
		PRRef: github.PRRef{Repo: "ethyca/a", Number: 1}, HeadRef: "feature/x", HeadSHA: "abc", SyncedSHA: "abc",
		Worktree: "/w", CreatedWorktree: true, CreatedBranch: true, TrackedAt: at(1), LastSeenAt: at(2),
		PendingUpdates: true, LastError: "boom",
	}}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(p)
	if err != nil || !reflect.DeepEqual(got, s) {
		t.Errorf("got %+v %v", got.PRs["ethyca/a#1"], err)
	}
}

func TestLoadStateMissing(t *testing.T) {
	s, err := LoadState(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil || s.PRs == nil || len(s.PRs) != 0 {
		t.Errorf("got %+v %v", s, err)
	}
}

func TestLoadStateCorrupt(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	os.WriteFile(p, []byte("{nope"), 0o644)
	if _, err := LoadState(p); err == nil {
		t.Fatal("corrupt state must error, not reset (would orphan worktrees)")
	}
}
