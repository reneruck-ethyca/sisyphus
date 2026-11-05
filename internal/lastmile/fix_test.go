package lastmile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

func TestParseCheckURL(t *testing.T) {
	for _, c := range []struct {
		url      string
		run, job int64
		ok       bool
	}{
		{"https://github.com/o/r/actions/runs/123/job/456", 123, 456, true},
		{"https://github.com/o/r/actions/runs/123/job/456?pr=7", 123, 456, true},
		{"https://github.com/o/r/actions/runs/123", 0, 0, false},
		{"https://ci.example.com/build/9", 0, 0, false},
		{"", 0, 0, false},
	} {
		run, job, ok := parseCheckURL(c.url)
		if run != c.run || job != c.job || ok != c.ok {
			t.Errorf("parseCheckURL(%q) = %d %d %v", c.url, run, job, ok)
		}
	}
}

func TestFingerprint(t *testing.T) {
	a := "2026-11-05T10:00:01.123Z FAILED tests/test_a.py::t - AssertionError: expected 5 got 7 in /tmp/pytest-12/x.py at 3f2a9c1\n" +
		"2026-11-05T10:00:01.200Z ok chatter"
	b := "2026-11-06T11:30:59.001Z FAILED tests/test_a.py::t - AssertionError: expected 9 got 1 in /tmp/pytest-99/x.py at deadbeef01\n" +
		"2026-11-06T11:30:59.900Z different chatter"
	c := "2026-11-05T10:00:01.123Z FAILED tests/test_b.py::t - AssertionError"
	if fingerprint("unit", a) != fingerprint("unit", b) {
		t.Errorf("same failure, different fingerprints: %s %s", fingerprint("unit", a), fingerprint("unit", b))
	}
	if fingerprint("unit", a) == fingerprint("unit", c) || fingerprint("unit", a) == fingerprint("lint", a) {
		t.Error("different failures share a fingerprint")
	}
	if !strings.HasPrefix(fingerprint("unit", "no errors here\njust text"), "unit:") {
		t.Error("fingerprint without error lines")
	}
}

func TestTail(t *testing.T) {
	var lines []string
	for i := range 400 {
		lines = append(lines, strings.Repeat("x", i%7+1))
	}
	got := strings.Split(tail(strings.Join(lines, "\n")+"\n", logTailLines), "\n")
	if len(got) != logTailLines || got[len(got)-1] != lines[399] {
		t.Errorf("tail has %d lines", len(got))
	}
}

func TestGlobAndProtected(t *testing.T) {
	m, _, _ := fixture(t)
	m.cfg.LastMile.AllowPaths = []string{"**/go.sum", "db/migrations/0001_*.sql"}
	for p, want := range map[string]bool{
		".github/workflows/ci.yml": true, "x/.github/workflows/ci.yml": false,
		"app/migrations/0002.py": true, "migrations/1.sql": true, "uv.lock": true, "web/pnpm-lock.yaml": true,
		"go.sum": false, "sub/go.sum": false, // allowed
		"db/migrations/0001_init.sql": false, "db/migrations/0002_more.sql": true,
		"src/main.go": false, "docs/migrations.md": false,
	} {
		if got := m.protected(p); got != want {
			t.Errorf("protected(%q) = %v, want %v", p, got, want)
		}
	}
}

// fakeTasks is a TaskRunner that edits the worktree and answers like an agent.
type fakeTasks struct {
	mu      sync.Mutex
	calls   []review.Task
	edit    func(dir string)
	result  string
	err     error
	release chan struct{} // when set, RunTask waits for it
}

func (f *fakeTasks) RunTask(ctx context.Context, t review.Task) (review.TaskResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, t)
	f.mu.Unlock()
	if f.release != nil {
		select {
		case <-f.release:
		case <-ctx.Done():
			return review.TaskResult{}, ctx.Err()
		}
	}
	if f.edit != nil {
		f.edit(t.Worktree)
	}
	return review.TaskResult{Result: []byte(f.result), Dir: "run"}, f.err
}

func (f *fakeTasks) n() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const okFix = `{"summary":"fix the thing","confidence":"high","escalate":false,"escalate_reason":""}`

func failCheck(name, job string) github.Check {
	return github.Check{Name: name, Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://github.com/ethyca/fidesplus/actions/runs/11/job/" + job}
}

// redFixture is a handoff with fix-ci on, PR open, whose PR shows a failing check "unit" (job 22).
func redFixture(t *testing.T, tasks *fakeTasks) (*Manager, *FakeGitHub, string, string) {
	t.Helper()
	m, gh, _, id := opened(t, Request{Autonomy: "fix-ci"})
	m.Tasks = tasks
	h := m.must(t, id)
	head := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	gh.Logs = map[int64]string{22: "FAILED test_a - AssertionError: 5 != 7\n"}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: head, Author: "me", BaseRef: "main", Checks: []github.Check{failCheck("unit", "22")}}
	return m, gh, id, head
}

func (m *Manager) tickWait(t *testing.T) {
	t.Helper()
	m.tick(context.Background())
	m.Wait()
}

func TestFixAgentCommitsAndPushes(t *testing.T) {
	tasks := &fakeTasks{result: okFix, edit: func(dir string) { os.WriteFile(filepath.Join(dir, "a.txt"), []byte("fixed"), 0o644) }}
	m, _, id, head := redFixture(t, tasks)
	m.tick(context.Background())
	m.Wait()
	h := m.must(t, id)
	if h.State != StateCIRed || h.Reason != "fix agent running" || h.FixRounds["ci"] != 1 {
		t.Fatalf("after launch: %+v", h)
	}
	m.tick(context.Background()) // applies the outcome
	h = m.must(t, id)
	if h.State != StateCIRunning || h.Error != "" {
		t.Fatalf("after run: %+v", h)
	}
	if got := gittest.Git(t, h.Worktree, "log", "-1", "--format=%s"); got != "TLM: fix the thing" {
		t.Errorf("commit subject = %q", got)
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "HEAD") == head || gittest.Git(t, h.Worktree, "status", "--porcelain") != "" {
		t.Error("no new commit, or a dirty worktree")
	}
	up := gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x")
	if up != gittest.Git(t, h.Worktree, "rev-parse", "HEAD") {
		t.Error("not pushed")
	}
	c := tasks.calls[0]
	if c.Repo != "ethyca/fidesplus" || !strings.Contains(c.Prompt, "AssertionError") || !strings.Contains(c.Prompt, "/out/result.json") || c.Context["snapshot.json"] == "" {
		t.Errorf("task = %+v", c)
	}
	if h.Snapshot.Failures[0].Fingerprint == "" || h.Snapshot.Failures[0].RunID != 11 || h.Snapshot.Failures[0].JobID != 22 {
		t.Errorf("failures = %+v", h.Snapshot.Failures)
	}
}

func TestFixAgentDoesNotBlockTick(t *testing.T) {
	tasks := &fakeTasks{result: okFix, release: make(chan struct{})}
	m, _, id, _ := redFixture(t, tasks)
	m.tick(context.Background()) // launches and returns while the agent waits
	for i := 0; tasks.n() == 0 && i < 500; i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if tasks.n() != 1 {
		t.Fatal("agent not started")
	}
	// The tick returns while the run is in flight, and skips the handoff rather than restart it.
	m.tick(context.Background())
	m.tick(context.Background())
	if tasks.n() != 1 || m.must(t, id).Reason != "fix agent running" {
		t.Fatalf("calls %d, handoff %+v", tasks.n(), m.must(t, id))
	}
	if _, ok := m.locks.TryLock("lastmile:" + id); ok {
		t.Error("worktree not locked during the run")
	}
	close(tasks.release)
	m.Wait()
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Reason != "agent made no changes" {
		t.Errorf("handoff = %+v", h)
	}
}

func TestFixAgentOutcomes(t *testing.T) {
	write := func(name, content string) func(string) {
		return func(dir string) {
			os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
			os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644)
		}
	}
	cases := []struct {
		name   string
		tasks  *fakeTasks
		mutate func(*Manager)
		want   string
		clean  bool // the worktree is back at its head
	}{
		{"escalate", &fakeTasks{result: `{"escalate":true,"escalate_reason":"needs a secret"}`}, nil, "agent escalated: needs a secret", true},
		{"no changes", &fakeTasks{result: okFix}, nil, "agent made no changes", true},
		{"workflow", &fakeTasks{result: okFix, edit: write(".github/workflows/ci.yml", "x")}, nil, "protected path: .github/workflows/ci.yml", true},
		{"lockfile", &fakeTasks{result: okFix, edit: write("sub/uv.lock", "x")}, nil, "protected path: sub/uv.lock", true},
		{"over cap", &fakeTasks{result: okFix, edit: write("big.txt", strings.Repeat("l\n", 50))}, func(m *Manager) { m.cfg.LastMile.MaxDiffLines = 10 }, "over max_diff_lines (10)", true},
		{"check fails", &fakeTasks{result: okFix, edit: write("a.txt", "fixed")}, func(m *Manager) {
			m.runHook = tracker.RunHook // opened() stubs it
			m.cfg.LastMile.RepoChecks = map[string]string{"ethyca/fidesplus": "exit 3"}
		}, "repo check failed", true},
		{"allowed path", &fakeTasks{result: okFix, edit: write("go.sum", "x")}, func(m *Manager) { m.cfg.LastMile.AllowPaths = []string{"go.sum"} }, "", false},
		{"bad json", &fakeTasks{result: `nope`}, nil, "result.json is not valid", false},
		{"run fails", &fakeTasks{err: errors.New("boom")}, nil, "fix agent failed: boom", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, _, id, head := redFixture(t, c.tasks)
			if c.mutate != nil {
				c.mutate(m)
			}
			m.tickWait(t)
			m.tick(context.Background())
			h := m.must(t, id)
			if c.want == "" {
				if h.State != StateCIRunning {
					t.Fatalf("handoff = %+v", h)
				}
				return
			}
			if h.State != StateNeedsHuman || !strings.Contains(h.Reason, c.want) {
				t.Fatalf("handoff = %s %q, want %q", h.State, h.Reason, c.want)
			}
			if c.clean && (gittest.Git(t, h.Worktree, "rev-parse", "HEAD") != head || gittest.Git(t, h.Worktree, "status", "--porcelain") != "") {
				t.Errorf("worktree not reset: %s", gittest.Git(t, h.Worktree, "status", "--porcelain"))
			}
			if h.EscalatedBy != TriggerCI {
				t.Errorf("escalated by %q", h.EscalatedBy)
			}
		})
	}
}

func TestRoundCapAndSameFailure(t *testing.T) {
	edit := func(dir string) { os.WriteFile(filepath.Join(dir, "a.txt"), []byte(time.Now().String()), 0o644) }
	tasks := &fakeTasks{result: okFix, edit: edit}
	m, gh, id, _ := redFixture(t, tasks)
	m.tickWait(t)
	m.tick(context.Background())
	// The failure comes back with a new job id and a different number in its message: the same fingerprint.
	gh.Logs[23] = "FAILED test_a - AssertionError: 6 != 9\n"
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, m.must(t, id).Worktree, "rev-parse", "HEAD"), Author: "me", BaseRef: "main", Checks: []github.Check{failCheck("unit", "23")}}
	m.tickWait(t)
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Reason != "same failure twice: unit" || tasks.n() != 1 {
		t.Fatalf("handoff = %s %q, %d runs", h.State, h.Reason, tasks.n())
	}

	// Different failures each time use up the rounds instead.
	m, gh, id, _ = redFixture(t, &fakeTasks{result: okFix, edit: edit})
	tasks = m.Tasks.(*fakeTasks)
	for i, job := range []string{"31", "32", "33"} {
		gh.Logs[int64(31+i)] = "FAILED test_" + strings.Repeat("x", i+1) + " - ValueError\n"
		gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, m.must(t, id).Worktree, "rev-parse", "HEAD"), Author: "me", BaseRef: "main",
			Checks: []github.Check{failCheck("unit", job)}}
		m.tickWait(t)
		m.tick(context.Background())
		if h := m.must(t, id); h.FixRounds["ci"] != i+1 || h.State != StateCIRunning {
			t.Fatalf("round %d: %+v", i+1, h)
		}
	}
	gh.Logs[34] = "FAILED another - KeyError\n"
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, m.must(t, id).Worktree, "rev-parse", "HEAD"), Author: "me", BaseRef: "main",
		Checks: []github.Check{failCheck("unit", "34")}}
	m.tickWait(t)
	if h := m.must(t, id); h.State != StateNeedsHuman || !strings.Contains(h.Reason, "after 3 fix rounds") || tasks.n() != 3 {
		t.Fatalf("handoff = %s %q, %d runs", h.State, h.Reason, tasks.n())
	}
}

func TestFlakeRerunBudget(t *testing.T) {
	tasks := &fakeTasks{result: okFix}
	m, gh, id, head := redFixture(t, tasks)
	m.cfg.LastMile.Flakes = []string{`ECONNRESET`}
	gh.Logs[22] = "npm ERR! ECONNRESET while fetching\n"
	for i := 1; i <= 2; i++ {
		m.tickWait(t)
		if h := m.must(t, id); h.State != StateCIRunning || h.FlakeReruns != i || len(gh.Reruns) != i || gh.Reruns[i-1] != 11 {
			t.Fatalf("rerun %d: %+v reruns %v", i, h, gh.Reruns)
		}
	}
	// Budget spent on this head: the flake is treated as a red like any other.
	m.tickWait(t)
	if len(gh.Reruns) != 2 || tasks.n() != 1 {
		t.Fatalf("reruns %v, agent runs %d", gh.Reruns, tasks.n())
	}
	// A new head has its own budget.
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: head + "2", Author: "me", BaseRef: "main", Checks: []github.Check{failCheck("unit", "22")}}
	m.Wait()
	m.mu.Lock()
	m.handoffs[0].State = StatePROpen
	m.mu.Unlock()
	m.running.drop(id)
	m.tickWait(t)
	if len(gh.Reruns) != 3 || m.must(t, id).FlakeReruns != 1 {
		t.Fatalf("reruns %v, handoff %+v", gh.Reruns, m.must(t, id))
	}
}

func TestFlakeRerunWithoutFixCI(t *testing.T) {
	m, gh, _, id := opened(t, Request{})
	m.cfg.LastMile.Flakes = []string{`timeout`}
	gh.Logs = map[int64]string{22: "step timeout\n"}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{failCheck("unit", "22")}}
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateCIRunning || !slices.Equal(gh.Reruns, []int64{11}) {
		t.Fatalf("handoff %+v reruns %v", h, gh.Reruns)
	}
}

func TestNonActionsCheckHasNoLog(t *testing.T) {
	tasks := &fakeTasks{result: okFix}
	m, gh, id, _ := redFixture(t, tasks)
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{{Name: "ext", Status: "COMPLETED", Conclusion: "FAILURE", URL: "https://ci.example.com/1"}}}
	m.tick(context.Background())
	f := m.must(t, id).Snapshot.Failures
	if len(f) != 1 || f[0].Log != "" || f[0].RunID != 0 || f[0].Fingerprint == "" {
		t.Errorf("failures = %+v", f)
	}
	m.Wait()
}

func TestResumeAndCancel(t *testing.T) {
	tasks := &fakeTasks{result: `{"escalate":true,"escalate_reason":"no"}`}
	m, gh, id, _ := redFixture(t, tasks)
	m.tickWait(t)
	m.tick(context.Background())
	h := m.must(t, id)
	if h.State != StateNeedsHuman || h.FixRounds["ci"] != 1 || len(h.FixedFailures) != 1 {
		t.Fatalf("setup: %+v", h)
	}
	got, err := m.Resume(context.Background(), id)
	if err != nil || got.State != StatePROpen || got.Reason != "" || got.FixRounds["ci"] != 0 || len(got.FixedFailures) != 0 || got.PrevState != "" {
		t.Fatalf("resume = %+v, %v", got, err)
	}
	if last := gh.Edits[len(gh.Edits)-1]; !slices.Equal(last.RemoveLabels, []string{"tlm:needs-human"}) {
		t.Errorf("label not removed: %+v", last)
	}
	invalidWith(t, func() error { _, err := m.Resume(context.Background(), id); return err }(), "not needs-human")
	if _, err := m.Resume(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}

	edits := len(gh.Edits)
	c, err := m.Cancel(id)
	if err != nil || c.State != StateCancelled || len(gh.Edits) != edits {
		t.Fatalf("cancel = %+v, %v", c, err)
	}
	invalidWith(t, func() error { _, err := m.Cancel(id); return err }(), "already cancelled")
	m.tick(context.Background()) // terminal: untouched
	if m.must(t, id).State != StateCancelled {
		t.Error("tick moved a cancelled handoff")
	}
	if _, err := m.Handoff(context.Background(), Request{Worktree: h.Worktree, Base: "main"}); err != nil {
		t.Errorf("branch not released: %v", err)
	}
}

func TestTickDiscardedAfterConcurrentEdit(t *testing.T) {
	m, _, _, id := opened(t, Request{})
	before := m.must(t, id)
	after := before
	after.State = StateCIGreen
	if _, err := m.Cancel(id); err != nil {
		t.Fatal(err)
	}
	if m.commit(before, after) || m.must(t, id).State != StateCancelled {
		t.Error("a stale tick overwrote the cancel")
	}
}

func TestAgentsNeedReviews(t *testing.T) {
	m, gh, _, id := opened(t, Request{Autonomy: "fix-ci"})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{failCheck("unit", "22")}}
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Reason != "agents need reviews enabled" {
		t.Errorf("handoff = %+v", h)
	}
}
