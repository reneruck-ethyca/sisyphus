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

	"github.com/reneruck/sisyphus/internal/gittest"
)

const okAdvice = `{"summary":"half a feature","message":"Add wip"}`

// dirtyFixture is a needs-human handoff whose worktree holds files (name → content),
// assessed by a fake model that answers okAdvice.
func dirtyFixture(t *testing.T, files map[string]string) (*Manager, string, string) {
	t.Helper()
	m, _, id, _ := redFixture(t, &fakeTasks{result: okFix})
	m.Assess = func(context.Context, string) (string, error) { return okAdvice, nil }
	wt := m.must(t, id).Worktree
	for name, content := range files {
		writeFile(t, wt, name, content)
	}
	m.tickWait(t) // the fix agent refuses the dirty worktree
	m.tickWait(t) // escalates
	m.tickWait(t) // lists the files and assesses
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Dirty == nil || h.Dirty.Message == "" {
		t.Fatalf("handoff = %s %+v", h.State, h.Dirty)
	}
	return m, id, wt
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755)
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func hashOf(t *testing.T, m *Manager, id string) string {
	t.Helper()
	return m.must(t, id).Dirty.Hash()
}

func TestDirtyFilesShowBeforeTheAssessment(t *testing.T) {
	m, _, id, _ := redFixture(t, &fakeTasks{result: okFix})
	release := make(chan struct{})
	var mu sync.Mutex
	var prompts []string
	m.Assess = func(_ context.Context, prompt string) (string, error) {
		mu.Lock()
		prompts = append(prompts, prompt)
		mu.Unlock()
		<-release
		return okAdvice, nil
	}
	wt := m.must(t, id).Worktree
	writeFile(t, wt, "a.txt", "two\nthree\n") // tracked "two" → one line changed, one added
	writeFile(t, wt, "sub/new.txt", "x\ny")
	writeFile(t, wt, "blob.bin", "a\x00b")
	m.tickWait(t)
	m.tickWait(t)
	if h := m.must(t, id); h.State != StateNeedsHuman || !strings.Contains(h.Reason, "uncommitted") {
		t.Fatalf("handoff = %s %q", h.State, h.Reason)
	}
	m.tick(context.Background()) // lists the files; the assessment hangs
	d := m.must(t, id).Dirty
	if d == nil || d.assessed() {
		t.Fatalf("dirty = %+v", d)
	}
	if v := m.Views()[0]; v.DirtyHash == "" || v.DirtyHash != d.Hash() {
		t.Errorf("view dirty hash = %q", v.DirtyHash)
	}
	want := []DirtyFile{
		{Path: "a.txt", Code: "M", Add: 2, Del: 1},
		{Path: "blob.bin", Code: "??", Binary: true},
		{Path: "sub/new.txt", Code: "??", Add: 2},
	}
	if !slices.Equal(d.Files, want) {
		t.Fatalf("files = %+v", d.Files)
	}
	close(release)
	m.Wait()
	m.tickWait(t) // same dirt: nothing new
	d = m.must(t, id).Dirty
	if d.Summary != "half a feature" || d.Message != "Add wip" || !slices.Equal(d.Files, want) {
		t.Fatalf("dirty = %+v", d)
	}
	if len(prompts) != 1 || !strings.Contains(prompts[0], "?? sub/") || !strings.Contains(prompts[0], "--- sub/new.txt ---\nx\ny") {
		t.Fatalf("prompts = %q", prompts)
	}

	for _, f := range []string{"sub/new.txt", "blob.bin"} {
		os.Remove(filepath.Join(wt, f))
	}
	gittest.Git(t, wt, "checkout", "a.txt")
	m.tickWait(t)
	if h := m.must(t, id); h.Dirty != nil {
		t.Fatalf("clean worktree keeps %+v", h.Dirty)
	}
}

func TestDirtyAssessmentFailureKeepsThePanel(t *testing.T) {
	m, _, id, _ := redFixture(t, &fakeTasks{result: okFix})
	m.Assess = func(context.Context, string) (string, error) { return "", errors.New("no claude") }
	writeFile(t, m.must(t, id).Worktree, "wip.txt", "x")
	for range 3 {
		m.tickWait(t)
	}
	d := m.must(t, id).Dirty
	if d == nil || !strings.Contains(d.Err, "no claude") || len(d.Files) != 1 {
		t.Fatalf("dirty = %+v", d)
	}
}

func TestDirtyOldFormatIsRebuilt(t *testing.T) {
	m, id, _ := dirtyFixture(t, map[string]string{"wip.txt": "x"})
	st := m.must(t, id).Dirty.Status
	if _, err := m.update(id, func(h *Handoff) error {
		h.Dirty = &Dirty{Status: st, Summary: "old advice"} // as saved before Files existed
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.tickWait(t)
	if d := m.must(t, id).Dirty; len(d.Files) != 1 || d.Summary != "half a feature" {
		t.Fatalf("dirty = %+v", d)
	}
}

func TestDirtyCommitSomePathsPushesAndResumes(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"a.txt": "edited", "wip.txt": "mine", "other.txt": "not this"})
	h, err := m.DirtyCommit(context.Background(), id, hashOf(t, m, id), []string{"a.txt", "wip.txt"}, "  My own subject\n\nbody  ")
	if err != nil {
		t.Fatal(err)
	}
	if h.State != StatePROpen || h.Dirty != nil {
		t.Errorf("handoff = %s %+v", h.State, h.Dirty)
	}
	if got := gittest.Git(t, wt, "log", "-1", "--format=%B%n--", "--name-only"); got != "My own subject\n\nbody\n\n--\n\na.txt\nwip.txt" {
		t.Errorf("commit = %q", got)
	}
	if gittest.Git(t, wt, "rev-parse", "origin/feature/x") != gittest.Git(t, wt, "rev-parse", "HEAD") {
		t.Error("not pushed")
	}
	if st := gittest.Git(t, wt, "status", "--porcelain"); st != "?? other.txt" {
		t.Errorf("status = %q", st)
	}
	if !slices.ContainsFunc(h.Events, func(e Event) bool { return e.Kind == "dirty" && e.Text == "commit 2 files: My own subject" }) {
		t.Errorf("no dirty event: %+v", h.Events)
	}
}

// A file named like a pathspec glob commits only itself, not every file it would match.
func TestDirtyCommitTakesPathsLiterally(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"*.txt": "glob-named", "other.txt": "not this"})
	if _, err := m.DirtyCommit(context.Background(), id, hashOf(t, m, id), []string{"*.txt"}, "m"); err != nil {
		t.Fatal(err)
	}
	if got := gittest.Git(t, wt, "log", "-1", "--format=", "--name-only"); got != "*.txt" {
		t.Errorf("committed = %q", got)
	}
	if st := gittest.Git(t, wt, "status", "--porcelain"); st != "?? other.txt" {
		t.Errorf("status = %q", st)
	}
}

func TestDirtyCommitRefusesBadInput(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"wip.txt": "x"})
	hash := hashOf(t, m, id)
	for _, c := range []struct {
		paths []string
		msg   string
		want  string
	}{
		{[]string{"a.txt"}, "m", "not in the dirty status"},
		{[]string{"../etc/passwd"}, "m", "not in the dirty status"},
		{[]string{"wip.txt"}, " \n ", "empty commit message"},
		{nil, "m", "no files"},
	} {
		_, err := m.DirtyCommit(context.Background(), id, hash, c.paths, c.msg)
		invalidWith(t, err, c.want)
	}
	if _, err := m.DirtyCommit(context.Background(), id, "nope", []string{"wip.txt"}, "m"); !errors.Is(err, ErrStale) {
		t.Errorf("wrong hash: %v", err)
	}
	if st := gittest.Git(t, wt, "status", "--porcelain"); st != "?? wip.txt" {
		t.Errorf("status = %q", st)
	}
}

func TestDirtyCommitFailureKeepsTheWork(t *testing.T) {
	for _, tc := range []struct {
		name, file string
		setup      func(wt string)
		want       string
	}{
		{"protected path", ".husky/pre-commit", nil, "protected path"},
		{"push fails", "wip.txt", func(wt string) { gittest.Git(t, wt, "remote", "set-url", "--push", "origin", "/nonexistent") }, "push"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, id, wt := dirtyFixture(t, map[string]string{tc.file: "precious"})
			head := gittest.Git(t, wt, "rev-parse", "HEAD")
			if tc.setup != nil {
				tc.setup(wt)
			}
			if _, err := m.DirtyCommit(context.Background(), id, hashOf(t, m, id), []string{tc.file}, "m"); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v", err)
			}
			if b, err := os.ReadFile(filepath.Join(wt, tc.file)); err != nil || string(b) != "precious" {
				t.Errorf("work lost: %q %v", b, err)
			}
			if gittest.Git(t, wt, "rev-parse", "HEAD") != head || gittest.Git(t, wt, "diff", "--cached", "--name-only") != "" {
				t.Error("commit or staging left behind")
			}
			if h := m.must(t, id); h.State != StateNeedsHuman || !strings.Contains(h.Events[len(h.Events)-1].Text, "commit failed") {
				t.Errorf("handoff = %s %+v", h.State, h.Events[len(h.Events)-1])
			}
		})
	}
}

func TestDirtyStashAndLeave(t *testing.T) {
	files := map[string]string{"wip.txt": "x", "debug.log": "y"}
	t.Run("stash", func(t *testing.T) {
		m, id, wt := dirtyFixture(t, files)
		h, err := m.DirtyStash(context.Background(), id, hashOf(t, m, id))
		if err != nil || h.State != StateNeedsHuman {
			t.Fatalf("%s %v", h.State, err)
		}
		if st := gittest.Git(t, wt, "status", "--porcelain"); st != "" {
			t.Errorf("status = %q", st)
		}
		if l := gittest.Git(t, wt, "stash", "list"); !strings.Contains(l, "tlm: feature/x WIP") {
			t.Errorf("stash = %q", l)
		}
		m.tickWait(t)
		if h := m.must(t, id); h.Dirty != nil {
			t.Errorf("dirty = %+v", h.Dirty)
		}
	})
	t.Run("leave", func(t *testing.T) {
		m, id, _ := dirtyFixture(t, files)
		if _, err := m.DirtyLeave(context.Background(), id, hashOf(t, m, id)); err != nil {
			t.Fatal(err)
		}
		m.tickWait(t)
		h := m.must(t, id)
		if h.Dirty == nil || !h.Dirty.Dismissed {
			t.Errorf("dirty = %+v", h.Dirty)
		}
		if e := h.Events[len(h.Events)-1]; e.Kind != "dirty" {
			t.Errorf("event = %+v", e)
		}
	})
}

func TestDirtyActionRefusesAChangedWorktree(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"wip.txt": "x"})
	hash := hashOf(t, m, id)
	writeFile(t, wt, "new.txt", "x")
	if _, err := m.DirtyStash(context.Background(), id, hash); !errors.Is(err, ErrStale) {
		t.Fatalf("changed worktree: %v", err)
	}
	if l := gittest.Git(t, wt, "stash", "list"); l != "" {
		t.Errorf("stashed anyway: %q", l)
	}
}

func TestDirtyDiff(t *testing.T) {
	m, id, _ := dirtyFixture(t, map[string]string{"a.txt": "edited", "wip.txt": "mine", "blob.bin": "\x00"})
	ctx := context.Background()
	if d, err := m.DirtyDiff(ctx, id, "a.txt"); err != nil || !strings.Contains(d, "-two") || !strings.Contains(d, "+edited") {
		t.Errorf("tracked: %q %v", d, err)
	}
	if d, err := m.DirtyDiff(ctx, id, "wip.txt"); err != nil || d != "mine" {
		t.Errorf("untracked: %q %v", d, err)
	}
	if d, err := m.DirtyDiff(ctx, id, "blob.bin"); err != nil || d != "binary file" {
		t.Errorf("binary: %q %v", d, err)
	}
	_, err := m.DirtyDiff(ctx, id, "../../etc/passwd")
	invalidWith(t, err, "not in the dirty status")
}

func TestDirtyAgentPushesBothCommitsAndResumes(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"wip.txt": "half"})
	tasks := &fakeTasks{result: okFix, edit: func(dir string) { writeFile(t, dir, "wip.txt", "whole") }}
	m.Tasks = tasks
	if _, err := m.DirtyAgent(context.Background(), id, hashOf(t, m, id), "Add wip", "make it whole"); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	m.tickWait(t) // applies the run
	h := m.must(t, id)
	if h.State != StatePROpen || h.Dirty != nil {
		t.Fatalf("handoff = %s %+v", h.State, h.Dirty)
	}
	if got := gittest.Git(t, wt, "log", "-2", "--format=%s"); got != "TLM: fix the thing\nAdd wip" {
		t.Errorf("log = %q", got)
	}
	if gittest.Git(t, wt, "rev-parse", "origin/feature/x") != gittest.Git(t, wt, "rev-parse", "HEAD") || gittest.Git(t, wt, "status", "--porcelain") != "" {
		t.Error("not pushed clean")
	}
	if p := tasks.calls[0].Prompt; !strings.Contains(p, "make it whole") || !strings.Contains(p, "+half") {
		t.Errorf("prompt = %s", p)
	}
}

func TestDirtyAgentFailureGivesTheChangesBack(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"wip.txt": "precious", "a.txt": "edited"})
	head := gittest.Git(t, wt, "rev-parse", "HEAD")
	m.Tasks = &fakeTasks{result: `{"summary":"x","escalate":true,"escalate_reason":"wrong PR"}`,
		edit: func(dir string) { writeFile(t, dir, "agent.txt", "junk") }}
	if _, err := m.DirtyAgent(context.Background(), id, hashOf(t, m, id), "Add wip", ""); err != nil {
		t.Fatal(err)
	}
	m.Wait()
	m.tickWait(t)
	h := m.must(t, id)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "wrong PR") || !strings.Contains(h.Reason, "uncommitted again") {
		t.Fatalf("handoff = %s %q", h.State, h.Reason)
	}
	if gittest.Git(t, wt, "rev-parse", "HEAD") != head {
		t.Error("WIP commit left behind")
	}
	if st := gittest.Git(t, wt, "status", "--porcelain"); st != "M a.txt\n?? wip.txt" {
		t.Errorf("status = %q", st)
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "wip.txt")); string(b) != "precious" {
		t.Errorf("wip.txt = %q", b)
	}
	if h.Dirty == nil || h.Dirty.WIP != "" {
		t.Errorf("dirty = %+v", h.Dirty)
	}
}

func TestDirtyAgentLostRunIsUnwrapped(t *testing.T) {
	m, id, wt := dirtyFixture(t, map[string]string{"wip.txt": "precious"})
	head := gittest.Git(t, wt, "rev-parse", "HEAD")
	gittest.Git(t, wt, "add", "wip.txt")
	gittest.Git(t, wt, "commit", "-qm", "wip")
	wip := gittest.Git(t, wt, "rev-parse", "HEAD")
	if _, err := m.update(id, func(h *Handoff) error { d := *h.Dirty; d.WIP = wip; h.Dirty = &d; return nil }); err != nil {
		t.Fatal(err)
	}
	m.tickWait(t) // a restart lost the run: the commit is unwrapped
	m.tickWait(t) // and the dirt seen again
	if gittest.Git(t, wt, "rev-parse", "HEAD") != head || gittest.Git(t, wt, "status", "--porcelain") != "?? wip.txt" {
		t.Error("not unwrapped")
	}
	if h := m.must(t, id); h.Dirty == nil || h.Dirty.WIP != "" {
		t.Errorf("dirty = %+v", h.Dirty)
	}
}

func TestDirtyAssessmentKeepsWIP(t *testing.T) {
	m, _, id, _ := redFixture(t, &fakeTasks{result: okFix})
	release := make(chan struct{})
	m.Assess = func(context.Context, string) (string, error) { <-release; return okAdvice, nil }
	writeFile(t, m.must(t, id).Worktree, "wip.txt", "half")
	m.tickWait(t)                // the fix agent refuses the dirty worktree
	m.tickWait(t)                // escalates
	m.tick(context.Background()) // lists the files; the assessment waits
	// DirtyAgent starts meanwhile.
	m.set(t, id, func(h *Handoff) { d := *h.Dirty; d.WIP = "abc"; h.Dirty = &d })
	close(release)
	m.Wait()
	if d := m.must(t, id).Dirty; d == nil || d.WIP != "abc" || d.Message == "" {
		t.Errorf("dirty = %+v", d)
	}
}
