package lastmile

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/proc"
)

var childRef = github.PRRef{Repo: "ethyca/fidesplus", Number: 8}

// advanceMain commits file to the upstream's main through a second clone, as a merge by someone else would.
func advanceMain(t *testing.T, clone, file, content string) {
	t.Helper()
	up := gittest.Git(t, clone, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other")
	gittest.Git(t, filepath.Dir(other), "clone", "-q", up, other)
	gittest.Git(t, other, "checkout", "-q", "main")
	gittest.Commit(t, other, file, content)
	gittest.Git(t, other, "push", "-q", "origin", "main")
}

// stackFixture hands off feature/x as the parent (PR #7) and feature/y, one commit on top of it, as
// its child (PR #8), and ticks until both PRs are open and the child has noted the parent's head.
func stackFixture(t *testing.T, childReq Request) (m *Manager, gh *FakeGitHub, parent, child Handoff, events *[]string) {
	t.Helper()
	ctx := context.Background()
	m, clone, _ := fixture(t)
	gh = m.GH.(*FakeGitHub)
	gh.Snapshots = map[string]github.PRSnapshot{}
	events = new([]string)
	m.runHook = func(_ context.Context, _ string, _ time.Duration, _ string, env map[string]string) error {
		*events = append(*events, env["TLM_ID"]+" "+env["TLM_EVENT"])
		return nil
	}
	parent, err := m.Handoff(ctx, Request{Worktree: clone, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	gh.NextPR = prRef
	m.tick(ctx)
	dir := filepath.Join(t.TempDir(), "child")
	gittest.Git(t, clone, "worktree", "add", "-q", "-b", "feature/y", dir, "feature/x")
	gittest.Commit(t, dir, "b.txt", "child")
	childReq.Worktree, childReq.Base, childReq.StackParent = dir, "main", parent.ID
	childReq.Notify, childReq.NotifyOn = "true", []string{EventStackParentMerged, EventNeedsHuman}
	if child, err = m.Handoff(ctx, childReq); err != nil {
		t.Fatal(err)
	}
	gh.NextPR = childRef
	m.tick(ctx)
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), BaseRef: "main"}
	gh.Snapshots[childRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, dir, "rev-parse", "HEAD"), BaseRef: "feature/x"}
	m.tick(ctx)
	return m, gh, m.must(t, parent.ID), m.must(t, child.ID), events
}

func TestStackParentResolution(t *testing.T) {
	ctx := context.Background()
	m, gh, parent, child, _ := stackFixture(t, Request{})
	if child.StackParent != parent.ID || child.Base != "feature/x" || gh.Created[1].Base != "feature/x" || child.PRNumber != 8 {
		t.Errorf("child = %+v, created %+v", child, gh.Created)
	}
	if sha := gittest.Git(t, parent.Worktree, "rev-parse", "origin/feature/x"); child.ParentHead != sha {
		t.Errorf("parent head = %q, want %q", child.ParentHead, sha)
	}

	n := 0
	newChild := func(parentName string) (Handoff, error) {
		n++
		dir := filepath.Join(t.TempDir(), "w")
		gittest.Git(t, parent.Worktree, "worktree", "add", "-q", "-b", "feature/n"+string(rune('0'+n)), dir, "feature/x")
		gittest.Commit(t, dir, "c.txt", "c")
		return m.Handoff(ctx, Request{Worktree: dir, Base: "main", StackParent: parentName})
	}
	byBranch, err := newChild("feature/x") // a branch with an open handoff is that handoff
	if err != nil || byBranch.StackParent != parent.ID || byBranch.Base != "feature/x" {
		t.Errorf("by branch = %+v, %v", byBranch, err)
	}
	byID, err := newChild(child.ID)
	if err != nil || byID.StackParent != child.ID || byID.Base != "feature/y" {
		t.Errorf("by id = %+v, %v", byID, err)
	}
	plain, err := newChild("main") // nobody's branch: just the base
	if err != nil || plain.StackParent != "" || plain.Base != "main" {
		t.Errorf("plain = %+v, %v", plain, err)
	}

	// Cycles: the parent's chain reaches the new handoff's own branch.
	_, err = m.Handoff(ctx, Request{Worktree: parent.Worktree, Base: "main", StackParent: "feature/x"})
	invalidWith(t, err, "cycle")
	_, err = m.Handoff(ctx, Request{Worktree: parent.Worktree, Base: "main", StackParent: child.ID})
	invalidWith(t, err, "cycle")

	m.handoffs[0].Repo = "ethyca/other"
	_, err = newChild(parent.ID)
	invalidWith(t, err, "belongs to ethyca/other")
	m.handoffs[0].Repo, m.handoffs[0].State = "ethyca/fidesplus", StateMerged
	_, err = newChild(parent.ID)
	invalidWith(t, err, "is merged")
}

func TestStackChildWaitsOnParentThenDashboardGroups(t *testing.T) {
	m, _, parent, child, _ := stackFixture(t, Request{})
	views := m.Views()
	if len(views) != 2 || views[0].ID != parent.ID || views[0].Depth != 0 || views[1].ID != child.ID || views[1].Depth != 1 || views[1].WaitingOn != parent.ID {
		t.Fatalf("views = %+v", views)
	}
	m.handoffs[0].State = StateMerged
	if v := m.Views(); v[1].WaitingOn != "" {
		t.Errorf("waiting on a merged parent: %+v", v[1])
	}
}

func TestStackParentMergedRetargetsAndRebasesOnto(t *testing.T) {
	ctx := context.Background()
	m, gh, parent, child, events := stackFixture(t, Request{})
	// The parent lands as a squash commit: its own sha is gone, and its change differs textually.
	advanceMain(t, parent.Worktree, "a.txt", "squashed")
	m.handoffs[0].State = StateMerged
	m.tick(ctx)
	h := m.must(t, child.ID)
	if h.State != StateCIRunning || h.Base != "main" || !h.ParentMerged || len(gh.Edits) == 0 || gh.Edits[len(gh.Edits)-1].Base != "main" {
		t.Fatalf("after tick: %+v, edits %+v", h, gh.Edits)
	}
	if !slices.Contains(*events, child.ID+" stack-parent-merged") {
		t.Errorf("events = %v", *events)
	}
	m.Wait()
	m.tick(ctx)
	h = m.must(t, child.ID)
	if h.State != StateCIRunning || h.StackParent != "" || h.ParentHead != "" || h.Error != "" {
		t.Fatalf("after rebase: %+v", h)
	}
	dir := h.Worktree
	if got := gittest.Git(t, dir, "log", "--format=%s", "origin/main..HEAD"); got != "child" {
		t.Errorf("commits on top of main: %q", got) // the parent's commit is skipped, not replayed
	}
	if gittest.Git(t, dir, "rev-parse", "origin/feature/y") != gittest.Git(t, dir, "rev-parse", "HEAD") {
		t.Error("not force-pushed")
	}
	if got := gittest.Git(t, dir, "show", "HEAD~:a.txt"); got != "squashed" {
		t.Errorf("base content = %q", got)
	}
	kinds := ""
	for _, e := range h.Events {
		kinds += e.Kind + " "
	}
	for _, k := range []string{"retarget", "rebase", "push"} {
		if !strings.Contains(kinds, k) {
			t.Errorf("no %s event in %s", k, kinds)
		}
	}
	if n := len(*events); n != 1 {
		t.Errorf("notified %d times: %v", n, *events)
	}
}

func TestStackParentClosed(t *testing.T) {
	m, _, parent, child, events := stackFixture(t, Request{})
	m.handoffs[0].State = StateClosed
	m.tick(context.Background())
	h := m.must(t, child.ID)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "stack parent closed: "+parent.ID) || h.StackParent != "" {
		t.Errorf("child = %+v", h)
	}
	if !slices.Contains(*events, child.ID+" needs-human") {
		t.Errorf("events = %v", *events)
	}
}

func TestDriftCleanRebase(t *testing.T) {
	ctx := context.Background()
	tasks := &fakeTasks{}
	m, gh, _, id := opened(t, Request{})
	m.Tasks = tasks
	h := m.must(t, id)
	pre := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	hooks, _ := filepath.Abs(gittest.Git(t, h.Worktree, "rev-parse", "--git-path", "hooks"))
	if !filepath.IsAbs(hooks) {
		hooks = filepath.Join(h.Worktree, hooks)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	os.MkdirAll(hooks, 0o755)
	for _, name := range []string{"post-rewrite", "post-checkout", "pre-push"} {
		os.WriteFile(filepath.Join(hooks, name), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	}
	advanceMain(t, h.Worktree, "main-only.txt", "m")

	snap := func(state string) {
		gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: pre, BaseRef: "main", Author: "me", MergeState: state}
	}
	// Only GitHub's word starts a rebase: a moved base on a CLEAN PR is left alone.
	snap("CLEAN")
	m.tickWait(t)
	if got := gittest.Git(t, h.Worktree, "rev-parse", "HEAD"); got != pre {
		t.Fatalf("rebased without being told")
	}
	snap("BEHIND")
	m.tick(ctx)
	if got := m.must(t, id); got.State != StateCIRunning || !strings.Contains(got.Reason, "rebasing onto main") {
		t.Fatalf("after launch: %+v", got)
	}
	m.Wait()
	m.tick(ctx)
	h = m.must(t, id)
	if h.Error != "" || h.State != StateCIRunning || tasks.n() != 0 {
		t.Fatalf("after run: %+v, agent calls %d", h, tasks.n())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("a git hook ran during the rebase or the push")
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "HEAD") == pre || gittest.Git(t, h.Worktree, "merge-base", "--is-ancestor", "origin/main", "HEAD") != "" {
		t.Error("not rebased onto origin/main")
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x") != gittest.Git(t, h.Worktree, "rev-parse", "HEAD") {
		t.Error("not force-pushed")
	}
	var logged []string
	for _, e := range h.Events {
		logged = append(logged, e.Kind+": "+e.Text)
	}
	if j := strings.Join(logged, "\n"); !strings.Contains(j, "no conflict predicted") || !strings.Contains(j, "cleanly") || !strings.Contains(j, "force-pushed") {
		t.Errorf("events:\n%s", j)
	}
}

func TestMergeTreePrediction(t *testing.T) {
	m, _, _, id := opened(t, Request{})
	dir := m.must(t, id).Worktree
	ctx := context.Background()
	gittest.Git(t, dir, "fetch", "-q", "origin")
	if files, err := mergeTree(ctx, dir, "main"); err != nil || len(files) != 0 {
		t.Errorf("clean: %v %v", files, err)
	}
	advanceMain(t, dir, "a.txt", "three")
	gittest.Git(t, dir, "fetch", "-q", "origin")
	if files, err := mergeTree(ctx, dir, "main"); err != nil || !slices.Equal(files, []string{"a.txt"}) {
		t.Errorf("conflict: %v %v", files, err)
	}
	if _, err := mergeTree(ctx, dir, "nope"); err == nil {
		t.Error("a missing base is an error, not a conflict")
	}
}

// conflictFixture is a handoff with fix-ci whose PR is DIRTY: feature/x changes file (and, with two, a second
// commit changes it again) while main changed it differently. Before any tick the worktree is at pre.
func conflictFixture(t *testing.T, tasks *fakeTasks, file string, twoCommits bool) (m *Manager, gh *FakeGitHub, id, pre string) {
	t.Helper()
	m, clone, _ := fixture(t)
	gh = m.GH.(*FakeGitHub)
	gh.NextPR, gh.Snapshots = prRef, map[string]github.PRSnapshot{}
	m.runHook = func(context.Context, string, time.Duration, string, map[string]string) error { return nil }
	m.Tasks = tasks
	if file != "a.txt" {
		gittest.Commit(t, clone, file, "feature")
	}
	if twoCommits {
		gittest.Commit(t, clone, file, "feature-2")
	}
	h, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main", Autonomy: "fix-ci", MaxFixRounds: 3})
	if err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background()) // opens the PR
	pre = gittest.Git(t, clone, "rev-parse", "HEAD")
	advanceMain(t, clone, file, "three")
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: pre, BaseRef: "main", Author: "me", Mergeable: "CONFLICTING", MergeState: "DIRTY"}
	return m, gh, h.ID, pre
}

// resolver is an agent edit that resolves a.txt, counting its calls.
func resolver(file string) func(string) {
	n := 0
	return func(dir string) {
		n++
		os.WriteFile(filepath.Join(dir, file), []byte("resolved"+string(rune('0'+n))+"\n"), 0o644)
	}
}

func TestConflictAgentResolves(t *testing.T) {
	tasks := &fakeTasks{result: okFix, edit: resolver("a.txt")}
	m, _, id, pre := conflictFixture(t, tasks, "a.txt", false)
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateConflict || !strings.Contains(h.Reason, "a.txt") {
		t.Fatalf("after launch: %+v", h)
	}
	m.Wait()
	m.tick(context.Background())
	h := m.must(t, id)
	if h.State != StateCIRunning || h.Error != "" || h.FixRounds[TriggerConflict] != 1 {
		t.Fatalf("after run: %+v", h)
	}
	dir := h.Worktree
	if got := gittest.Git(t, dir, "show", "HEAD:a.txt"); got != "resolved1" {
		t.Errorf("a.txt = %q", got)
	}
	if gittest.Git(t, dir, "status", "--porcelain") != "" || rebaseInProgress(context.Background(), dir) {
		t.Error("worktree dirty or mid-rebase")
	}
	if gittest.Git(t, dir, "rev-parse", "origin/feature/x") != gittest.Git(t, dir, "rev-parse", "HEAD") || gittest.Git(t, dir, "rev-parse", "HEAD") == pre {
		t.Error("not force-pushed")
	}
	if got := gittest.Git(t, dir, "log", "-1", "--format=%s"); got != "two" {
		t.Errorf("replayed commit subject = %q", got) // the rebase keeps the commit, no TLM: prefix
	}
	c := tasks.calls[0]
	if !strings.Contains(c.Prompt, "a.txt") || !strings.Contains(c.Prompt, "<<<<<<<") || !strings.Contains(c.Prompt, "two") || !strings.Contains(c.Prompt, "/out/result.json") ||
		c.Context["ours-0"] != "three" || c.Context["theirs-0"] != "two" || c.Context["conflict.diff"] == "" {
		t.Errorf("task = %+v", c)
	}
	var kinds string
	for _, e := range h.Events {
		kinds += e.Kind + " "
	}
	if !strings.Contains(kinds, "agent") || !strings.Contains(kinds, "rebase") || !strings.Contains(kinds, "push") {
		t.Errorf("events: %s", kinds)
	}
}

func TestConflictAgentTwoStopsAndRoundLimit(t *testing.T) {
	tasks := &fakeTasks{result: okFix, edit: resolver("a.txt")}
	m, _, id, _ := conflictFixture(t, tasks, "a.txt", true)
	m.tickWait(t)
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateCIRunning || h.FixRounds[TriggerConflict] != 2 || tasks.n() != 2 {
		t.Fatalf("two stops: %+v, calls %d", h, tasks.n())
	}

	tasks = &fakeTasks{result: okFix, edit: resolver("a.txt")}
	m, _, id, pre := conflictFixture(t, tasks, "a.txt", true)
	m.handoffs[0].MaxFixRounds = 1
	m.tickWait(t)
	m.tick(context.Background())
	h := m.must(t, id)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "still conflicting after 1 rounds") || h.EscalatedBy != TriggerConflict {
		t.Fatalf("round limit: %+v", h)
	}
	assertRolledBack(t, h, pre)
}

// assertRolledBack: the worktree is at pre, clean, not mid-rebase, and origin untouched.
func assertRolledBack(t *testing.T, h Handoff, pre string) {
	t.Helper()
	dir := h.Worktree
	if gittest.Git(t, dir, "rev-parse", "HEAD") != pre || gittest.Git(t, dir, "status", "--porcelain") != "" || rebaseInProgress(context.Background(), dir) {
		t.Errorf("worktree not back at %.7s: head %.7s", pre, gittest.Git(t, dir, "rev-parse", "HEAD"))
	}
	if gittest.Git(t, dir, "rev-parse", "origin/"+h.Branch) != pre {
		t.Error("origin moved")
	}
}

func TestConflictAgentRejections(t *testing.T) {
	escalate := `{"summary":"","confidence":"low","escalate":true,"escalate_reason":"semantic clash"}`
	for _, c := range []struct {
		name   string
		edit   func(dir string)
		result string
		want   string
	}{
		{"markers left", func(string) {}, okFix, "conflict markers are left in a.txt"},
		{"markers half removed", func(dir string) {
			os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x\n=======\ny\n"), 0o644)
		}, okFix, "conflict markers are left"},
		{"other file edited", func(dir string) {
			resolver("a.txt")(dir)
			os.WriteFile(filepath.Join(dir, "other.txt"), []byte("sneaky"), 0o644)
		}, okFix, "wasn't in conflict: other.txt"},
		{"nested repo", func(dir string) {
			resolver("a.txt")(dir)
			os.MkdirAll(filepath.Join(dir, "nested", ".git"), 0o755)
			os.WriteFile(filepath.Join(dir, "nested", "f"), []byte("x"), 0o644)
		}, okFix, "wasn't in conflict: nested/"},
		{"symlink", func(dir string) {
			os.Remove(filepath.Join(dir, "a.txt"))
			os.Symlink("/etc/passwd", filepath.Join(dir, "a.txt"))
		}, okFix, "other than a regular file"},
		{"escalate", resolver("a.txt"), escalate, "agent escalated: semantic clash"},
		{"invalid result", resolver("a.txt"), "nope", "result.json is not valid"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tasks := &fakeTasks{result: c.result, edit: c.edit}
			m, _, id, pre := conflictFixture(t, tasks, "a.txt", false)
			m.tickWait(t)
			m.tick(context.Background())
			h := m.must(t, id)
			if h.State != StateNeedsHuman || !strings.HasPrefix(h.Reason, "rebase conflict: a.txt") || !strings.Contains(h.Reason, c.want) {
				t.Fatalf("handoff = %+v", h)
			}
			assertRolledBack(t, h, pre)
			if exists(filepath.Join(h.Worktree, "other.txt")) {
				t.Error("the agent's file survived")
			}
		})
	}
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestConflictWithoutAgentOrOnProtectedPath(t *testing.T) {
	// fix-ci off: merge-tree predicts the conflict, no rebase is started, a human is told.
	m, _, id, pre := conflictFixture(t, &fakeTasks{}, "a.txt", false)
	m.handoffs[0].Autonomy.FixCI = false
	m.tick(context.Background())
	h := m.must(t, id)
	if h.State != StateNeedsHuman || h.Reason != "rebase conflict: a.txt" || h.EscalatedBy != TriggerConflict {
		t.Fatalf("fix-ci off: %+v", h)
	}
	assertRolledBack(t, h, pre)

	// A protected path in conflict never reaches the agent.
	tasks := &fakeTasks{result: okFix, edit: resolver("go.sum")}
	m, _, id, pre = conflictFixture(t, tasks, "go.sum", false)
	m.tickWait(t)
	m.tick(context.Background())
	h = m.must(t, id)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "rebase conflict: go.sum") || !strings.Contains(h.Reason, "protected") || tasks.n() != 0 {
		t.Fatalf("protected: %+v, calls %d", h, tasks.n())
	}
	assertRolledBack(t, h, pre)

	// No runtime for agents (reviews disabled).
	m, _, id, _ = conflictFixture(t, &fakeTasks{}, "a.txt", false)
	m.Tasks = nil
	m.tickWait(t)
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || !strings.Contains(h.Reason, "agents need reviews enabled") {
		t.Errorf("no runtime: %+v", h)
	}
}

func TestStaleRebaseIsAborted(t *testing.T) {
	m, gh, _, id := opened(t, Request{})
	h := m.must(t, id)
	pre := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	advanceMain(t, h.Worktree, "a.txt", "three")
	gittest.Git(t, h.Worktree, "fetch", "-q", "origin")
	if _, err := rebaseCmd(t, h.Worktree); err == nil || !rebaseInProgress(context.Background(), h.Worktree) {
		t.Fatalf("setup: the rebase should stop on a conflict: %v", err)
	}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: pre, BaseRef: "main", Author: "me"}
	m.tick(context.Background())
	if rebaseInProgress(context.Background(), h.Worktree) || gittest.Git(t, h.Worktree, "rev-parse", "HEAD") != pre {
		t.Error("the rebase was left in progress")
	}
	got := m.must(t, id)
	if !slices.ContainsFunc(got.Events, func(e Event) bool { return e.Kind == "rebase" && strings.Contains(e.Text, "aborted") }) {
		t.Errorf("events = %+v", got.Events)
	}
}

func rebaseCmd(t *testing.T, dir string) (string, error) {
	t.Helper()
	return proc.Run(context.Background(), dir, "git", "rebase", "origin/main")
}

func TestForceWithLeaseRefusesWhenOriginMoved(t *testing.T) {
	tasks := &fakeTasks{result: okFix, edit: resolver("a.txt")}
	m, _, id, pre := conflictFixture(t, tasks, "a.txt", false)
	h := m.must(t, id)
	// Someone pushes to the PR branch after TLM last looked.
	up := gittest.Git(t, h.Worktree, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "other")
	gittest.Git(t, filepath.Dir(other), "clone", "-q", up, other)
	gittest.Git(t, other, "checkout", "-q", "feature/x")
	moved := gittest.Commit(t, other, "theirs.txt", "someone else")
	gittest.Git(t, other, "push", "-q", "origin", "feature/x")

	m.tickWait(t)
	m.tick(context.Background())
	h = m.must(t, id)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "force-with-lease") {
		t.Fatalf("handoff = %+v", h)
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "HEAD") != pre || rebaseInProgress(context.Background(), h.Worktree) {
		t.Error("worktree not back at its head")
	}
	if got := gittest.Git(t, h.Worktree, "ls-remote", "origin", "refs/heads/feature/x"); !strings.HasPrefix(got, moved) {
		t.Errorf("origin's branch was overwritten: %s", got)
	}
}
