package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/gittest"
	"github.com/reneruck/waterboy/internal/tracker"
)

var refA = github.PRRef{Repo: "ethyca/a", Number: 1}

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
	cfg := config.Review{Image: "img:1", Command: []string{"sh", "-c", "review"}, Timeout: config.Duration{Duration: time.Minute},
		MaxConcurrent: 1, Cleanup: true}
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
	infos, _ := f.rt.List(context.Background(), map[string]string{"waterboy": "1"})
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
	if s.Image != "img:1" || s.Workdir != f.tr.Worktree || s.Labels["waterboy.pr"] != refA.Key() {
		t.Fatalf("spec = %+v", s)
	}
	if s.Env["WATERBOY_REASON"] != "new" || s.Env["WATERBOY_HEAD_SHA"] != f.tr.HeadSHA || s.Env["WATERBOY_OUT"] != "/out" || s.Env["WB_TEST_TOKEN"] != "t0k" {
		t.Fatalf("env = %v", s.Env)
	}
	// A clone's .git lives inside the worktree, so only the worktree and /out are mounted here.
	if len(s.Mounts) != 2 || s.Mounts[0] != (container.Mount{Source: f.tr.Worktree, Target: f.tr.Worktree}) || s.Mounts[1].Target != "/out" {
		t.Fatalf("mounts = %+v (git dir %s)", s.Mounts, gitDir)
	}
	if st := f.r.Status()[refA.Key()]; st.State != "running" {
		t.Fatalf("state = %q", st.State)
	}

	f.rt.Finish(f.containers()[0].ID, 0, "looks good\n")
	waitFor(t, "run to finish", f.lastStatus(Succeeded))
	run := f.runs()[0]
	if b, _ := os.ReadFile(filepath.Join(run.Dir, "log")); string(b) != "looks good\n" {
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
	f.tr.Worktree = wt
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	var targets []string
	for _, m := range f.rt.Started()[0].Mounts {
		targets = append(targets, m.Target)
	}
	if len(targets) != 3 || targets[0] != wt || !strings.HasSuffix(targets[1], "/.git") || targets[2] != "/out" {
		t.Fatalf("mount targets = %q", targets)
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
		f.rt.Finish(f.containers()[0].ID, 0, "")
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
	if s := f.rt.Started()[1]; s.Env["WATERBOY_REASON"] != "commits" || s.Env["WATERBOY_HEAD_SHA"] != next.HeadSHA {
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
	f.rt.Finish(f.containers()[0].ID, 0, "")
	waitFor(t, "follow-up run", func() bool { return len(f.rt.Started()) == 2 })
	if f.rt.Started()[1].Env["WATERBOY_REASON"] != "replies" || f.runs()[1].Status != Succeeded {
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
	f.rt.Finish(f.containers()[0].ID, 0, "")
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

func TestTimeoutStopsContainer(t *testing.T) {
	f := newFixture(t, func(c *config.Review) { c.Timeout = config.Duration{Duration: 50 * time.Millisecond} })
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "timeout", f.lastStatus(TimedOut))
	if f.runs()[0].ExitCode != 137 || len(f.containers()) != 0 {
		t.Fatalf("run = %+v, containers %d", f.runs()[0], len(f.containers()))
	}
}

func TestFailedExitCode(t *testing.T) {
	f := newFixture(t)
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 2, "boom")
	waitFor(t, "failure", f.lastStatus(Failed))
	if f.runs()[0].ExitCode != 2 {
		t.Fatalf("run = %+v", f.runs()[0])
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
	f.rt.Finish(f.containers()[0].ID, 0, "replayed\n")
	waitFor(t, "re-attached run to finish", f.lastStatus(Succeeded))
	if b, _ := os.ReadFile(filepath.Join(f.runs()[0].Dir, "log")); string(b) != "replayed\n" {
		t.Fatalf("log = %q", b)
	}
}

func TestReattachLostRunIsRequeued(t *testing.T) {
	f := newFixture(t)
	f.r.st.Runs[refA.Key()] = []Run{{ID: "r1", Job: Job{PR: f.tr, Reason: "commits"}, Status: Running, Container: "gone",
		Started: time.Now(), Dir: filepath.Join(f.dir, "runs", "r1")}}
	orphan := f.rt.Seed(map[string]string{"waterboy": "1", "waterboy.run": "zz"}, true, 0, "")
	f.r.Reattach(context.Background())
	if st := f.r.Status()[refA.Key()]; st.State != "queued" || st.Runs[0].Status != Lost {
		t.Fatalf("status = %+v", st)
	}
	if slices.ContainsFunc(f.containers(), func(c container.Info) bool { return c.ID == orphan }) {
		t.Fatal("orphaned container not removed")
	}
	f.start(t)
	waitFor(t, "requeued run", f.started(1))
	if f.rt.Started()[0].Env["WATERBOY_REASON"] != "commits" {
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
