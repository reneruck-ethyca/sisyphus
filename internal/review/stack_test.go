package review

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/reneruck/waterboy/internal/config"
)

// fakeShell records host-side stack commands; fn decides each one's outcome.
type fakeShell struct {
	mu    sync.Mutex
	calls []shellCall
	fn    func(c shellCall, out io.Writer) error
}

type shellCall struct {
	Dir, Script string
	Env         []string
}

func (s *fakeShell) run(_ context.Context, dir string, env []string, script string, out io.Writer) error {
	c := shellCall{dir, script, env}
	s.mu.Lock()
	s.calls = append(s.calls, c)
	fn := s.fn
	s.mu.Unlock()
	if fn != nil {
		return fn(c, out)
	}
	return nil
}

func (s *fakeShell) get() []shellCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.calls)
}

// ups and stops: the calls that were stack up, and the projects stopped/removed.
func (s *fakeShell) ups() (ups []shellCall) {
	for _, c := range s.get() {
		if c.Script == "up" {
			ups = append(ups, c)
		}
	}
	return ups
}

func (s *fakeShell) composed(verb string) (projects []string) {
	for _, c := range s.get() {
		if strings.Contains(c.Script, "docker compose -p \"$WATERBOY_STACK\" "+verb) {
			projects = append(projects, strings.TrimPrefix(c.Env[0], "WATERBOY_STACK="))
		}
	}
	return projects
}

func withStack(from, globalFrom string) func(*config.Review) {
	return func(c *config.Review) {
		c.StackFrom = globalFrom
		c.Repos["ethyca/a"] = config.RepoReview{Image: "img:1",
			Stack: &config.Stack{From: from, SafeDir: "/safe", Up: "up", Project: "proj-{id}"}}
	}
}

func stackFixture(t *testing.T, mutate func(*config.Review)) (*fixture, *fakeShell) {
	f := newFixture(t, mutate)
	sh := &fakeShell{}
	f.r.sh = sh.run
	return f, sh
}

func TestStackStartsBeforeReviewAndStopsAfter(t *testing.T) {
	f, sh := stackFixture(t, withStack("", config.StackFromWorktree))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))

	ups := sh.ups()
	if len(ups) != 1 || ups[0].Dir != f.tr.Worktree || !slices.Equal(ups[0].Env, []string{"WORKTREE_ID=pr-1"}) {
		t.Fatalf("ups = %+v", ups)
	}
	if n := f.rt.Started()[0].Network; n != "proj-pr-1_default" {
		t.Fatalf("network = %q", n)
	}
	if len(sh.composed("stop")) != 0 {
		t.Fatal("stack stopped while the review runs")
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "run to finish", f.lastStatus(Succeeded))
	if got := sh.composed("stop"); !slices.Equal(got, []string{"proj-pr-1"}) {
		t.Fatalf("stopped = %v", got)
	}
	run := f.runs()[0]
	if run.Stack == nil || run.Stack.Project != "proj-pr-1" || run.Stack.Dir != f.tr.Worktree {
		t.Fatalf("run.Stack = %+v", run.Stack)
	}
	if b, _ := os.ReadFile(filepath.Join(run.Dir, "stack.log")); !strings.Contains(string(b), "up in "+f.tr.Worktree) {
		t.Fatalf("stack.log = %q", b)
	}
}

func TestStackFromSafeCheckout(t *testing.T) {
	for _, c := range []struct{ from, global, want string }{
		{"", config.StackFromSafe, "/safe"},
		{config.StackFromSafe, config.StackFromWorktree, "/safe"},
		{config.StackFromWorktree, config.StackFromSafe, ""}, // the worktree
	} {
		f, sh := stackFixture(t, withStack(c.from, c.global))
		f.start(t)
		f.r.Tracked(f.tr)
		waitFor(t, "container start", f.started(1))
		want := c.want
		if want == "" {
			want = f.tr.Worktree
		}
		if ups := sh.ups(); len(ups) != 1 || ups[0].Dir != want {
			t.Errorf("from %q, global %q: ups = %+v, want dir %s", c.from, c.global, ups, want)
		}
	}
}

func TestStackPortClashRetriesNextOffset(t *testing.T) {
	f, sh := stackFixture(t, withStack("", config.StackFromWorktree))
	clashes := 2
	sh.fn = func(c shellCall, out io.Writer) error {
		if c.Script == "up" && clashes > 0 {
			clashes--
			io.WriteString(out, "Error: Bind for 0.0.0.0:5632 failed: port is already allocated\n")
			return errors.New("exit status 1")
		}
		return nil
	}
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	var offsets []string
	for _, u := range sh.ups() {
		offsets = append(offsets, strings.Join(u.Env[1:], ""))
	}
	// PR 1: the stack's own pick, then slots from 100*(1+1%49) on.
	if want := []string{"", "PORT_OFFSET=200", "PORT_OFFSET=300"}; !slices.Equal(offsets, want) {
		t.Fatalf("offsets = %q, want %q", offsets, want)
	}
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "run to finish", f.lastStatus(Succeeded))

	// The offset that worked is where the next run starts, also after a restart.
	r2 := f.newReviewer(t, f.r.cfg)
	r2.sh = sh.run
	f.r = r2
	f.start(t)
	f.r.Updated(f.tr, true, 0)
	waitFor(t, "second container", func() bool { return len(f.rt.Started()) == 2 })
	if ups := sh.ups(); !slices.Equal(ups[len(ups)-1].Env, []string{"WORKTREE_ID=pr-1", "PORT_OFFSET=300"}) {
		t.Fatalf("second run env = %v", ups[len(ups)-1].Env)
	}
}

func TestStackFailureFailsRunWithoutContainer(t *testing.T) {
	f, sh := stackFixture(t, withStack("", config.StackFromWorktree))
	sh.fn = func(c shellCall, out io.Writer) error {
		if c.Script == "up" {
			io.WriteString(out, "nox > Session dev_datastores failed.\n")
			return errors.New("exit status 1")
		}
		return nil
	}
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "run to end", f.lastStatus(Errored))
	if len(f.rt.Started()) != 0 || len(sh.ups()) != 1 {
		t.Fatalf("started %d containers after %d ups", len(f.rt.Started()), len(sh.ups()))
	}
	if run := f.runs()[0]; !strings.Contains(run.Error, "dev_datastores failed") {
		t.Fatalf("error = %q", run.Error)
	}
	if got := sh.composed("down"); !slices.Equal(got, []string{"proj-pr-1"}) || len(sh.composed("stop")) != 0 {
		t.Fatalf("half-started stack not removed: down %v, stop %v", got, sh.composed("stop"))
	}
}

// A failed review leaves no stack behind: containers, network and volumes go.
func TestFailedReviewRemovesStack(t *testing.T) {
	f, sh := stackFixture(t, withStack("", config.StackFromWorktree))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.rt.Finish(f.containers()[0].ID, 1, "npm error code ECONNRESET\n")
	waitFor(t, "run to fail", f.lastStatus(Failed))
	if got := sh.composed("down"); !slices.Equal(got, []string{"proj-pr-1"}) || len(sh.composed("stop")) != 0 {
		t.Fatalf("down %v, stop %v", got, sh.composed("stop"))
	}
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	if len(f.r.st.Stacks) != 0 {
		t.Fatalf("stack still recorded: %v", f.r.st.Stacks)
	}
}

func TestStackRemovedOnDropAndForget(t *testing.T) {
	f, sh := stackFixture(t, withStack("", config.StackFromWorktree))
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "run to finish", f.lastStatus(Succeeded))

	if err := f.r.Forget(context.Background(), refA.Key()); err != nil {
		t.Fatal(err)
	}
	if got := sh.composed("down"); !slices.Equal(got, []string{"proj-pr-1"}) {
		t.Fatalf("removed on forget = %v", got)
	}

	f.r.Tracked(f.tr)
	waitFor(t, "second container", func() bool { return len(f.rt.Started()) == 2 })
	f.succeed(f.containers()[0].ID, "")
	waitFor(t, "second run", f.lastStatus(Succeeded))
	f.r.Dropped(f.tr, "merged")
	waitFor(t, "removed on drop", func() bool { return len(sh.composed("down")) == 2 })
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	if len(f.r.st.Stacks) != 0 {
		t.Fatalf("stacks left = %v", f.r.st.Stacks)
	}
}

func TestNextOffset(t *testing.T) {
	for _, c := range []struct{ cur, pr, want int }{{0, 1, 200}, {0, 49, 100}, {0, 6618, 100 * (1 + 6618%49)}, {300, 1, 400}, {4900, 1, 100}} {
		if got := nextOffset(c.cur, c.pr); got != c.want {
			t.Errorf("nextOffset(%d, %d) = %d, want %d", c.cur, c.pr, got, c.want)
		}
	}
}

func TestErrorLinesPicksTheCause(t *testing.T) {
	out := `Volume fidesplus-pr-1_postgres Created
Error response from daemon: Bind for 0.0.0.0:5532 failed: port is already allocated
Docker version 29.8.2, build 7fc2dff
Python version: 3.13.13
nox > Command docker compose up failed with exit code 1
nox > Session dev_datastores failed.
`
	want := "Error response from daemon: Bind for 0.0.0.0:5532 failed: port is already allocated\n" +
		"nox > Command docker compose up failed with exit code 1\nnox > Session dev_datastores failed."
	if got := errorLines(out); got != want {
		t.Fatalf("got %q", got)
	}
	if got := errorLines("a\nb\n"); got != "a\nb" {
		t.Fatalf("no error lines: got %q", got)
	}
}
