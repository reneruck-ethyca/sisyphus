package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeGitHub struct {
	cands   []PRRef
	candErr error
	prs     map[string]PR
	prErr   error
	prCalls int
}

func (f *fakeGitHub) CurrentUser(context.Context) (string, error) { return "me", nil }
func (f *fakeGitHub) Candidates(context.Context, string, string) ([]PRRef, error) {
	return f.cands, f.candErr
}
func (f *fakeGitHub) PR(_ context.Context, ref PRRef) (PR, error) {
	f.prCalls++
	return f.prs[ref.Key()], f.prErr
}

func newTestTracker(t *testing.T, gh GitHub, tracked ...*Tracked) *Tracker {
	t.Helper()
	dir := t.TempDir()
	cfg := Config{User: "me", Org: "ethyca", ReposRoot: filepath.Join(dir, "repos"), StateFile: filepath.Join(dir, "state.json")}
	os.Mkdir(cfg.ReposRoot, 0o755)
	st := &State{PRs: map[string]*Tracked{}}
	for _, tr := range tracked {
		if tr.Worktree == "" {
			tr.Worktree = dir // must exist, or Tick treats it as deleted by hand
		}
		st.PRs[tr.Key()] = tr
	}
	return NewTracker(cfg, gh, st)
}

var refA = PRRef{"ethyca/a", 1}

func TestTickCandidatesErrorDropsNothing(t *testing.T) {
	gh := &fakeGitHub{candErr: errors.New("offline")}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, CreatedWorktree: true})
	if err := tr.Tick(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if tr.state.PRs[refA.Key()] == nil || gh.prCalls != 0 {
		t.Fatalf("state touched: %+v calls=%d", tr.state.PRs, gh.prCalls)
	}
}

func TestTickPRErrorKeepsTracked(t *testing.T) {
	gh := &fakeGitHub{prErr: errors.New("502")}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, CreatedWorktree: true})
	tr.Tick(context.Background())
	got := tr.state.PRs[refA.Key()]
	if got == nil || got.LastError == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestTickDropsClosedReused(t *testing.T) {
	gh := &fakeGitHub{cands: []PRRef{refA}, prs: map[string]PR{refA.Key(): {PRRef: refA, State: "closed"}}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, CreatedWorktree: false})
	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadState(tr.cfg.StateFile)
	if err != nil || len(tr.state.PRs) != 0 || len(reloaded.PRs) != 0 {
		t.Fatalf("not dropped: %+v %+v %v", tr.state.PRs, reloaded.PRs, err)
	}
}

func TestTickSkipsForkAndMissingClone(t *testing.T) {
	fork := PRRef{"ethyca/fidesplus", 2}
	noClone := PRRef{"ethyca/absent", 3}
	gh := &fakeGitHub{cands: []PRRef{fork, noClone}, prs: map[string]PR{
		fork.Key():    {PRRef: fork, State: "open", HeadRepo: "x/fidesplus"},
		noClone.Key(): {PRRef: noClone, State: "open", HeadRepo: "ethyca/absent"},
	}}
	tr := newTestTracker(t, gh)
	os.Mkdir(filepath.Join(tr.cfg.ReposRoot, "fidesplus"), 0o755)
	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tr.state.PRs) != 0 {
		t.Fatalf("tracked: %+v", tr.state.PRs)
	}
}

func TestTickRecordsNewReply(t *testing.T) {
	gh := &fakeGitHub{cands: []PRRef{refA}, prs: map[string]PR{refA.Key(): {
		PRRef: refA, State: "open", HeadRepo: "ethyca/a", HeadSHA: "a",
		Comments: []Comment{{User: "alice", CreatedAt: at(2)}},
	}}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadSHA: "a", SyncedSHA: "a", LastSeenAt: at(1)})
	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := tr.state.PRs[refA.Key()]
	if !got.PendingUpdates || !got.LastSeenAt.Equal(at(2)) {
		t.Fatalf("got %+v", got)
	}
}

func TestTickKeepsRequestedPRMissingFromSearch(t *testing.T) {
	gh := &fakeGitHub{prs: map[string]PR{refA.Key(): {PRRef: refA, State: "open", HeadSHA: "a", RequestedReviewers: []string{"me"}}}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadSHA: "a", SyncedSHA: "a", CreatedWorktree: true})
	tr.Tick(context.Background())
	if tr.state.PRs[refA.Key()] == nil {
		t.Fatal("dropped on search lag")
	}
}

// Review #4: a persistent conflict runs the hook once per PR head, not every tick.
func TestSyncHookOncePerHead(t *testing.T) {
	_, clone := setup(t)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("dirty"), 0o644)
	runs := filepath.Join(t.TempDir(), "runs")
	tr := newTestTracker(t, &fakeGitHub{})
	tr.cfg.Hooks = map[string]string{"on_conflict": "echo x >> " + runs}
	tr.cfg.HookTimeout = Duration{time.Minute}
	tracked := &Tracked{PRRef: refA, HeadRef: "feature/x", HeadSHA: "h1", Worktree: clone}
	count := func() int { b, _ := os.ReadFile(runs); return strings.Count(string(b), "x") }
	tr.sync(context.Background(), tracked)
	tr.sync(context.Background(), tracked)
	if count() != maxHookRuns || tracked.LastError == "" {
		t.Fatalf("hook runs %d, err %q", count(), tracked.LastError)
	}
	tracked.HeadSHA = "h2"
	tr.sync(context.Background(), tracked)
	if count() != 2*maxHookRuns {
		t.Fatalf("new head should re-run hook: %d", count())
	}
}

// A worktree the user deleted by hand is forgotten (and our branch removed if it
// holds nothing unpushed) instead of leaving the PR stuck; discover re-tracks it.
func TestTickForgetsManuallyDeletedWorktree(t *testing.T) {
	_, clone := setup(t)
	gh := &fakeGitHub{}
	tr := newTestTracker(t, gh)
	tr.cfg.ReposRoot = filepath.Dir(clone)
	ref := PRRef{"ethyca/clone", 1}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, clone, "worktree", "add", "-q", "-b", "pr-branch", wt)
	os.RemoveAll(wt)
	tr.state.PRs[ref.Key()] = &Tracked{PRRef: ref, HeadRef: "pr-branch", Worktree: wt, CreatedWorktree: true, CreatedBranch: true}

	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tr.state.PRs) != 0 || gh.prCalls != 0 {
		t.Fatalf("still tracked: %+v calls=%d", tr.state.PRs, gh.prCalls)
	}
	if branchExists(context.Background(), clone, "pr-branch") {
		t.Fatal("created branch with nothing unpushed should be deleted")
	}
}

func TestTickKeepsUnpushedBranchOfDeletedWorktree(t *testing.T) {
	_, clone := setup(t)
	tr := newTestTracker(t, &fakeGitHub{})
	tr.cfg.ReposRoot = filepath.Dir(clone)
	ref := PRRef{"ethyca/clone", 1}
	wt := filepath.Join(t.TempDir(), "wt")
	git(t, clone, "worktree", "add", "-q", "-b", "pr-branch", wt)
	commit(t, wt, "local.txt", "unpushed review notes")
	os.RemoveAll(wt)
	tr.state.PRs[ref.Key()] = &Tracked{PRRef: ref, HeadRef: "pr-branch", Worktree: wt, CreatedWorktree: true, CreatedBranch: true}

	tr.Tick(context.Background())
	if len(tr.state.PRs) != 0 {
		t.Fatalf("still tracked: %+v", tr.state.PRs)
	}
	if !branchExists(context.Background(), clone, "pr-branch") {
		t.Fatal("branch with unpushed commits must be kept")
	}
}
