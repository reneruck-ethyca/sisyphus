package lastmile

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
)

var watchedRef = github.PRRef{Repo: "ethyca/fidesplus", Number: 5}

// chainChild hands off feature/y, one commit on top of feature/x, with base feature/x and no
// --stack-parent, and ticks until its PR (#8) is open and read once.
func chainChild(t *testing.T, m *Manager, clone string, req Request) Handoff {
	t.Helper()
	ctx := context.Background()
	gh := m.GH.(*FakeGitHub)
	dir := filepath.Join(t.TempDir(), "child")
	gittest.Git(t, clone, "worktree", "add", "-q", "-b", "feature/y", dir, "feature/x")
	gittest.Commit(t, dir, "b.txt", "child")
	req.Worktree, req.Base = dir, "feature/x"
	child, err := m.Handoff(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	gh.NextPR = childRef
	m.tick(ctx)
	gh.Snapshots[childRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, dir, "rev-parse", "HEAD"), BaseRef: "feature/x"}
	m.tick(ctx)
	return m.must(t, child.ID)
}

func TestChainUnderAHandoffIsLinked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, clone, _ := fixture(t)
	gh := m.GH.(*FakeGitHub)
	gh.Snapshots = map[string]github.PRSnapshot{}
	parent, err := m.Handoff(ctx, Request{Worktree: clone, Base: "main"})
	if err != nil {
		t.Fatal(err)
	}
	gh.NextPR = prRef
	m.tick(ctx)
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), BaseRef: "main"}
	child := chainChild(t, m, clone, Request{})
	if child.StackParent != parent.ID || child.ParentPR != 0 || child.Base != "feature/x" {
		t.Fatalf("child = %+v", child)
	}
	m.tick(ctx) // the next tick notes the parent's head, as for --stack-parent
	if h := m.must(t, child.ID); h.ParentHead != gittest.Git(t, clone, "rev-parse", "HEAD") {
		t.Errorf("parent head = %q", h.ParentHead)
	}
}

// watchedFixture: feature/x is PR #5, which nobody handed off; feature/y is chained on it.
func watchedFixture(t *testing.T, req Request) (*Manager, *FakeGitHub, string, Handoff) {
	t.Helper()
	m, clone, _ := fixture(t)
	gh := m.GH.(*FakeGitHub)
	gh.Snapshots = map[string]github.PRSnapshot{}
	gh.Found = map[string]int{"feature/x": watchedRef.Number}
	gittest.Git(t, clone, "push", "-q", "origin", "feature/x")
	gh.Snapshots[watchedRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), BaseRef: "main"}
	return m, gh, clone, chainChild(t, m, clone, req)
}

func TestChainOnAPRNobodyHandedOffIsWatchedThenRestacked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, clone, child := watchedFixture(t, Request{})
	if child.ParentPR != watchedRef.Number || child.StackParent != "" {
		t.Fatalf("child = %+v", child)
	}
	m.tick(ctx)
	if h := m.must(t, child.ID); h.ParentHead != gittest.Git(t, clone, "rev-parse", "HEAD") {
		t.Fatalf("parent head = %q", h.ParentHead)
	}
	// Someone squash-merges #5.
	advanceMain(t, clone, "a.txt", "squashed")
	p := gh.Snapshots[watchedRef.Key()]
	p.State = "MERGED"
	gh.Snapshots[watchedRef.Key()] = p
	m.tick(ctx)
	h := m.must(t, child.ID)
	if !h.ParentMerged || h.Base != "main" || len(gh.Edits) == 0 || gh.Edits[len(gh.Edits)-1].Base != "main" {
		t.Fatalf("after merge: %+v, edits %+v", h, gh.Edits)
	}
	m.Wait()
	m.tick(ctx)
	h = m.must(t, child.ID)
	if h.ParentPR != 0 || h.ParentHead != "" || h.Error != "" {
		t.Fatalf("after rebase: %+v", h)
	}
	if got := gittest.Git(t, h.Worktree, "log", "--format=%s", "origin/main..HEAD"); got != "child" {
		t.Errorf("commits on top of main: %q", got)
	}
}

func TestWatchedParentClosed(t *testing.T) {
	t.Parallel()
	m, gh, _, child := watchedFixture(t, Request{})
	p := gh.Snapshots[watchedRef.Key()]
	p.State = "CLOSED"
	gh.Snapshots[watchedRef.Key()] = p
	m.tick(context.Background())
	h := m.must(t, child.ID)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "stack parent closed: #5") || h.ParentPR != 0 {
		t.Errorf("child = %+v", h)
	}
}

// A long-lived base (by name, or protected on GitHub, rulesets included) is never a
// parent, even with its own open PR: hotfix/3.3.1 -> main stays open while fixes target it.
func TestLongLivedBaseIsNoChain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		longLived []string
		protected bool
	}{{"by name", []string{"feature/*"}, false}, {"protected", nil, true}} {
		t.Run(tc.name, func(t *testing.T) {
			m, clone, _ := fixture(t)
			gh := m.GH.(*FakeGitHub)
			gh.Snapshots = map[string]github.PRSnapshot{}
			gh.Found = map[string]int{"feature/x": watchedRef.Number}
			gh.Protected = map[string]bool{"feature/x": tc.protected}
			m.cfg.LastMile.LongLivedBases = tc.longLived
			gittest.Git(t, clone, "push", "-q", "origin", "feature/x")
			if h := chainChild(t, m, clone, Request{}); h.ParentPR != 0 || h.StackParent != "" {
				t.Errorf("child = %+v", h)
			}
		})
	}
}

func TestChainLookupMissIsRetried(t *testing.T) {
	t.Parallel()
	m, clone, _ := fixture(t)
	gh := m.GH.(*FakeGitHub)
	gh.Snapshots = map[string]github.PRSnapshot{}
	gittest.Git(t, clone, "push", "-q", "origin", "feature/x")
	child := chainChild(t, m, clone, Request{}) // feature/x has no PR yet
	gh.Found = map[string]int{"feature/x": watchedRef.Number}
	gh.Snapshots[watchedRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), BaseRef: "main"}
	m.tick(context.Background())
	if h := m.must(t, child.ID); h.ParentPR != 0 {
		t.Fatalf("looked up again right away: %+v", h)
	}
	later := time.Now().Add(chainRecheck + time.Minute)
	m.now = func() time.Time { return later }
	m.tick(context.Background())
	if h := m.must(t, child.ID); h.ParentPR != watchedRef.Number {
		t.Errorf("a PR opened for the base later isn't found: %+v", h)
	}
}

func TestWatchedChildDriftReplaysOnlyItsOwnCommits(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, clone, child := watchedFixture(t, Request{})
	m.tick(ctx) // notes the parent's head
	f := gittest.Git(t, clone, "diff-tree", "--no-commit-id", "--name-only", "-r", "HEAD")
	os.WriteFile(filepath.Join(clone, f), []byte("rewritten"), 0o644)
	gittest.Git(t, clone, "commit", "-q", "-a", "--amend", "-m", "rewritten")
	gittest.Git(t, clone, "push", "-q", "-f", "origin", "feature/x")
	gh.Snapshots[watchedRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: gittest.Git(t, clone, "rev-parse", "HEAD"), BaseRef: "main"}
	c := gh.Snapshots[childRef.Key()]
	c.MergeState = "DIRTY"
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	m.Wait()
	h := m.must(t, child.ID)
	if got := gittest.Git(t, h.Worktree, "log", "--format=%s", "origin/feature/x..HEAD"); got != "child" || h.State == StateNeedsHuman {
		t.Errorf("commits on top of the parent: %q; %s %q", got, h.State, h.Reason)
	}
}

// githubAmends force-pushes a rewritten feature/y, as GitHub's restack of a mid-stack PR does.
func githubAmends(t *testing.T, clone string) string {
	t.Helper()
	other := filepath.Join(t.TempDir(), "gh")
	gittest.Git(t, filepath.Dir(other), "clone", "-q", gittest.Git(t, clone, "remote", "get-url", "origin"), other)
	gittest.Git(t, other, "checkout", "-q", "feature/y")
	gittest.Git(t, other, "commit", "-q", "--amend", "-m", "child, restacked")
	gittest.Git(t, other, "push", "-q", "-f", "origin", "feature/y")
	return gittest.Git(t, other, "rev-parse", "HEAD")
}

func TestNativeChildFollowsGitHubsPushWhileItsParentIsOpen(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, clone, child := watchedFixture(t, Request{})
	c := gh.Snapshots[childRef.Key()]
	c.Stack = &github.PRStack{Number: 3, Base: "main", Bottom: 4, Position: 3, Size: 3}
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	c.HeadSHA = githubAmends(t, clone) // #4, below #5, merged; GitHub restacked #5 and this PR
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	h := m.must(t, child.ID)
	if got := gittest.Git(t, h.Worktree, "rev-parse", "HEAD"); got != c.HeadSHA || h.State == StateNeedsHuman {
		t.Errorf("worktree at %.12s, PR at %.12s; %s %q", got, c.HeadSHA, h.State, h.Reason)
	}
	c.Stack = nil // taken out of the stack while #5 is open: an ordinary chain again
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	if h := m.must(t, child.ID); h.StackNumber != 0 {
		t.Errorf("stack number kept: %+v", h)
	}
}

func TestNativeChildWaitsForGitHubsRestackThenEscalates(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, _, child := watchedFixture(t, Request{})
	c := gh.Snapshots[childRef.Key()]
	c.Stack = &github.PRStack{Number: 3, Base: "main", Bottom: watchedRef.Number, Position: 2, Size: 2}
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	p := gh.Snapshots[watchedRef.Key()]
	p.State = "MERGED"
	gh.Snapshots[watchedRef.Key()] = p
	c.BaseRef = "main" // retargeted, but the rebased head isn't pushed yet
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	if h := m.must(t, child.ID); h.ParentPR == 0 || !strings.Contains(h.Reason, "GitHub") || retargets(gh.Edits) != 0 {
		t.Fatalf("retarget alone taken as the restack: %+v", h)
	}
	later := time.Now().Add(restackWait + time.Minute)
	m.now = func() time.Time { return later }
	m.tick(ctx)
	if h := m.must(t, child.ID); h.State != StateNeedsHuman || !strings.Contains(h.Reason, "restack") {
		t.Errorf("no escalation: %s %q", h.State, h.Reason)
	}
}

func TestNativeMergeAsyncIsAskedOnceThenEscalates(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.Stack = &github.PRStack{Number: 3, Base: "main", Bottom: prRef.Number, Position: 1, Size: 2}
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	m.tick(context.Background())
	if len(gh.AsyncMerged) != 1 {
		t.Fatalf("asked %d times", len(gh.AsyncMerged))
	}
	later := time.Now().Add(asyncMergeWait + time.Minute)
	m.now = func() time.Time { return later }
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || len(gh.AsyncMerged) != 1 {
		t.Errorf("after the wait: %s %q, asked %d times", h.State, h.Reason, len(gh.AsyncMerged))
	}
}

func TestChainChildNeverMergesIntoItsParent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, _, child := watchedFixture(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.HeadSHA, p.BaseRef, p.AutoMergeAllowed = child.Snapshot.Head, "feature/x", true
	p.Reviews[0].CommitSHA = p.HeadSHA
	gh.Snapshots[childRef.Key()] = p
	m.tick(ctx)
	h := m.must(t, child.ID)
	if len(gh.AutoMerged)+len(gh.Merged)+len(gh.AsyncMerged) != 0 || !strings.Contains(h.Reason, "waiting on") {
		t.Fatalf("auto %v merged %v async %v; %s %q", gh.AutoMerged, gh.Merged, gh.AsyncMerged, h.State, h.Reason)
	}
	_, err := m.Merge(ctx, child.ID)
	invalidWith(t, err, "waiting on")
}

func TestNativeStackMergesOnlyTheBottom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed = true
	p.Stack = &github.PRStack{Number: 3, Base: "main", Bottom: 6, Position: 2, Size: 2}
	gh.Snapshots[prRef.Key()] = p
	m.tick(ctx)
	if h := m.must(t, id); len(gh.AutoMerged)+len(gh.Merged)+len(gh.AsyncMerged) != 0 || !strings.Contains(h.Reason, "waiting on #6") {
		t.Fatalf("auto %v merged %v async %v; %q", gh.AutoMerged, gh.Merged, gh.AsyncMerged, h.Reason)
	}
}

func TestNativeBottomMergesAsync(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed = true
	p.Stack = &github.PRStack{Number: 3, Base: "main", Bottom: prRef.Number, Position: 1, Size: 2}
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.AsyncMerged) != 1 || len(gh.AutoMerged)+len(gh.Merged) != 0 || h.Error != "" {
		t.Fatalf("async %v auto %v merged %v; %+v", gh.AsyncMerged, gh.AutoMerged, gh.Merged, h)
	}
}

// githubRestacks does what GitHub does to a native stack's child once the PR below
// squash-merged: replays the child's commit onto main and force-pushes it. It returns the new head.
func githubRestacks(t *testing.T, clone string) string {
	t.Helper()
	up := gittest.Git(t, clone, "remote", "get-url", "origin")
	other := filepath.Join(t.TempDir(), "gh")
	gittest.Git(t, filepath.Dir(other), "clone", "-q", up, other)
	gittest.Git(t, other, "checkout", "-q", "-b", "restacked", "origin/main")
	gittest.Commit(t, other, "b.txt", "child")
	gittest.Git(t, other, "push", "-q", "-f", "origin", "HEAD:feature/y")
	return gittest.Git(t, other, "rev-parse", "HEAD")
}

func retargets(edits []github.PREdit) int {
	n := 0
	for _, e := range edits {
		if e.Base != "" {
			n++
		}
	}
	return n
}

func TestNativeChildLetsGitHubRestack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, clone, child := watchedFixture(t, Request{})
	native := &github.PRStack{Number: 3, Base: "main", Bottom: watchedRef.Number, Position: 2, Size: 2}
	c := gh.Snapshots[childRef.Key()]
	c.Stack = native
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	if h := m.must(t, child.ID); h.StackNumber != 3 || h.ParentPR != watchedRef.Number {
		t.Fatalf("child = %+v", h)
	}

	advanceMain(t, clone, "a.txt", "squashed")
	p := gh.Snapshots[watchedRef.Key()]
	p.State = "MERGED"
	gh.Snapshots[watchedRef.Key()] = p
	m.tick(ctx) // merged, but GitHub hasn't restacked the child yet
	h := m.must(t, child.ID)
	if _, busy := m.running.get(child.ID); retargets(gh.Edits) != 0 || busy || !strings.Contains(h.Reason, "GitHub") {
		t.Fatalf("before GitHub's restack: edits %+v, %+v", gh.Edits, h)
	}

	head := githubRestacks(t, clone)
	c.BaseRef, c.HeadSHA, c.Stack = "main", head, nil // a one-PR stack is gone
	gh.Snapshots[childRef.Key()] = c
	m.tick(ctx)
	h = m.must(t, child.ID)
	if retargets(gh.Edits) != 0 || h.Base != "main" || h.ParentPR != 0 || h.StackNumber != 0 || h.State == StateNeedsHuman {
		t.Fatalf("after GitHub's restack: edits %+v, %+v", gh.Edits, h)
	}
	if got := gittest.Git(t, h.Worktree, "rev-parse", "HEAD"); got != head {
		t.Errorf("worktree at %s, want GitHub's %s", got, head)
	}
}
