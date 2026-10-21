package tracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/gittest"
	"github.com/reneruck/waterboy/internal/worktree"
)

func newTestTracker(t *testing.T, gh github.Client, tracked ...*Tracked) *Tracker {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{User: "me", Org: "ethyca", ReposRoot: filepath.Join(dir, "repos"), StateFile: filepath.Join(dir, "state.json")}
	os.Mkdir(cfg.ReposRoot, 0o755)
	st := &State{PRs: map[string]*Tracked{}}
	for _, tr := range tracked {
		if tr.Worktree == "" {
			tr.Worktree = dir // must exist, or Tick treats it as deleted by hand
		}
		st.PRs[tr.Key()] = tr
	}
	return New(cfg, gh, st)
}

var refA = github.PRRef{Repo: "ethyca/a", Number: 1}

func TestTickCandidatesErrorDropsNothing(t *testing.T) {
	gh := &github.Fake{CandErr: errors.New("offline")}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, CreatedWorktree: true})
	if err := tr.Tick(context.Background()); err == nil {
		t.Fatal("want error")
	}
	if tr.state.PRs[refA.Key()] == nil || gh.PRCalls != 0 {
		t.Fatalf("state touched: %+v calls=%d", tr.state.PRs, gh.PRCalls)
	}
}

func TestTickPRErrorKeepsTracked(t *testing.T) {
	gh := &github.Fake{PRErr: errors.New("502")}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, CreatedWorktree: true})
	tr.Tick(context.Background())
	got := tr.state.PRs[refA.Key()]
	if got == nil || got.LastError == "" {
		t.Fatalf("got %+v", got)
	}
}

func TestTickDropsClosedReused(t *testing.T) {
	gh := &github.Fake{Cands: []github.PRRef{refA}, PRs: map[string]github.PR{refA.Key(): {PRRef: refA, State: "closed"}}}
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
	fork := github.PRRef{Repo: "ethyca/fidesplus", Number: 2}
	noClone := github.PRRef{Repo: "ethyca/absent", Number: 3}
	gh := &github.Fake{Cands: []github.PRRef{fork, noClone}, PRs: map[string]github.PR{
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
	gh := &github.Fake{Cands: []github.PRRef{refA}, PRs: map[string]github.PR{refA.Key(): {
		PRRef: refA, State: "open", HeadRepo: "ethyca/a", HeadSHA: "a",
		Comments: []github.Comment{{User: "alice", CreatedAt: at(2)}},
	}}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadSHA: "a", SyncedSHA: "a", AnnouncedSHA: "a", LastSeenAt: at(1)})
	rec := record(tr)
	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	got := tr.state.PRs[refA.Key()]
	if !got.LastSeenAt.Equal(at(2)) || got.UnannouncedReplies != 0 {
		t.Fatalf("got %+v", got)
	}
	rec.want(t, "updated a commits=false replies=1")
}

func TestTickKeepsRequestedPRMissingFromSearch(t *testing.T) {
	gh := &github.Fake{PRs: map[string]github.PR{refA.Key(): {PRRef: refA, State: "open", HeadSHA: "a", RequestedReviewers: []string{"me"}}}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadSHA: "a", SyncedSHA: "a", CreatedWorktree: true})
	tr.Tick(context.Background())
	if tr.state.PRs[refA.Key()] == nil {
		t.Fatal("dropped on search lag")
	}
}

// Review #4: a persistent conflict runs the hook once per PR head, not every tick.
func TestSyncHookOncePerHead(t *testing.T) {
	_, clone := gittest.Setup(t)
	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("dirty"), 0o644)
	runs := filepath.Join(t.TempDir(), "runs")
	tr := newTestTracker(t, &github.Fake{})
	tr.cfg.Hooks = map[string]string{"on_conflict": "echo x >> " + runs}
	tr.cfg.HookTimeout = config.Duration{Duration: time.Minute}
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
	_, clone := gittest.Setup(t)
	gh := &github.Fake{}
	tr := newTestTracker(t, gh)
	rec := record(tr)
	tr.cfg.ReposRoot = filepath.Dir(clone)
	ref := github.PRRef{Repo: "ethyca/clone", Number: 1}
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Git(t, clone, "worktree", "add", "-q", "-b", "pr-branch", wt)
	os.RemoveAll(wt)
	tr.state.PRs[ref.Key()] = &Tracked{PRRef: ref, HeadRef: "pr-branch", Worktree: wt, CreatedWorktree: true, CreatedBranch: true}

	if err := tr.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tr.state.PRs) != 0 || gh.PRCalls != 0 {
		t.Fatalf("still tracked: %+v calls=%d", tr.state.PRs, gh.PRCalls)
	}
	rec.want(t, "dropped worktree deleted outside waterboy")
	if worktree.BranchExists(context.Background(), clone, "pr-branch") {
		t.Fatal("created branch with nothing unpushed should be deleted")
	}
}

func TestTickKeepsUnpushedBranchOfDeletedWorktree(t *testing.T) {
	_, clone := gittest.Setup(t)
	tr := newTestTracker(t, &github.Fake{})
	tr.cfg.ReposRoot = filepath.Dir(clone)
	ref := github.PRRef{Repo: "ethyca/clone", Number: 1}
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Git(t, clone, "worktree", "add", "-q", "-b", "pr-branch", wt)
	gittest.Commit(t, wt, "local.txt", "unpushed review notes")
	os.RemoveAll(wt)
	tr.state.PRs[ref.Key()] = &Tracked{PRRef: ref, HeadRef: "pr-branch", Worktree: wt, CreatedWorktree: true, CreatedBranch: true}

	tr.Tick(context.Background())
	if len(tr.state.PRs) != 0 {
		t.Fatalf("still tracked: %+v", tr.state.PRs)
	}
	if !worktree.BranchExists(context.Background(), clone, "pr-branch") {
		t.Fatal("branch with unpushed commits must be kept")
	}
}

// recorder captures tracker events as short strings.
type recorder struct{ got []string }

func record(tr *Tracker) *recorder {
	r := &recorder{}
	tr.Events = r
	return r
}

func (r *recorder) Tracked(tr Tracked) { r.got = append(r.got, "tracked "+tr.HeadSHA) }
func (r *recorder) Updated(tr Tracked, commits bool, replies int) {
	r.got = append(r.got, fmt.Sprintf("updated %s commits=%v replies=%d", tr.HeadSHA, commits, replies))
}
func (r *recorder) Reclaim(Tracked)               { r.got = append(r.got, "reclaim") }
func (r *recorder) Dropped(_ Tracked, why string) { r.got = append(r.got, "dropped "+why) }

func (r *recorder) want(t *testing.T, want ...string) {
	t.Helper()
	if !slices.Equal(r.got, want) {
		t.Fatalf("events = %q, want %q", r.got, want)
	}
}

func openPR(head string) github.PR {
	return github.PR{PRRef: refA, State: "open", HeadRepo: "ethyca/a", HeadRef: "feature/x", HeadSHA: head, RequestedReviewers: []string{"me"}}
}

func TestTickAnnouncesTrackedOnce(t *testing.T) {
	gh := &github.Fake{Cands: []github.PRRef{refA}, PRs: map[string]github.PR{refA.Key(): openPR("a")}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadSHA: "a", SyncedSHA: "a"})
	rec := record(tr)
	tr.Tick(context.Background())
	tr.Tick(context.Background())
	rec.want(t, "tracked a")
}

// New commits on a PR whose worktree a review is using: the tick doesn't sync,
// asks for the worktree back, and announces only once it has synced.
func TestTickDefersBusySyncThenAnnounces(t *testing.T) {
	up, clone := gittest.Setup(t)
	old := gittest.Git(t, clone, "rev-parse", "HEAD")
	head := gittest.Commit(t, up, "a.txt", "three")
	gh := &github.Fake{Cands: []github.PRRef{refA}, PRs: map[string]github.PR{refA.Key(): openPR(head)}}
	tr := newTestTracker(t, gh, &Tracked{PRRef: refA, HeadRef: "feature/x", HeadSHA: old, SyncedSHA: old, AnnouncedSHA: old, Worktree: clone})
	rec := record(tr)
	ctx := context.Background()

	unlock, ok := tr.Locks.TryLock(refA.Key())
	if !ok {
		t.Fatal("lock")
	}
	tr.Tick(ctx)
	if got, _ := worktree.HeadSHA(ctx, clone); got != old {
		t.Fatal("synced a busy worktree")
	}
	rec.want(t, "reclaim")

	unlock()
	tr.Tick(ctx)
	if got, _ := worktree.HeadSHA(ctx, clone); got != head {
		t.Fatalf("not synced: %s", got)
	}
	rec.want(t, "reclaim", "updated "+head+" commits=true replies=0")
}

func TestTickDropWaitsForBusyWorktree(t *testing.T) {
	closed := github.PR{PRRef: refA, State: "closed"}
	tr := newTestTracker(t, &github.Fake{PRs: map[string]github.PR{refA.Key(): closed}}, &Tracked{PRRef: refA})
	rec := record(tr)
	unlock, _ := tr.Locks.TryLock(refA.Key())
	tr.Tick(context.Background())
	if tr.state.PRs[refA.Key()] == nil {
		t.Fatal("dropped while busy")
	}
	unlock()
	tr.Tick(context.Background())
	if tr.state.PRs[refA.Key()] != nil {
		t.Fatal("not dropped")
	}
	rec.want(t, "reclaim", "dropped closed")
}

func TestLocks(t *testing.T) {
	var l Locks
	unlock, ok := l.TryLock("a")
	if !ok {
		t.Fatal("first TryLock")
	}
	if _, ok := l.TryLock("a"); ok {
		t.Fatal("second TryLock should fail")
	}
	if u, ok := l.TryLock("b"); !ok {
		t.Fatal("other id")
	} else {
		u()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := l.Lock(ctx, "a"); err == nil {
		t.Fatal("Lock should give up when ctx ends")
	}
	unlock()
	u, err := l.Lock(context.Background(), "a")
	if err != nil {
		t.Fatal(err)
	}
	u()
}

func TestForget(t *testing.T) {
	tr := newTestTracker(t, &github.Fake{}, &Tracked{PRRef: refA}, &Tracked{PRRef: github.PRRef{Repo: "ethyca/b", Number: 2}})
	ctx := context.Background()
	if err := tr.Forget(ctx, refA.Key()); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := LoadState(tr.cfg.StateFile)
	if tr.state.PRs[refA.Key()] != nil || len(tr.state.PRs) != 1 || len(reloaded.PRs) != 1 {
		t.Fatalf("not forgotten: %+v", tr.state.PRs)
	}
	if err := tr.Forget(ctx, "ethyca/nope#9"); err != nil {
		t.Fatalf("unknown PR: %v", err)
	}

	// A worktree still in use is waited for, not removed from under it.
	b := github.PRRef{Repo: "ethyca/b", Number: 2}.Key()
	unlock, _ := tr.Locks.TryLock(b)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if err := tr.Forget(short, b); err == nil || tr.state.PRs[b] == nil {
		t.Fatalf("forgot a locked PR: %v", err)
	}
	unlock()
}
