package lastmile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// TestMain points HOME at a scratch dir for the whole package, so config
// defaults stay out of the user's, and its tests can run in parallel.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "lastmile-test-home")
	if err != nil {
		panic(err)
	}
	os.Setenv("HOME", home)
	flockPoll = 10 * time.Millisecond
	code := m.Run()
	os.RemoveAll(home)
	os.Exit(code)
}

// fixture is a clone on feature/x, one commit ahead of origin/main, whose origin
// reads as ethyca/fidesplus on GitHub but fetches from the local upstream.
func fixture(t *testing.T) (*Manager, string, string) {
	t.Helper()
	upstream, clone := gittest.Setup(t)
	gittest.Git(t, clone, "config", "url."+upstream+".insteadOf", "https://github.com/ethyca/fidesplus.git")
	gittest.Git(t, clone, "remote", "set-url", "origin", "https://github.com/ethyca/fidesplus.git")
	gittest.Git(t, upstream, "config", "receive.denyCurrentBranch", "ignore") // handoffs push the branch it has checked out
	cfg, err := config.Load(filepath.Join(t.TempDir(), "none.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	m, err := New(cfg, &FakeGitHub{}, &tracker.Locks{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	m.removeWT = func(context.Context, string, string) error { return nil }
	return m, clone, dir
}

func invalidWith(t *testing.T, err error, want string) {
	t.Helper()
	var ie InvalidError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want an InvalidError containing %q", err, want)
	}
}

func TestHandoffAccepts(t *testing.T) {
	t.Parallel()
	m, clone, dir := fixture(t)
	os.WriteFile(filepath.Join(clone, "untracked.env"), []byte("x"), 0o644) // wtp hooks copy such files in
	h, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main", Jira: "ENG-12", Labels: []string{" a "},
		Reviewers: []string{"alice"}, Autonomy: "fix-ci,merge=shadow", Agent: "codex:gpt:high", Notify: "echo {{.ID}}", Requester: "orca"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h.ID, "fidesplus-feature-x-") || len(h.ID) != len("fidesplus-feature-x-")+4 {
		t.Errorf("id = %q", h.ID)
	}
	if h.Repo != "ethyca/fidesplus" || h.Branch != "feature/x" || h.State != StateHandedOff || h.Title != "two" ||
		h.MaxFixRounds != 3 || !h.Autonomy.FixCI || h.Autonomy.AddressReviews || h.Autonomy.Merge != MergeShadow ||
		h.Agent.Vendor != "codex" || h.Agent.Effort != "high" || len(h.Labels) != 1 || h.Labels[0] != "a" ||
		strings.Join(h.NotifyOn, ",") != "merged,needs-human,closed" || h.Created.IsZero() {
		t.Errorf("handoff = %+v", h)
	}
	if got, ok := m.Get(h.ID); !ok || got.ID != h.ID || len(m.List()) != 1 {
		t.Error("handoff isn't listed")
	}
	again, err := New(m.cfg, m.GH, m.locks, dir)
	if err != nil || len(again.List()) != 1 || again.List()[0].ID != h.ID {
		t.Errorf("reloaded = %+v, %v", again.List(), err)
	}

	_, err = m.Handoff(context.Background(), Request{Worktree: clone, Base: "main"})
	invalidWith(t, err, "already owned by handoff "+h.ID)
}

func TestHandoffRelease(t *testing.T) {
	t.Parallel()
	m, clone, _ := fixture(t)
	h, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main", Title: "t"})
	if err != nil {
		t.Fatal(err)
	}
	m.handoffs[0].State = StateMerged
	h2, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main"})
	if err != nil || h2.ID == h.ID {
		t.Fatalf("a finished handoff releases its branch: %v", err)
	}
}

func TestHandoffRejects(t *testing.T) {
	t.Parallel()
	m, clone, _ := fixture(t)
	ctx := context.Background()
	ok := Request{Worktree: clone, Base: "main"}
	with := func(f func(*Request)) Request { r := ok; f(&r); return r }

	for _, c := range []struct {
		name string
		req  Request
		want string
	}{
		{"relative worktree", with(func(r *Request) { r.Worktree = "rel/path" }), "absolute"},
		{"no base", with(func(r *Request) { r.Base = "" }), "base"},
		{"flag-like base", with(func(r *Request) { r.Base = "-x" }), "base"},
		{"bad jira", with(func(r *Request) { r.Jira = "nope" }), "jira"},
		{"unknown merge mode", with(func(r *Request) { r.Autonomy = "fix-ci,merge=yes" }), "merge=yes"},
		{"unknown autonomy", with(func(r *Request) { r.Autonomy = "yolo" }), "autonomy"},
		{"bad agent", with(func(r *Request) { r.Agent = "claude" }), "vendor:model:effort"},
		{"unknown vendor", with(func(r *Request) { r.Agent = "gemini:x:y" }), "vendor"},
		{"negative rounds", with(func(r *Request) { r.MaxFixRounds = -1 }), "max_fix_rounds"},
		{"label with comma", with(func(r *Request) { r.Labels = []string{"a,b"} }), "label"},
		{"reviewer not a login", with(func(r *Request) { r.Reviewers = []string{"a b"} }), "reviewer"},
		{"bad template", with(func(r *Request) { r.Notify = "echo {{.ID" }), "notify template"},
		{"bad event", with(func(r *Request) { r.Notify, r.NotifyOn = "true", []string{"boom"} }), "notify_on"},
		{"events without command", with(func(r *Request) { r.NotifyOn = []string{"merged"} }), "notify_on needs"},
		{"base can't be fetched", with(func(r *Request) { r.Base = "nope" }), "fetching base"},
		{"nothing ahead", with(func(r *Request) { r.Base = "feature/x" }), "no commits ahead"},
	} {
		if _, err := m.Handoff(ctx, c.req); err == nil {
			t.Errorf("%s: accepted", c.name)
		} else {
			t.Run(c.name, func(t *testing.T) { invalidWith(t, err, c.want) })
		}
	}

	os.WriteFile(filepath.Join(clone, "a.txt"), []byte("edited"), 0o644)
	_, err := m.Handoff(ctx, ok)
	invalidWith(t, err, "isn't clean")
	gittest.Git(t, clone, "checkout", "-q", "a.txt")

	gittest.Git(t, clone, "checkout", "-q", "--detach")
	_, err = m.Handoff(ctx, ok)
	invalidWith(t, err, "no branch")
	gittest.Git(t, clone, "checkout", "-q", "feature/x")

	m.cfg.LastMile.ManualMerge = []string{"ethyca/fidesplus"}
	for _, mode := range []string{MergeAuto, MergeApproved} {
		_, err = m.Handoff(ctx, with(func(r *Request) { r.Autonomy = "fix-ci,merge=" + mode }))
		invalidWith(t, err, "merges only from the dashboard")
	}
	m.cfg.LastMile.ManualMerge = nil

	gittest.Git(t, clone, "remote", "set-url", "origin", "https://github.com/ethyca/other.git")
	_, err = m.Handoff(ctx, ok)
	invalidWith(t, err, "isn't in review.repos")

	if len(m.List()) != 0 {
		t.Errorf("rejected handoffs were recorded: %+v", m.List())
	}
}

func TestParseRepo(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"https://github.com/ethyca/fidesplus.git": "ethyca/fidesplus",
		"https://github.com/ethyca/fidesplus":     "ethyca/fidesplus",
		"git@github.com:ethyca/fides-qa.git":      "ethyca/fides-qa",
		"ssh://git@github.com/ethyca/fides.git":   "ethyca/fides",
	} {
		if got, err := parseRepo(in); err != nil || got != want {
			t.Errorf("parseRepo(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "github.com", "https://github.com/onlyowner", "https://github.com/a/b/c",
		"/Users/x/ethyca/fidesplus", "https://evil.example/ethyca/fidesplus.git", "git@evil.example:ethyca/fidesplus.git", "file:///tmp/ethyca/fidesplus"} {
		if _, err := parseRepo(bad); err == nil {
			t.Errorf("parseRepo(%q) should fail", bad)
		}
	}
}

func TestNotifyIsInert(t *testing.T) {
	t.Parallel()
	evil := `'; rm -rf / #`
	dir := t.TempDir()
	h := Handoff{ID: "x-1", Repo: "o/r", Branch: "b", Worktree: dir, Requester: evil, Reason: "a'b\"c$(touch pwned)`touch pwned`",
		Notify:   `printf %s {{.Requester}} > who; printf %s {{.Reason}} > why; printf '%s|%s' "$TLM_EVENT" "$TLM_ID" > env`,
		NotifyOn: []string{EventNeedsHuman}, State: StateNeedsHuman}
	m := &Manager{runHook: tracker.RunHook, cfg: config.Config{HookTimeout: config.Duration{Duration: time.Minute}}}
	m.Notify(context.Background(), h, EventMerged) // not asked for
	if _, err := os.Stat(filepath.Join(dir, "who")); err == nil {
		t.Fatal("notified for an event the handoff didn't ask for")
	}
	m.Notify(context.Background(), h, EventNeedsHuman)
	read := func(f string) string { b, _ := os.ReadFile(filepath.Join(dir, f)); return string(b) }
	if read("who") != evil || read("why") != h.Reason || read("env") != "needs-human|x-1" {
		t.Errorf("who=%q why=%q env=%q", read("who"), read("why"), read("env"))
	}
	if _, err := os.Stat(filepath.Join(dir, "pwned")); err == nil {
		t.Error("a value ran as shell")
	}
}

func TestRenderNotify(t *testing.T) {
	t.Parallel()
	h := Handoff{ID: "i", PRNumber: 7, Branch: "it's"}
	got, err := renderNotify("go {{.ID}} {{.PRNumber}} {{.Branch}} [{{.PRURL}}]", h, EventMerged)
	if err != nil || got != `go 'i' '7' 'it'\''s' ['']` {
		t.Errorf("got %q, %v", got, err)
	}
	if _, err := renderNotify("{{.Nope}}", h, EventMerged); err == nil {
		t.Error("unknown field should fail")
	}
}

func TestActionsNotifyOnceWithoutTheLock(t *testing.T) {
	t.Parallel()
	m, clone, _ := fixture(t)
	changes := 0
	m.OnChange = func() {
		m.Views() // takes the lock: deadlocks if a caller still holds it
		changes++
	}
	expect := func(what string, want int) {
		t.Helper()
		if changes != want {
			t.Fatalf("after %s: %d changes, want %d", what, changes, want)
		}
	}
	h, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	expect("handoff", 1)
	if _, err := m.SetReviewers(h.ID, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	expect("reviewers", 2)
	if _, err := m.SetReviewers("nope", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	expect("a failed edit", 2)
	m.set(t, h.ID, func(h *Handoff) { h.State = StateNeedsHuman })
	if _, err := m.Resume(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	expect("resume", 3)
	if _, err := m.Cancel(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	expect("cancel", 4)
}

func TestExcerptKeepsTheFailingTestAboveTheTail(t *testing.T) {
	log := "2026-11-24T16:37:43Z E   TimeoutError\n2026-11-24T16:37:44Z FAILED tests/t.py::test_x\n" +
		strings.Repeat("nox teardown\n", logTailLines)
	got := excerpt(log)
	if !strings.Contains(got, "FAILED tests/t.py::test_x") || !strings.Contains(got, "E   TimeoutError") ||
		strings.Count(got, "nox teardown") != logTailLines {
		t.Fatalf("excerpt:\n%s", got)
	}
	if short := "FAILED tests/t.py::test_x\n"; excerpt(short) != strings.TrimRight(short, "\n") {
		t.Fatalf("a short log changed: %q", excerpt(short))
	}
}
