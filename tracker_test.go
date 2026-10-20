package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
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
