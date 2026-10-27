package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/tracker"
)

var refA = github.PRRef{Repo: "ethyca/a", Number: 1}

func init() {
	// Set once: background goroutines of earlier tests still read these.
	retryEvery = 20 * time.Millisecond
	logGrace = 10 * time.Millisecond
}

type fixture struct {
	r     *Reviewer
	rt    *container.Fake
	locks *tracker.Locks
	dir   string
	tr    tracker.Tracked // a PR whose worktree is a real clone at its head
	kicks atomic.Int32
}

func newFixture(t *testing.T, mutate ...func(*config.Review)) *fixture {
	t.Helper()
	_, clone := gittest.Setup(t)
	f := &fixture{rt: &container.Fake{}, locks: &tracker.Locks{}, dir: t.TempDir()}
	f.tr = tracker.Tracked{PRRef: refA, URL: "https://github.com/ethyca/a/pull/1", HeadRef: "feature/x",
		HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), Worktree: clone, CreatedWorktree: true}
	cfg := config.Review{Model: "opus", MaxTurns: 10, Timeout: config.Duration{Duration: time.Minute},
		MaxConcurrent: 1, Cleanup: true,
		Repos: map[string]config.RepoReview{"ethyca/a": {Image: "img:1"}, "ethyca/b": {Image: "img:1"}}}
	for _, m := range mutate {
		m(&cfg)
	}
	f.r = f.newReviewer(t, cfg)
	return f
}

func (f *fixture) newReviewer(t *testing.T, cfg config.Review) *Reviewer {
	t.Helper()
	r, err := New(cfg, f.rt, f.locks, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	r.Kick = func() { f.kicks.Add(1) }
	return r
}

func (f *fixture) start(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go f.r.Run(ctx)
}

const okResult = `{"type":"result","subtype":"success","is_error":false,"num_turns":2,"duration_ms":1000,"total_cost_usd":0.25,"session_id":"s1"}` + "\n"

// succeed ends a container the way a successful Claude run does: output, then a success result line.
func (f *fixture) succeed(id, out string) { f.rt.Finish(id, 0, out+okResult) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fixture) started(n int) func() bool {
	return func() bool { return len(f.containers()) >= n }
}

func (f *fixture) containers() []container.Info {
	infos, _ := f.rt.List(context.Background(), map[string]string{"sisyphus": "1"})
	return infos
}

// runs returns PR a's runs, newest first.
func (f *fixture) runs() []Run { return f.r.Status()[refA.Key()].Runs }

func (f *fixture) lastStatus(status string) func() bool {
	return func() bool { rs := f.runs(); return len(rs) > 0 && rs[0].Status == status }
}

func TestTrackedRunsReview(t *testing.T) {
	f := newFixture(t, func(c *config.Review) { c.Env = []string{"WB_TEST_TOKEN"} })
	t.Setenv("WB_TEST_TOKEN", "t0k")
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))

	if _, ok := f.locks.TryLock(refA.Key()); ok {
		t.Fatal("worktree not locked while the review runs")
	}
	s := f.rt.Started()[0]
	gitDir, _ := filepath.EvalSymlinks(filepath.Join(f.tr.Worktree, ".git"))
	if s.Image != "img:1" || s.Workdir != f.tr.Worktree || s.Labels["sisyphus.pr"] != refA.Key() {
		t.Fatalf("spec = %+v", s)
	}
	if s.Env["SISYPHUS_REASON"] != "new" || s.Env["SISYPHUS_HEAD_SHA"] != f.tr.HeadSHA || s.Env["SISYPHUS_OUT"] != "/out" || s.Env["WB_TEST_TOKEN"] != "t0k" {
		t.Fatalf("env = %v", s.Env)
	}
	// A clone's .git lives inside the (read/write) worktree; it's overlaid read-only
	// so the container can't plant hooks or config that host-side git would run.
	dotGit := filepath.Join(f.tr.Worktree, ".git")
	if len(s.Mounts) != 5 || s.Mounts[0] != (container.Mount{Source: f.tr.Worktree, Target: f.tr.Worktree}) ||
		s.Mounts[1] != (container.Mount{Source: dotGit, Target: dotGit, ReadOnly: true}) || s.Mounts[2].Target != "/out" {
		t.Fatalf("mounts = %+v (git dir %s)", s.Mounts, gitDir)
	}
	if st := f.r.Status()[refA.Key()]; st.State != "running" {
		t.Fatalf("state = %q", st.State)
	}

	f.succeed(f.containers()[0].ID, "looks good\n")
	waitFor(t, "run to finish", f.lastStatus(Succeeded))
	run := f.runs()[0]
	if b, _ := os.ReadFile(filepath.Join(run.Dir, "log")); string(b) != "looks good\n"+okResult+"── sisyphus: not posted: review.post is off (dry run); the review is in out/review.json\n" {
		t.Fatalf("log = %q", b)
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "run.json")); err != nil {
		t.Fatal(err)
	}
	if u, ok := f.locks.TryLock(refA.Key()); !ok {
		t.Fatal("lock not released")
	} else {
		u()
	}
	if len(f.containers()) != 0 {
		t.Fatal("finished container not removed")
	}
}

// A linked worktree's .git points into the clone, which must be mounted too.
func TestMountsCloneGitDirForLinkedWorktree(t *testing.T) {
	f := newFixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Git(t, f.tr.Worktree, "worktree", "add", "-q", "-b", "pr", wt)
	common, _ := filepath.EvalSymlinks(filepath.Join(f.tr.Worktree, ".git"))
	f.tr.Worktree = wt
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	var got []string
	for _, m := range f.rt.Started()[0].Mounts {
		got = append(got, fmt.Sprintf("%s ro=%v", m.Target, m.ReadOnly))
	}
	want := []string{wt + " ro=false", wt + "/.git ro=true", common + " ro=true", "/out ro=false",
		"/sisyphus/prompt.md ro=true", "/sisyphus/claude ro=false"}
	if !slices.Equal(got, want) {
		t.Fatalf("mounts\n got %q\nwant %q", got, want)
	}
}

func TestCleanupResetsOnlyCreatedWorktrees(t *testing.T) {
	for _, created := range []bool{true, false} {
		f := newFixture(t)
		f.tr.CreatedWorktree = created
		f.start(t)
		f.r.Tracked(f.tr)
		waitFor(t, "container start", f.started(1))
		os.WriteFile(filepath.Join(f.tr.Worktree, "a.txt"), []byte("reviewer edit"), 0o644)
		os.WriteFile(filepath.Join(f.tr.Worktree, "scratch.txt"), []byte("x"), 0o644)
		f.succeed(f.containers()[0].ID, "")
		waitFor(t, "run to finish", f.lastStatus(Succeeded))
		dirty := gittest.Git(t, f.tr.Worktree, "status", "--porcelain") != ""
		if dirty == created {
			t.Fatalf("created=%v: dirty=%v after run", created, dirty)
		}
	}
}

// The tracker found new commits but can't sync a worktree in use: the run is
// stopped, a tick is requested, and the new head's review runs once announced.
func TestReclaimStopsRunAndKicks(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))

	f.r.Reclaim(f.tr)
	waitFor(t, "run to stop", f.lastStatus(Cancelled))
	waitFor(t, "tick request", func() bool { return f.kicks.Load() == 1 })
	if !strings.Contains(f.runs()[0].Error, "reclaimed") {
		t.Fatalf("run=%+v", f.runs()[0])
	}

	next := f.tr
	next.HeadSHA = gittest.Commit(t, f.tr.Worktree, "a.txt", "pushed") // what the tick's sync does
	f.r.Updated(next, true, 0)
	waitFor(t, "second container", f.started(1))
	if s := f.rt.Started()[1]; s.Env["SISYPHUS_REASON"] != "commits" || s.Env["SISYPHUS_HEAD_SHA"] != next.HeadSHA {
		t.Fatalf("second run env = %v", s.Env)
	}
}

func TestRepliesQueueBehindRunningReview(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.r.Updated(f.tr, false, 2)
	if st := f.r.Status()[refA.Key()]; st.State != "running" || len(f.rt.Started()) != 1 {
		t.Fatalf("replies interrupted the run: %+v", st)
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "follow-up run", func() bool { return len(f.rt.Started()) == 2 })
	if f.rt.Started()[1].Env["SISYPHUS_REASON"] != "replies" || f.runs()[1].Status != Succeeded {
		t.Fatalf("follow-up = %v, first = %+v", f.rt.Started()[1].Env, f.runs()[1])
	}
}

func TestQueueKeepsStrongestReason(t *testing.T) {
	f := newFixture(t) // not started: jobs stay queued
	f.r.Tracked(f.tr)
	f.r.Updated(f.tr, false, 1)
	if q := f.r.st.Queue; len(q) != 1 || q[0].Reason != "new" {
		t.Fatalf("queue = %+v", q)
	}
	f.r.Dropped(f.tr, "closed")
	if len(f.r.st.Queue) != 0 {
		t.Fatal("dropped PR still queued")
	}
}

func TestMaxConcurrent(t *testing.T) {
	f := newFixture(t)
	other := f.tr
	other.PRRef = github.PRRef{Repo: "ethyca/b", Number: 2}
	f.start(t)
	f.r.Tracked(f.tr)
	f.r.Tracked(other)
	waitFor(t, "first container", f.started(1))
	time.Sleep(50 * time.Millisecond)
	if n := len(f.rt.Started()); n != 1 {
		t.Fatalf("%d containers running, max is 1", n)
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "second container", func() bool { return len(f.rt.Started()) == 2 })
}

func TestHeadMismatchIsAnError(t *testing.T) {
	f := newFixture(t)
	stale := f.tr
	stale.HeadSHA = "0000000000000000000000000000000000000000"
	f.start(t)
	f.r.Tracked(stale)
	waitFor(t, "run to error", f.lastStatus(Errored))
	if len(f.rt.Started()) != 0 || !strings.Contains(f.runs()[0].Error, "not the announced head") {
		t.Fatalf("started=%d run=%+v", len(f.rt.Started()), f.runs()[0])
	}
}

// Past review.timeout a run is only flagged, never stopped.
func TestOverdueIsOnlyFlagged(t *testing.T) {
	f := newFixture(t, func(c *config.Review) { c.Timeout = config.Duration{Duration: 30 * time.Millisecond} })
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	waitFor(t, "overdue flag", func() bool { rs := f.runs(); return len(rs) == 1 && rs[0].Overdue })
	if c := f.containers(); len(c) != 1 || !c[0].Running || f.runs()[0].Status != Running {
		t.Fatalf("overdue run was stopped: %+v %+v", c, f.runs()[0])
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "finish", f.lastStatus(Succeeded))
	if f.runs()[0].Overdue {
		t.Fatal("finished run still flagged overdue")
	}
}

func TestFailedExitCode(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 2, "uv sync: boom\n") // setup failed: Claude never ran
	waitFor(t, "failure", f.lastStatus(Failed))
	if r := f.runs()[0]; r.ExitCode != 2 || !strings.Contains(r.Error, "before Claude finished") {
		t.Fatalf("run = %+v", r)
	}
}

// Shutting down leaves the container running; a new process re-attaches to it.
func TestShutdownThenReattach(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	go f.r.Run(ctx)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	waitFor(t, "container id recorded", func() bool { rs := f.runs(); return len(rs) == 1 && rs[0].Container != "" })
	cancel()
	time.Sleep(50 * time.Millisecond)
	if c := f.containers(); len(c) != 1 || !c[0].Running {
		t.Fatalf("container stopped on shutdown: %+v", c)
	}

	f.locks = &tracker.Locks{} // a fresh process
	r2 := f.newReviewer(t, f.r.cfg)
	f.r = r2
	r2.Reattach(context.Background())
	if _, ok := f.locks.TryLock(refA.Key()); ok {
		t.Fatal("re-attached run must hold the worktree lock")
	}
	f.start(t)
	f.succeed(f.containers()[0].ID, "replayed\n")
	waitFor(t, "re-attached run to finish", f.lastStatus(Succeeded))
	if b, _ := os.ReadFile(filepath.Join(f.runs()[0].Dir, "log")); string(b) != "replayed\n"+okResult+"── sisyphus: not posted: review.post is off (dry run); the review is in out/review.json\n" {
		t.Fatalf("log = %q", b)
	}
}

func TestReattachLostRunIsRequeued(t *testing.T) {
	f := newFixture(t)
	f.r.st.Runs[refA.Key()] = []Run{{ID: "r1", Job: Job{PR: f.tr, Reason: "commits"}, Status: Running, Container: "gone",
		Started: time.Now(), Dir: filepath.Join(f.dir, "runs", "r1")}}
	orphan := f.rt.Seed(map[string]string{"sisyphus": "1", "sisyphus.run": "zz"}, true, 0, "")
	f.r.Reattach(context.Background())
	waitFor(t, "lost and requeued", func() bool {
		st := f.r.Status()[refA.Key()]
		return st.State == "queued" && st.Runs[0].Status == Lost
	})
	waitFor(t, "orphan removed", func() bool {
		return !slices.ContainsFunc(f.containers(), func(c container.Info) bool { return c.ID == orphan })
	})
	f.start(t)
	waitFor(t, "requeued run", f.started(1))
	if f.rt.Started()[0].Env["SISYPHUS_REASON"] != "commits" {
		t.Fatalf("env = %v", f.rt.Started()[0].Env)
	}
}

func TestQueuePersists(t *testing.T) {
	f := newFixture(t)
	f.r.Tracked(f.tr)
	r2 := f.newReviewer(t, f.r.cfg)
	if q := r2.st.Queue; len(q) != 1 || q[0].PR.Key() != refA.Key() {
		t.Fatalf("queue after reload = %+v", q)
	}
}

func TestJobsWaitForRuntime(t *testing.T) {
	f := newFixture(t)
	f.rt.SetUnavailable(errors.New("docker daemon not running"))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "runtime error", func() bool { return f.r.RuntimeError() != "" })
	if st := f.r.Status()[refA.Key()]; st.State != "queued" || len(f.rt.Started()) != 0 {
		t.Fatalf("status = %+v", st)
	}
	f.rt.SetUnavailable(nil)
	f.r.poke() // stands in for the one-minute retry
	waitFor(t, "container start", f.started(1))
	if f.r.RuntimeError() != "" {
		t.Fatal("runtime error not cleared")
	}
}

// A broken `docker wait` must not hold the worktree (and the only run slot) until
// the container happens to exit.
func TestWaitErrorReleasesWorktree(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.rt.FailWait(f.containers()[0].ID)
	waitFor(t, "run to error", f.lastStatus(Errored))
	if u, ok := f.locks.TryLock(refA.Key()); !ok {
		t.Fatal("worktree still locked")
	} else {
		u()
	}
	if len(f.containers()) != 0 {
		t.Fatal("container left running")
	}
}

// The daemon may have created the container even though `docker run` failed.
func TestStartFailureRemovesContainer(t *testing.T) {
	f := newFixture(t)
	f.rt.OnStart = func(context.Context, string) error { return errors.New("docker run: context deadline exceeded") }
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "run to error", f.lastStatus(Errored))
	if len(f.containers()) != 0 {
		t.Fatalf("orphan left: %+v", f.containers())
	}
}

// blockStart makes Start hang until its context ends, like a slow image pull.
func blockStart(f *fixture) chan struct{} {
	entered := make(chan struct{}, 1)
	f.rt.OnStart = func(ctx context.Context, _ string) error {
		entered <- struct{}{}
		<-ctx.Done()
		return ctx.Err()
	}
	return entered
}

// Quitting while the container is being started leaves the run "running" (not
// errored), so the next start re-attaches to whatever the daemon started.
func TestShutdownDuringStartReattaches(t *testing.T) {
	f := newFixture(t)
	entered := blockStart(f)
	ctx, cancel := context.WithCancel(context.Background())
	go f.r.Run(ctx)
	f.r.Tracked(f.tr)
	<-entered
	cancel()
	time.Sleep(50 * time.Millisecond)
	if rs := f.runs(); len(rs) != 1 || rs[0].Status != Running {
		t.Fatalf("runs = %+v", rs)
	}
	f.rt.OnStart = nil
	f.locks = &tracker.Locks{}
	f.r = f.newReviewer(t, f.r.cfg)
	f.r.Reattach(context.Background())
	waitFor(t, "re-attached", func() bool { return f.r.Status()[refA.Key()].State == "running" })
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "finished", f.lastStatus(Succeeded))
}

func TestReclaimBeforeStartCancelsAndKicks(t *testing.T) {
	f := newFixture(t)
	entered := blockStart(f)
	f.start(t)
	f.r.Tracked(f.tr)
	<-entered
	f.r.Reclaim(f.tr)
	waitFor(t, "cancelled", f.lastStatus(Cancelled))
	waitFor(t, "tick request", func() bool { return f.kicks.Load() == 1 })
}

// While the runtime is down, each poke must not start another retry loop.
func TestRuntimeRetryDoesNotMultiply(t *testing.T) {
	f := newFixture(t)
	f.rt.SetUnavailable(errors.New("down"))
	f.start(t)
	for range 5 {
		f.r.Tracked(f.tr)
	}
	time.Sleep(300 * time.Millisecond)
	if n := f.rt.AvailableCalls(); n > 30 {
		t.Fatalf("%d Available calls in 300ms: retry timers are piling up", n)
	}
}

// At login Docker usually isn't up yet: re-attach waits for it, holding the
// worktree lock, instead of marking every running review lost.
func TestReattachWaitsForRuntime(t *testing.T) {
	f := newFixture(t)
	id := f.rt.Seed(map[string]string{"sisyphus": "1", "sisyphus.run": "r1"}, true, 0, "")
	f.r.st.Runs[refA.Key()] = []Run{{ID: "r1", Job: Job{PR: f.tr, Reason: "new"}, Status: Running, Container: id,
		Started: time.Now(), Dir: filepath.Join(f.dir, "runs", "r1")}}
	f.rt.SetUnavailable(errors.New("docker not running"))
	f.r.Reattach(context.Background())
	if _, ok := f.locks.TryLock(refA.Key()); ok {
		t.Fatal("worktree unlocked while waiting to re-attach")
	}
	time.Sleep(60 * time.Millisecond)
	if f.runs()[0].Status != Running {
		t.Fatalf("run = %+v", f.runs()[0])
	}
	f.rt.SetUnavailable(nil)
	f.start(t)
	time.Sleep(60 * time.Millisecond)
	f.succeed(id, "ok\n")
	waitFor(t, "re-attached run to finish", f.lastStatus(Succeeded))
}

// A container that finished while sisyphus was down is a success, even if the
// run's timeout has passed by the time we look.
func TestReattachExitedPastDeadline(t *testing.T) {
	f := newFixture(t)
	id := f.rt.Seed(map[string]string{"sisyphus": "1", "sisyphus.run": "r1"}, false, 0, "done\n"+okResult)
	f.r.st.Runs[refA.Key()] = []Run{{ID: "r1", Job: Job{PR: f.tr, Reason: "new"}, Status: Running, Container: id,
		Started: time.Now().Add(-2 * time.Hour), Dir: filepath.Join(f.dir, "runs", "r1")}}
	os.MkdirAll(filepath.Join(f.dir, "runs", "r1"), 0o755)
	f.r.Reattach(context.Background())
	waitFor(t, "run to finish", func() bool { rs := f.runs(); return rs[0].Status != Running })
	if rs := f.runs(); rs[0].Status != Succeeded {
		t.Fatalf("run = %+v", rs[0])
	}
}

func TestClaudeOutputStreamsAndIsRecorded(t *testing.T) {
	f := newFixture(t)
	live, cancel := f.r.SubscribeEvents()
	defer cancel()
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	id := f.containers()[0].ID
	f.rt.Print(id, "── sisyphus: claude ──\n")
	f.rt.Print(id, `{"type":"assistant","message":{"content":[{"type":"text","text":"Reading the diff."},{"type":"tool_use","name":"Bash","input":{"command":"gh pr diff 1"}}]}}`+"\n")

	var got []string
	for len(got) < 3 {
		select {
		case e := <-live:
			if e.PR != refA.Key() || e.Run != f.runs()[0].ID {
				t.Fatalf("event for the wrong run: %+v", e)
			}
			got = append(got, e.Kind+":"+e.Text)
		case <-time.After(5 * time.Second):
			t.Fatalf("live events so far: %q", got)
		}
	}
	want := []string{"output:── sisyphus: claude ──", "text:Reading the diff.", "tool:gh pr diff 1"}
	if !slices.Equal(got, want) {
		t.Fatalf("live = %q, want %q", got, want)
	}

	f.succeed(id, "")
	waitFor(t, "finish", f.lastStatus(Succeeded))
	run := f.runs()[0]
	if run.Claude == nil || run.Claude.SessionID != "s1" || run.Claude.NumTurns != 2 || run.Claude.CostUSD != 0.25 {
		t.Fatalf("claude result = %+v", run.Claude)
	}
	evs, next, err := f.r.Events(refA.Key(), run.ID)
	// After Claude's output: what came of posting (here: a dry run).
	if err != nil || len(evs) != 5 || evs[3].Kind != "result" || evs[1].Line != 1 || next != 4 ||
		evs[4].Line != 3 || !strings.HasPrefix(evs[4].Text, "── sisyphus: not posted") {
		t.Fatalf("Events = %+v, next %d, %v", evs, next, err)
	}
}

func TestClaudeErrorResultFails(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 1, `{"type":"result","subtype":"error_max_turns","is_error":true,"num_turns":10,"session_id":"s","errors":["Reached max turns (10)"]}`+"\n")
	waitFor(t, "failure", f.lastStatus(Failed))
	if r := f.runs()[0]; !strings.Contains(r.Error, "Reached max turns") || r.Claude == nil || r.Claude.Subtype != "error_max_turns" {
		t.Fatalf("run = %+v", r)
	}
}

func TestUnconfiguredRepoIsNotReviewed(t *testing.T) {
	f := newFixture(t)
	other := f.tr
	other.PRRef = github.PRRef{Repo: "ethyca/fides", Number: 9}
	f.r.Tracked(other)
	f.r.Updated(other, true, 0)
	if len(f.r.st.Queue) != 0 {
		t.Fatalf("queued a repo without review config: %+v", f.r.st.Queue)
	}
}

// A review whose env file is missing waits in the queue, says why, and starts
// once the file exists.
func TestMissingEnvFileHoldsReviewUntilCreated(t *testing.T) {
	secrets := filepath.Join(t.TempDir(), "review.env")
	f := newFixture(t, func(c *config.Review) { c.EnvFile = secrets })
	f.start(t)
	f.r.Tracked(f.tr)
	if got := f.r.Blocked(refA.Repo); got != "env file missing: "+secrets {
		t.Fatalf("Blocked = %q", got)
	}
	time.Sleep(5 * retryEvery)
	if len(f.rt.Started()) != 0 || f.r.Status()[refA.Key()].State != "queued" {
		t.Fatalf("started %d, state %q", len(f.rt.Started()), f.r.Status()[refA.Key()].State)
	}
	os.WriteFile(secrets, nil, 0o600)
	waitFor(t, "review to start once the env file exists", f.started(1))
}

// A PR tracked while it couldn't be reviewed gets its first review later.
func TestEnsureQueuesFirstReview(t *testing.T) {
	f := newFixture(t)
	tr := f.tr
	tr.AnnouncedSHA = tr.HeadSHA
	unsynced := tracker.Tracked{PRRef: github.PRRef{Repo: "ethyca/b", Number: 2}, HeadSHA: "b2", AnnouncedSHA: "b1"}
	other := tracker.Tracked{PRRef: github.PRRef{Repo: "ethyca/zzz", Number: 3}, HeadSHA: "z", AnnouncedSHA: "z"}
	f.r.Ensure([]tracker.Tracked{tr, unsynced, other})
	f.r.Ensure([]tracker.Tracked{tr})
	if len(f.r.st.Queue) != 1 || f.r.st.Queue[0].PR.Key() != refA.Key() || f.r.st.Queue[0].Reason != "new" {
		t.Fatalf("queue = %+v", f.r.st.Queue)
	}
}

func TestSpecForConfiguredRepo(t *testing.T) {
	settings := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(settings, []byte(`{"enabledPlugins":{}}`), 0o644)
	prompt := filepath.Join(t.TempDir(), "p.md")
	os.WriteFile(prompt, []byte("Review {{.PR.URL}} in {{.Workdir}} ({{.Reason}})"), 0o644)
	secrets, aEnv := filepath.Join(t.TempDir(), "secrets.env"), filepath.Join(t.TempDir(), "a.env")
	os.WriteFile(secrets, nil, 0o600)
	os.WriteFile(aEnv, nil, 0o600)
	f := newFixture(t, func(c *config.Review) {
		c.EnvFile, c.ClaudeSettings, c.PluginsVolume, c.MaxBudgetUSD = secrets, settings, "wb-plugins", 2.5
		c.Repos["ethyca/a"] = config.RepoReview{Image: "reviewer:a", Workdir: "/a", EnvFile: aEnv, PromptFile: prompt,
			Env: map[string]string{"PYTHONPATH": "/a/src"}, Setup: "uv sync", Cache: []string{".venv", "clients/node_modules", "/home/u/.cache/uv"}}
	})
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	s := f.rt.Started()[0]
	run := f.runs()[0]

	if s.Image != "reviewer:a" || s.Workdir != "/a" || s.Env["PYTHONPATH"] != "/a/src" || s.Env["CLAUDE_CONFIG_DIR"] != "/sisyphus/claude" ||
		s.Env["SISYPHUS_WORKTREE"] != "/a" || !slices.Equal(s.EnvFiles, []string{aEnv, secrets}) {
		t.Fatalf("spec = %+v", s)
	}
	mounts := map[string]container.Mount{}
	for _, m := range s.Mounts {
		mounts[m.Target] = m
	}
	for target, want := range map[string]container.Mount{
		"/a":                       {Source: f.tr.Worktree, Target: "/a"},
		"/a/.git":                  {Source: filepath.Join(f.tr.Worktree, ".git"), Target: "/a/.git", ReadOnly: true},
		"/a/.venv":                 {Source: "sisyphus-ethyca-a-.venv", Target: "/a/.venv"},
		"/a/clients/node_modules":  {Source: "sisyphus-ethyca-a-clients-node_modules", Target: "/a/clients/node_modules"},
		"/home/u/.cache/uv":        {Source: "sisyphus-ethyca-a--home-u-.cache-uv", Target: "/home/u/.cache/uv"}, // absolute: as is
		"/sisyphus/claude/plugins": {Source: "wb-plugins", Target: "/sisyphus/claude/plugins"},
		"/sisyphus/prompt.md":      {Source: filepath.Join(run.Dir, "prompt.md"), Target: "/sisyphus/prompt.md", ReadOnly: true},
		"/sisyphus/claude":         {Source: filepath.Join(run.Dir, "claude"), Target: "/sisyphus/claude"},
	} {
		if mounts[target] != want {
			t.Errorf("mount %s = %+v, want %+v", target, mounts[target], want)
		}
	}
	script := s.Command[2]
	for _, want := range []string{"sudo -n chown", "'/a/.venv'", "'/home/u/.cache/uv'", "'/sisyphus/claude/plugins'", "uv sync",
		"exec 'claude' '-p' '--output-format' 'stream-json' '--verbose' '--dangerously-skip-permissions' '--model' 'opus' '--max-turns' '10' '--max-budget-usd' '2.5' \"${extra[@]}\" < /sisyphus/prompt.md"} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if s.Command[0] != "bash" || strings.Index(script, "uv sync") > strings.Index(script, "exec 'claude'") {
		t.Errorf("setup must run before claude: %q", s.Command)
	}
	if b, _ := os.ReadFile(filepath.Join(run.Dir, "prompt.md")); string(b) != "Review https://github.com/ethyca/a/pull/1 in /a (new)" {
		t.Errorf("prompt.md = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(run.Dir, "claude", "settings.json")); string(b) != `{"enabledPlugins":{}}` {
		t.Errorf("settings.json = %q", b)
	}
}

func TestPreviousHeadIsLastSuccessfulReview(t *testing.T) {
	f := newFixture(t)
	f.r.st.Runs[refA.Key()] = []Run{
		{ID: "1", Status: Succeeded, Job: Job{PR: tracker.Tracked{HeadSHA: "aaa"}}},
		{ID: "2", Status: Failed, Job: Job{PR: tracker.Tracked{HeadSHA: "bbb"}}},
	}
	if got := f.r.previousHead(refA.Key()); got != "aaa" {
		t.Fatalf("previousHead = %q", got)
	}
}

func TestForget(t *testing.T) {
	f := newFixture(t)
	other := f.tr
	other.PRRef = github.PRRef{Repo: "ethyca/b", Number: 2}
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.r.Tracked(other) // queued behind it (max_concurrent 1)

	aDir := f.runs()[0].Dir
	if err := f.r.Forget(context.Background(), refA.Key()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a's run stopped and forgotten", func() bool { return len(f.runs()) == 0 && f.r.Status()[refA.Key()].State == "" })
	if _, err := os.Stat(aDir); !os.IsNotExist(err) {
		t.Fatalf("a's run dir still there: %v", err)
	}
	if u, ok := f.locks.TryLock(refA.Key()); !ok {
		t.Fatal("a's worktree still locked")
	} else {
		u()
	}
	waitFor(t, "b starts", func() bool { return f.r.Status()[other.Key()].State == "running" })

	waitFor(t, "b's container", f.started(1))
	if err := f.r.Forget(context.Background(), ""); err != nil { // everything
		t.Fatal(err)
	}
	waitFor(t, "all forgotten", func() bool { return len(f.r.Status()) == 0 })
	if len(f.r.st.Queue) != 0 || len(f.containers()) != 0 {
		t.Fatalf("queue = %+v, containers = %+v", f.r.st.Queue, f.containers())
	}
	if _, err := os.Stat(filepath.Join(f.dir, "runs")); !os.IsNotExist(err) {
		t.Fatalf("runs dir still there: %v", err)
	}
}

// fakeGH records what the reviewer posts.
type fakeGH struct {
	mu         sync.Mutex
	failCreate int      // CreateReview fails this many times first
	reviews    []string // posted payloads
	replies    []string // "id: body"
}

func (g *fakeGH) Discussion(context.Context, github.PRRef) ([]github.DiscussionFile, error) {
	return []github.DiscussionFile{{Name: "pr.json", Body: []byte(`{"title":"T"}`)}, {Name: "review_comments.json", Body: []byte(`[{"id":7}]`)}}, nil
}
func (g *fakeGH) CreateReview(_ context.Context, _ github.PRRef, file string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	b, _ := os.ReadFile(file)
	g.reviews = append(g.reviews, string(b))
	if g.failCreate > 0 {
		g.failCreate--
		return "", errors.New("HTTP 422: Unprocessable Entity (line must be part of the diff)")
	}
	return "https://github.com/ethyca/a/pull/1#pullrequestreview-1", nil
}
func (g *fakeGH) ReplyToComment(_ context.Context, _ github.PRRef, id int64, body string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.replies = append(g.replies, fmt.Sprintf("%d: %s", id, body))
	return nil
}
func (g *fakeGH) posted() ([]string, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return slices.Clone(g.reviews), slices.Clone(g.replies)
}

// runWithReview runs one review whose agent writes reviewJSON to /out/review.json.
func runWithReview(t *testing.T, gh *fakeGH, reviewJSON string, mutate ...func(*config.Review)) (*fixture, Run) {
	t.Helper()
	f := newFixture(t, append([]func(*config.Review){func(c *config.Review) { c.Post = true }}, mutate...)...)
	f.r.GitHub = gh
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	if reviewJSON != "" {
		os.WriteFile(filepath.Join(f.runs()[0].Dir, "out", "review.json"), []byte(reviewJSON), 0o644)
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "run to finish", func() bool { rs := f.runs(); return len(rs) == 1 && rs[0].Status != Running })
	return f, f.runs()[0]
}

func TestPostsReviewFromHost(t *testing.T) {
	gh := &fakeGH{}
	f, run := runWithReview(t, gh, `{"event":"REQUEST_CHANGES","body":"One bug.","comments":[{"path":"a.txt","line":1,"body":"off by one"}],
		"replies":[{"comment_id":7,"body":"Fixed in the latest push, thanks."}]}`)
	if run.Status != Succeeded || run.Posted == nil || run.Posted.URL == "" || run.Posted.Event != "REQUEST_CHANGES" || run.Posted.Comments != 1 || run.Posted.Replies != 1 {
		t.Fatalf("run = %+v posted = %+v", run, run.Posted)
	}
	reviews, replies := gh.posted()
	var p map[string]any
	json.Unmarshal([]byte(reviews[0]), &p)
	if len(reviews) != 1 || p["commit_id"] != f.tr.HeadSHA || p["event"] != "REQUEST_CHANGES" ||
		!strings.HasSuffix(p["body"].(string), "🤖 Reviewed by sisyphus") || len(p["comments"].([]any)) != 1 {
		t.Fatalf("posted %q", reviews)
	}
	if !slices.Equal(replies, []string{"7: Fixed in the latest push, thanks.\n\n🤖 Reviewed by sisyphus"}) {
		t.Fatalf("replies = %q", replies)
	}
}

// GitHub refuses inline comments outside the diff, or approving your own PR:
// fall back to one plain comment carrying everything.
func TestPostFallsBackToPlainComment(t *testing.T) {
	gh := &fakeGH{failCreate: 1}
	_, run := runWithReview(t, gh, `{"event":"APPROVE","body":"LGTM.","comments":[{"path":"a.txt","line":99,"body":"nit"}]}`)
	reviews, _ := gh.posted()
	var p map[string]any
	json.Unmarshal([]byte(reviews[1]), &p)
	if len(reviews) != 2 || p["event"] != "COMMENT" || p["comments"] != nil || !run.Posted.Fallback ||
		!strings.Contains(p["body"].(string), "a.txt:99") || !strings.Contains(p["body"].(string), "APPROVE") {
		t.Fatalf("fallback = %q, posted %+v", reviews, run.Posted)
	}
	if run.Status != Succeeded {
		t.Fatalf("run = %+v", run)
	}
}

func TestPostFailureFailsTheRun(t *testing.T) {
	_, run := runWithReview(t, &fakeGH{failCreate: 2}, `{"event":"COMMENT","body":"hi"}`)
	if run.Status != Failed || !strings.Contains(run.Error, "posting review") || run.Posted == nil || run.Posted.Error == "" {
		t.Fatalf("run = %+v posted = %+v", run, run.Posted)
	}
}

func TestNothingToPost(t *testing.T) {
	gh := &fakeGH{}
	_, run := runWithReview(t, gh, "") // no review.json: the agent decided nothing needed saying
	if reviews, replies := gh.posted(); run.Status != Succeeded || run.Posted != nil || len(reviews)+len(replies) != 0 {
		t.Fatalf("run = %+v, posted %q %q", run, reviews, replies)
	}
}

func TestBadReviewFileFails(t *testing.T) {
	_, run := runWithReview(t, &fakeGH{}, `{"event":"LGTM","body":"x"}`)
	if run.Status != Failed || !strings.Contains(run.Error, "event") {
		t.Fatalf("run = %+v", run)
	}
}

func TestDryRunDoesNotPost(t *testing.T) {
	gh := &fakeGH{}
	_, run := runWithReview(t, gh, `{"event":"COMMENT","body":"hi"}`, func(c *config.Review) { c.Post = false })
	if reviews, _ := gh.posted(); run.Status != Succeeded || len(reviews) != 0 || run.Posted != nil {
		t.Fatalf("dry run posted: %q %+v", reviews, run)
	}
}

func TestDiscussionIsHandedToTheAgent(t *testing.T) {
	f := newFixture(t)
	f.r.GitHub = &fakeGH{}
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	dir := filepath.Join(f.runs()[0].Dir, "context")
	if b, _ := os.ReadFile(filepath.Join(dir, "review_comments.json")); string(b) != `[{"id":7}]` {
		t.Fatalf("context file = %q", b)
	}
	if !slices.Contains(f.rt.Started()[0].Mounts, container.Mount{Source: dir, Target: "/sisyphus/context", ReadOnly: true}) {
		t.Fatalf("context not mounted: %+v", f.rt.Started()[0].Mounts)
	}
}

// A container could point out/review.json at a host file to get it posted.
func TestReviewFileSymlinkIsRefused(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(secret, []byte(`{"event":"COMMENT","body":"host secret"}`), 0o644)
	gh := &fakeGH{}
	f := newFixture(t, func(c *config.Review) { c.Post = true })
	f.r.GitHub = gh
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	os.Symlink(secret, filepath.Join(f.runs()[0].Dir, "out", "review.json"))
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "run to finish", func() bool { return f.runs()[0].Status != Running })
	if reviews, _ := gh.posted(); len(reviews) != 0 || f.runs()[0].Status != Failed {
		t.Fatalf("followed the symlink: posted %q, run %+v", reviews, f.runs()[0])
	}
}

func TestFallbackDoesNotLeakLocalDetails(t *testing.T) {
	err := errors.New("gh api --method POST repos/ethyca/a/pulls/1/reviews --input /Users/me/.local/state/sisyphus/runs/x/post.json: exit status 1: gh: Unprocessable Entity (HTTP 422)")
	body := folded(ReviewFile{Event: "APPROVE", Body: "ok"}, err)
	if strings.Contains(body, "/Users/") || strings.Contains(body, "--input") || !strings.Contains(body, "HTTP 422") {
		t.Fatalf("fallback body = %q", body)
	}
}

// setupOutput runs the setup part of the container script in bash on the host.
func setupOutput(t *testing.T, setup string, limit time.Duration) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("needs GNU timeout")
	}
	r := &Reviewer{cfg: config.Review{Model: "opus", MaxTurns: 1, StepTimeout: config.Duration{Duration: limit}}}
	script, _, _ := strings.Cut(r.script(config.RepoReview{Setup: setup}, nil), "echo '── sisyphus: claude ──'")
	out, err := exec.Command("bash", "-c", script).CombinedOutput()
	return string(out), err
}

// Each setup line is a step, announced and timed; a backslash continues a line.
func TestSetupStepsAreAnnouncedAndTimed(t *testing.T) {
	out, err := setupOutput(t, "echo one\n\necho two \\\n  three\n", time.Minute)
	want := "── sisyphus: setup: echo one ──\none\n── sisyphus: took 0s\n" +
		"── sisyphus: setup: echo two three ──\ntwo three\n── sisyphus: took 0s\n"
	if err != nil || out != want {
		t.Fatalf("err %v, output:\n%s\nwant:\n%s", err, out, want)
	}
}

// A step past setup_step_timeout is killed, says so, and fails the run; later steps don't run.
func TestSetupStepTimeout(t *testing.T) {
	out, err := setupOutput(t, "sleep 30\necho never", time.Second)
	if err == nil || !strings.Contains(out, "── sisyphus: timed out after 1s (setup_step_timeout)") || strings.Contains(out, "never") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func TestSetupStepFailureStopsSetup(t *testing.T) {
	out, err := setupOutput(t, "false | cat\necho never", time.Minute) // pipefail: a failing pipe fails the step
	if err == nil || strings.Contains(out, "never") || strings.Contains(out, "timed out") {
		t.Fatalf("err %v, output:\n%s", err, out)
	}
}

func TestPostNote(t *testing.T) {
	for _, c := range []struct {
		p       *Posted
		err     error
		posting bool
		want    string
	}{
		{nil, nil, false, "not posted: review.post is off (dry run); the review is in out/review.json"},
		{nil, nil, true, "nothing posted: the agent wrote no out/review.json"},
		{nil, errors.New("HTTP 502"), true, "posting failed: HTTP 502"},
		{&Posted{Event: "REQUEST_CHANGES", Comments: 3, URL: "https://x/r1"}, nil, true, "posted REQUEST_CHANGES with 3 inline comments: https://x/r1"},
		{&Posted{Replies: 2}, nil, true, "posted 2 replies"},
		{&Posted{Event: "APPROVE", Replies: 1, URL: "u"}, nil, true, "posted APPROVE and 1 replies: u"},
		{&Posted{Event: "COMMENT", Fallback: true, URL: "u"}, nil, true, "posted COMMENT (as one plain comment: GitHub refused the review): u"},
	} {
		if got := postNote(c.p, c.err, c.posting); got != c.want {
			t.Errorf("got %q\nwant %q", got, c.want)
		}
	}
}

func withRetries(n int, backoff time.Duration) func(*config.Review) {
	return func(c *config.Review) { c.Retries, c.RetryBackoff = n, config.Duration{Duration: backoff} }
}

// A run that fails before Claude starts is retried after a doubling backoff, up to review.retries.
func TestSetupFailureIsRetriedWithBackoff(t *testing.T) {
	f := newFixture(t, withRetries(2, 50*time.Millisecond))
	f.start(t)
	f.r.Tracked(f.tr)
	var failedAt []time.Time
	for n := 1; n <= 3; n++ {
		waitFor(t, fmt.Sprintf("start %d", n), func() bool { return len(f.rt.Started()) == n })
		if n > 1 && time.Since(failedAt[n-2]) < 50*time.Millisecond<<(n-2) {
			t.Fatalf("attempt %d started %v after the failure, before its backoff", n, time.Since(failedAt[n-2]))
		}
		f.rt.Finish(f.containers()[0].ID, 1, "npm error code ECONNRESET\n")
		failedAt = append(failedAt, time.Now())
		waitFor(t, "run to fail", func() bool { rs := f.runs(); return len(rs) == n && rs[0].Status == Failed })
	}
	time.Sleep(300 * time.Millisecond)
	if len(f.rt.Started()) != 3 || len(f.r.st.Queue) != 0 {
		t.Fatalf("started %d, queue %+v: want 1 try + 2 retries, then nothing", len(f.rt.Started()), f.r.st.Queue)
	}
	if rs := f.runs(); rs[0].Job.Attempt != 2 || rs[2].Job.Attempt != 0 {
		t.Fatalf("attempts = %d..%d", rs[2].Job.Attempt, rs[0].Job.Attempt)
	}
}

// Claude's own failures aren't retried: a re-run would likely fail the same way, and costs.
func TestClaudeFailureIsNotRetried(t *testing.T) {
	f := newFixture(t, withRetries(2, time.Millisecond))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 1, `{"type":"result","subtype":"error_max_turns","is_error":true}`+"\n")
	waitFor(t, "run to fail", f.lastStatus(Failed))
	time.Sleep(50 * time.Millisecond)
	if len(f.rt.Started()) != 1 || len(f.r.st.Queue) != 0 {
		t.Fatalf("started %d, queue %+v", len(f.rt.Started()), f.r.st.Queue)
	}
}

// New commits replace a waiting retry and run right away.
func TestNewCommitsReplaceWaitingRetry(t *testing.T) {
	f := newFixture(t, withRetries(1, time.Hour))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 1, "boom\n")
	waitFor(t, "retry queued", func() bool { q := f.r.Status()[refA.Key()].Queued; return q != nil && q.Attempt == 1 })
	f.r.Updated(f.tr, true, 0)
	waitFor(t, "the new job to start", func() bool { return len(f.rt.Started()) == 2 })
	if run := f.runs()[0]; run.Job.Attempt != 0 || !run.Job.NotBefore.IsZero() { // reason stays "new": never reviewed yet
		t.Fatalf("job = %+v", run.Job)
	}
}

func TestTriggerQueuesManualReview(t *testing.T) {
	f := newFixture(t)
	unsynced := f.tr
	if err := f.r.Trigger(unsynced); err == nil {
		t.Fatal("triggered a review of a PR not synced to its head")
	}
	other := f.tr
	other.PRRef = github.PRRef{Repo: "ethyca/zzz", Number: 9}
	if err := f.r.Trigger(other); err == nil {
		t.Fatal("triggered a review in a repo without review config")
	}
	tr := f.tr
	tr.AnnouncedSHA = tr.HeadSHA
	f.start(t)
	if err := f.r.Trigger(tr); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "start", f.started(1))
	if err := f.r.Trigger(tr); err != nil { // while it runs: queued behind it
		t.Fatal(err)
	}
	if st := f.r.Status()[refA.Key()]; st.State != "running" || st.Queued == nil || st.Queued.Reason != "manual" || len(f.rt.Started()) != 1 {
		t.Fatalf("status = %+v, started %d", st, len(f.rt.Started()))
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "the queued manual review", func() bool { return len(f.rt.Started()) == 2 })
	if f.rt.Started()[1].Env["SISYPHUS_REASON"] != "manual" {
		t.Fatalf("reason = %q", f.rt.Started()[1].Env["SISYPHUS_REASON"])
	}
}

func TestShellIsTheReviewContainerWithBash(t *testing.T) {
	f := newFixture(t, func(c *config.Review) {
		c.Repos["ethyca/a"] = config.RepoReview{Image: "reviewer:a", Workdir: "/a", Setup: "uv sync", Cache: []string{".venv"}}
	})
	spec, stop, err := f.r.Shell(context.Background(), f.tr)
	defer stop()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Image != "reviewer:a" || spec.Workdir != "/a" || spec.Env["SISYPHUS_HEAD_SHA"] != f.tr.HeadSHA ||
		spec.Command[0] != "bash" || !strings.HasSuffix(spec.Command[2], "exec bash") {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Labels["sisyphus"] != "" || spec.Labels["sisyphus.pr"] != "" {
		t.Fatalf("labels = %v: the app would sweep it, or take it for a running review", spec.Labels)
	}
	scripts := map[string]string{}
	for _, m := range spec.Mounts {
		if name, ok := strings.CutPrefix(m.Target, "/sisyphus/"); ok && strings.HasSuffix(name, ".sh") {
			b, _ := os.ReadFile(m.Source)
			scripts[name] = string(b)
		}
	}
	if !strings.Contains(scripts["setup.sh"], "uv sync") || strings.Contains(scripts["setup.sh"], "claude") ||
		!strings.HasPrefix(scripts["review.sh"], scripts["setup.sh"]) || !strings.Contains(scripts["review.sh"], "exec 'claude'") {
		t.Fatalf("scripts = %q", scripts)
	}

	// Not next to a running review: its stack would be re-upped and stopped under it.
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	if _, _, err := f.r.Shell(context.Background(), f.tr); err == nil || !strings.Contains(err.Error(), "is running") {
		t.Fatalf("err = %v, want refusal while a review runs", err)
	}
}

// A job waits in the queue while the PR moves on: the run re-reads the head
// first and stands down instead of reviewing the old code.
func TestRunRefreshesHeadFirst(t *testing.T) {
	f := newFixture(t)
	var refreshed atomic.Int32
	f.r.Refresh = func(context.Context, string) (tracker.Tracked, error) {
		refreshed.Add(1)
		tr := f.tr
		tr.HeadSHA = "newer"
		return tr, nil
	}
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "cancelled", f.lastStatus(Cancelled))
	if refreshed.Load() != 1 || len(f.rt.Started()) != 0 {
		t.Fatalf("refreshed %d, started %d", refreshed.Load(), len(f.rt.Started()))
	}

	f.r.Refresh = func(context.Context, string) (tracker.Tracked, error) { return f.tr, nil }
	f.r.Tracked(f.tr)
	waitFor(t, "start at an unchanged head", f.started(1))
}
