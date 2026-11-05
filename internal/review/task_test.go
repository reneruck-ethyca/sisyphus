package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
)

func init() { taskPoll = 10 * time.Millisecond }

// finishTask ends the n-th started container like an agent run: result.json (or not), then exit.
func (f *fixture) finishTask(t *testing.T, n int, result string, code int) {
	t.Helper()
	waitFor(t, "task container", func() bool { return len(f.rt.Started()) > n })
	s := f.rt.Started()[n]
	if result != "" {
		for _, m := range s.Mounts {
			if m.Target == "/out" {
				if err := os.WriteFile(filepath.Join(m.Source, "result.json"), []byte(result), 0o644); err != nil {
					t.Error(err)
				}
			}
		}
	}
	waitFor(t, "container listed", func() bool { return len(f.containers()) > 0 })
	for _, c := range f.containers() {
		if c.Running {
			f.rt.Finish(c.ID, code, "")
			return
		}
	}
}

func (f *fixture) task(id string) Task {
	return Task{ID: id, Repo: "ethyca/a", Worktree: f.tr.Worktree, Prompt: "fix it", Context: map[string]string{"snap.json": "{}"}}
}

func TestRunTaskReturnsResult(t *testing.T) {
	f := newFixture(t, func(c *config.Review) { c.Env = []string{"GH_TOKEN"} })
	t.Setenv("GH_TOKEN", "secret")
	go f.finishTask(t, 0, `{"summary":"ok"}`, 0)
	res, err := f.r.RunTask(context.Background(), f.task("h1"))
	if err != nil || string(res.Result) != `{"summary":"ok"}` {
		t.Fatalf("res = %q, %v", res.Result, err)
	}
	s := f.rt.Started()[0]
	if v, ok := s.Env["GH_TOKEN"]; !ok || v != "" || s.Env["GITHUB_TOKEN"] != "" || s.Labels["sisyphus.task"] != "h1" || s.Labels["sisyphus.pr"] != "" {
		t.Fatalf("env/labels = %v %v", s.Env, s.Labels)
	}
	var mounts []string
	for _, m := range s.Mounts {
		mounts = append(mounts, m.Target)
	}
	if !strings.Contains(strings.Join(mounts, " "), "/sisyphus/context") || !strings.Contains(res.Dir, filepath.Join("tasks", "h1")) {
		t.Fatalf("mounts = %v dir = %s", mounts, res.Dir)
	}
	if b, _ := os.ReadFile(filepath.Join(res.Dir, "prompt.md")); string(b) != "fix it" {
		t.Fatalf("prompt = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(res.Dir, "context", "snap.json")); string(b) != "{}" {
		t.Fatalf("context = %q", b)
	}
	if _, ok := f.r.Status()["task:h1"]; ok {
		t.Fatal("task runs show up as PR statuses")
	}
}

func TestRunTaskErrors(t *testing.T) {
	for name, c := range map[string]struct {
		result string
		code   int
		want   string
	}{"no result": {"", 0, "no /out/result.json"}, "exit code": {`{}`, 3, "exited 3"}} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			go f.finishTask(t, 0, c.result, c.code)
			if _, err := f.r.RunTask(context.Background(), f.task("h1")); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	f := newFixture(t)
	bad := f.task("h1")
	bad.Repo = "ethyca/unknown"
	if _, err := f.r.RunTask(context.Background(), bad); err == nil {
		t.Fatal("unknown repo accepted")
	}
}

func TestRunTaskSharesSlots(t *testing.T) {
	f := newFixture(t) // max_concurrent 1
	done := make(chan error, 2)
	for _, id := range []string{"h1", "h2"} {
		go func() { _, err := f.r.RunTask(context.Background(), f.task(id)); done <- err }()
		waitFor(t, "first container", func() bool { return len(f.rt.Started()) >= 1 })
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.rt.Started()); n != 1 {
		t.Fatalf("%d containers started with one slot", n)
	}
	f.finishTask(t, 0, `{}`, 0)
	waitFor(t, "second task", func() bool { return len(f.rt.Started()) == 2 })
	f.finishTask(t, 1, `{}`, 0)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestRunTaskWaitsForSlotUntilCancelled(t *testing.T) {
	f := newFixture(t)
	f.r.mu.Lock()
	f.r.active["ethyca/a#9"] = func(error) {} // a review holds the only slot
	f.r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := f.r.RunTask(ctx, f.task("h1")); err == nil || len(f.rt.Started()) != 0 {
		t.Fatalf("err = %v, started %d", err, len(f.rt.Started()))
	}
}

func TestSlots(t *testing.T) {
	f := newFixture(t) // max_concurrent 1
	if used, max := f.r.Slots(); used != 0 || max != 1 {
		t.Fatalf("idle slots = %d of %d", used, max)
	}
	f.r.mu.Lock()
	f.r.active["ethyca/a#9"] = func(error) {}
	f.r.active["task:h1"] = func(error) {}
	f.r.mu.Unlock()
	if used, max := f.r.Slots(); used != 2 || max != 1 {
		t.Errorf("slots = %d of %d, want a review and a task counted", used, max)
	}
}
