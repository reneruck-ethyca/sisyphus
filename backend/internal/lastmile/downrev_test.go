package lastmile

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

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/proc"
)

const versions = "src/app/alembic/versions"

var migrationGlobs = []string{"**/migrations/**", "**/alembic/versions/**"}

// revisionFile is a revision file as fidesplus's alembic template writes it; down is the whole down_revision line.
func revisionFile(rev, down string) string {
	return fmt.Sprintf(`"""%s

Revision ID: %s
Revises: whatever
Create Date: 2026-11-18 09:00:00.000000

"""

import sqlalchemy as sa
from alembic import op

# revision identifiers, used by Alembic.
revision = "%s"
%s
branch_labels = None
depends_on = None


def upgrade():
    # a revision = "nope" in a comment is not an assignment
    pass
`, rev, rev, rev, down)
}

func dr(rev string) string { return `down_revision = "` + rev + `"` }

// commitMigration writes the revision file rev under versions in dir and commits it.
func commitMigration(t *testing.T, dir, rev, down string) {
	t.Helper()
	p := filepath.Join(dir, versions, rev+".py")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(revisionFile(rev, down)), 0o644); err != nil {
		t.Fatal(err)
	}
	gittest.Git(t, dir, "add", "-A")
	gittest.Git(t, dir, "commit", "-qm", "add "+rev)
}

// rebased is a clone whose origin/main has the base migrations and whose feature/x adds
// the PR's on top of it: the worktree as a rebase leaves it. Each pair is {rev, down line}.
func rebased(t *testing.T, base, pr [][2]string) string {
	t.Helper()
	_, clone := gittest.Setup(t)
	for _, b := range base {
		commitMigration(t, clone, b[0], b[1])
	}
	gittest.Git(t, clone, "update-ref", "refs/remotes/origin/main", "HEAD")
	for _, p := range pr {
		commitMigration(t, clone, p[0], p[1])
	}
	return clone
}

func read(t *testing.T, dir, rev string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, versions, rev+".py"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestBumpDownrevRewritesFirstMigration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := [][2]string{{"b0", "down_revision = None"}, {"b1a", dr("b0")}, {"b1b", dr("b0")},
		{"b1", `down_revision = ("b1a", "b1b")`}, {"b2", "down_revision = 'b1'"}}
	dir := rebased(t, base, [][2]string{{"p1", "down_revision: Union[str, None] = 'b1'  # stale"}, {"p2", dr("p1")}})
	before := gittest.Git(t, dir, "rev-parse", "HEAD")
	p2 := read(t, dir, "p2")
	changed, err := bumpDownrev(ctx, dir, "main", migrationGlobs)
	if err != nil || !slices.Equal(changed, []string{versions + "/p1.py"}) {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if got := read(t, dir, "p1"); !strings.Contains(got, "\ndown_revision: Union[str, None] = 'b2'  # stale\n") ||
		!strings.Contains(got, "\nRevises: b2\n") || strings.Count(got, "b1") != 0 {
		t.Errorf("p1:\n%s", got)
	}
	if read(t, dir, "p2") != p2 {
		t.Error("p2 changed")
	}
	if n := gittest.Git(t, dir, "rev-list", "--count", before+"..HEAD"); n != "1" {
		t.Errorf("%s new commits", n)
	}
	if s := gittest.Git(t, dir, "log", "-1", "--format=%s"); s != "chore(migrations): point p1.py at b2" {
		t.Errorf("subject %q", s)
	}
	if st := gittest.Git(t, dir, "status", "--porcelain"); st != "" {
		t.Errorf("worktree left dirty: %s", st)
	}
}

func TestBumpDownrevNoopWhenCurrent(t *testing.T) {
	t.Parallel()
	dir := rebased(t, [][2]string{{"b1", "down_revision = None"}, {"b2", dr("b1")}}, [][2]string{{"p1", dr("b2")}})
	before := gittest.Git(t, dir, "rev-parse", "HEAD")
	if changed, err := bumpDownrev(context.Background(), dir, "main", migrationGlobs); err != nil || len(changed) != 0 {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if gittest.Git(t, dir, "rev-parse", "HEAD") != before {
		t.Error("committed a no-op")
	}
}

func TestBumpDownrevMultipleBaseHeads(t *testing.T) {
	t.Parallel()
	dir := rebased(t, [][2]string{{"b1", "down_revision = None"}, {"b2a", dr("b1")}, {"b2b", dr("b1")}}, [][2]string{{"p1", dr("b1")}})
	if _, err := bumpDownrev(context.Background(), dir, "main", migrationGlobs); err == nil || !strings.Contains(err.Error(), "heads") {
		t.Fatalf("err = %v", err)
	}
}

func TestBumpDownrevUnparsable(t *testing.T) {
	t.Parallel()
	for _, down := range []string{"down_revision = pick()", `down_revision = "b1; rm -rf ~"`, `down_revision = "../b1"`} {
		dir := rebased(t, [][2]string{{"b1", "down_revision = None"}, {"b2", dr("b1")}}, [][2]string{{"p1", down}})
		if _, err := bumpDownrev(context.Background(), dir, "main", migrationGlobs); err == nil || !strings.Contains(err.Error(), "p1.py") {
			t.Errorf("%s: err = %v", down, err)
		}
	}
}

// The base's head is read from the base: a PR that edits a base migration can't move it.
func TestBumpDownrevBaseHeadFromBaseOnly(t *testing.T) {
	t.Parallel()
	dir := rebased(t, [][2]string{{"b1", "down_revision = None"}, {"b2", dr("b1")}}, [][2]string{{"p1", dr("b1")}})
	os.WriteFile(filepath.Join(dir, versions, "b2.py"), []byte(revisionFile("b2", "down_revision = None")), 0o644)
	gittest.Git(t, dir, "commit", "-qam", "edit b2")
	if changed, err := bumpDownrev(context.Background(), dir, "main", migrationGlobs); err != nil || len(changed) != 1 {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if got := read(t, dir, "p1"); !strings.Contains(got, dr("b2")) {
		t.Errorf("p1:\n%s", got)
	}
}

// downrevFixture is a handoff whose PR adds migrations p1 (down b1) and p2 on a base with b1, opened,
// after which b2 (down b1) landed on main, and, with conflict, a change to a.txt the branch changes too.
// GitHub shows the PR behind (or in conflict) at pre, with checks green or red.
func downrevFixture(t *testing.T, conflict, red bool) (m *Manager, gh *FakeGitHub, id, pre string) {
	t.Helper()
	m, clone, _ := fixture(t)
	gh = m.GH.(*FakeGitHub)
	gh.NextPR, gh.Snapshots = prRef, map[string]github.PRSnapshot{}
	m.runHook = func(context.Context, string, time.Duration, string, map[string]string) error { return nil }
	gittest.Git(t, clone, "checkout", "-q", "main")
	commitMigration(t, clone, "b1", "down_revision = None")
	gittest.Git(t, clone, "push", "-q", "origin", "main")
	gittest.Git(t, clone, "checkout", "-q", "feature/x")
	gittest.Git(t, clone, "rebase", "-q", "main")
	commitMigration(t, clone, "p1", dr("b1"))
	commitMigration(t, clone, "p2", dr("p1"))
	gittest.Git(t, clone, "push", "-qf", "origin", "feature/x")
	req := Request{Worktree: clone, Base: "main"}
	if conflict {
		req.Autonomy, req.MaxFixRounds = "fix-ci", 3
		m.Tasks = &fakeTasks{result: okFix, edit: resolver("a.txt")}
	}
	h, err := m.Handoff(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	pre = gittest.Git(t, clone, "rev-parse", "HEAD")
	advanceMain(t, clone, versions+"/b2.py", revisionFile("b2", dr("b1")))
	p := github.PRSnapshot{State: "OPEN", HeadSHA: pre, BaseRef: "main", Author: "me", MergeState: "BEHIND",
		Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	if conflict {
		advanceMain(t, clone, "a.txt", "three")
		p.MergeState, p.Mergeable = "DIRTY", "CONFLICTING"
	}
	if red {
		p.Checks = []github.Check{chk("a", "COMPLETED", "FAILURE")}
	}
	gh.Snapshots[prRef.Key()] = p
	return m, gh, h.ID, pre
}

// pushWatch notes what origin's feature/x is each time a label goes on or comes off, and can refuse either.
type pushWatch struct {
	*FakeGitHub
	dir                 string
	remote, removedAt   []string
	failAdd, failRemove bool
}

func (p *pushWatch) EditPR(ctx context.Context, ref github.PRRef, e github.PREdit) error {
	origin := func() string {
		out, _ := proc.Run(ctx, p.dir, "git", "ls-remote", "origin", "refs/heads/feature/x")
		return strings.Fields(out + " ")[0]
	}
	if len(e.AddLabels) > 0 {
		p.remote = append(p.remote, origin())
		if p.failAdd {
			return errors.New("boom")
		}
	}
	if len(e.RemoveLabels) > 0 {
		p.removedAt = append(p.removedAt, origin())
		if p.failRemove {
			return errors.New("boom")
		}
	}
	return p.FakeGitHub.EditPR(ctx, ref, e)
}

// labelled runs the restack of a downrevFixture through to a pushed, labelled bump.
func labelled(t *testing.T) (m *Manager, gh *FakeGitHub, w *pushWatch, id, pre string) {
	t.Helper()
	m, gh, id, pre = downrevFixture(t, false, false)
	w = &pushWatch{FakeGitHub: gh, dir: m.must(t, id).Worktree}
	m.GH = w
	m.tickWait(t)
	m.tick(context.Background())
	return m, gh, w, id, pre
}

// labelEdits counts the EditPR calls that added and that removed label.
func labelEdits(gh *FakeGitHub, label string) (added, removed int) {
	for _, e := range gh.Edits {
		if slices.Contains(e.AddLabels, label) {
			added++
		}
		if slices.Contains(e.RemoveLabels, label) {
			removed++
		}
	}
	return added, removed
}

// bumped says the worktree's p1 now hangs off b2, in a commit of its own on top of the rebase.
func bumped(t *testing.T, h Handoff) {
	t.Helper()
	if got := read(t, h.Worktree, "p1"); !strings.Contains(got, dr("b2")) {
		t.Errorf("p1 not bumped:\n%s", got)
	}
	if s := gittest.Git(t, h.Worktree, "log", "-1", "--format=%s"); s != "chore(migrations): point p1.py at b2" {
		t.Errorf("last commit %q", s)
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x") != gittest.Git(t, h.Worktree, "rev-parse", "HEAD") {
		t.Error("bump not pushed")
	}
}

func TestDownrevLabelAddedAndRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, w, id, pre := labelled(t)
	h := m.must(t, id)
	bumped(t, h)
	head := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	if added, _ := labelEdits(gh, "downrev-only"); added != 1 || !slices.Contains(gh.Labels, "downrev-only") || h.DownrevHead != head || h.DownrevFrom != pre {
		t.Fatalf("label added %d, created %v, handoff %+v", added, gh.Labels, h)
	}
	if !slices.Equal(w.remote, []string{pre}) {
		t.Errorf("origin had %v when the label went on, want the head before the push %s", w.remote, pre)
	}
	m.tick(ctx) // GitHub still shows the head from before the push
	if _, removed := labelEdits(gh, "downrev-only"); removed != 0 || m.must(t, id).DownrevHead != head {
		t.Fatal("label removed while GitHub lagged")
	}
	p := gh.Snapshots[prRef.Key()]
	p.HeadSHA, p.MergeState = "f00d", "CLEAN"
	gh.Snapshots[prRef.Key()] = p
	m.tick(ctx)
	if _, removed := labelEdits(gh, "downrev-only"); removed != 1 {
		t.Errorf("label removed %d times", removed)
	}
	if h := m.must(t, id); h.DownrevHead != "" || h.DownrevFrom != "" {
		t.Errorf("downrev kept: %+v", h)
	}

	t.Run("other repo", func(t *testing.T) {
		t.Parallel()
		m, gh, id, _ := downrevFixture(t, false, false)
		m.cfg.LastMile.DownrevLabels = nil
		m.tickWait(t)
		m.tick(ctx)
		h := m.must(t, id)
		bumped(t, h)
		if added, _ := labelEdits(gh, "downrev-only"); added != 0 || h.DownrevHead != "" {
			t.Errorf("labelled: %d, %+v", added, h)
		}
	})
}

func TestDownrevNoLabelAfterConflictOrRed(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ conflict, red bool }{"conflict": {true, false}, "red": {false, true}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, gh, id, _ := downrevFixture(t, c.conflict, c.red)
			m.tickWait(t)
			m.tick(context.Background())
			h := m.must(t, id)
			if h.State == StateNeedsHuman {
				t.Fatalf("escalated: %s", h.Reason)
			}
			bumped(t, h)
			if added, _ := labelEdits(gh, "downrev-only"); added != 0 || h.DownrevHead != "" {
				t.Errorf("labelled: %d, %+v", added, h)
			}
		})
	}
}

func TestDownrevApprovalCarriesOver(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	approvedBefore := func(head string) github.PRSnapshot {
		p := mergeable()
		p.HeadSHA = head
		p.Reviews = []github.SnapReview{{User: "carol", State: "APPROVED", CommitSHA: "old"}}
		return p
	}
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	m.handoffs[0].DownrevFrom, m.handoffs[0].DownrevHead = "old", "h"
	gh.Snapshots[prRef.Key()] = approvedBefore("h")
	m.tick(ctx)
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
		t.Errorf("labelled bump: merged %v, handoff %+v", gh.Merged, m.must(t, id))
	}

	m, gh, _, id = opened(t, Request{Autonomy: "merge=approved"})
	m.handoffs[0].DownrevFrom, m.handoffs[0].DownrevHead = "old", "h"
	gh.Snapshots[prRef.Key()] = approvedBefore("h2") // a new head after the bump: the label comes off, the approval is stale
	m.tick(ctx)
	if h := m.must(t, id); len(gh.Merged) != 0 || h.DownrevHead != "" {
		t.Errorf("new head: merged %v, handoff %+v", gh.Merged, h)
	}
	if _, err := m.Merge(ctx, id); err == nil {
		t.Error("merge button allowed on a stale approval")
	}
}

func TestDownrevLabelFailurePushesWithout(t *testing.T) {
	t.Parallel()
	m, gh, id, _ := downrevFixture(t, false, false)
	m.GH = &pushWatch{FakeGitHub: gh, dir: m.must(t, id).Worktree, failAdd: true}
	m.tickWait(t)
	m.tick(context.Background())
	h := m.must(t, id)
	bumped(t, h)
	if h.State == StateNeedsHuman || h.DownrevHead != "" || h.DownrevFrom != "" || h.DownrevRoot != "" {
		t.Errorf("handoff %+v", h)
	}
}

func TestDownrevLabelOffWhenPushFails(t *testing.T) {
	t.Parallel()
	m, gh, id, pre := downrevFixture(t, false, false)
	h := m.must(t, id)
	other := filepath.Join(t.TempDir(), "other") // someone pushes to the branch: the lease fails
	gittest.Git(t, filepath.Dir(other), "clone", "-q", gittest.Git(t, h.Worktree, "remote", "get-url", "origin"), other)
	gittest.Git(t, other, "checkout", "-q", "feature/x")
	gittest.Commit(t, other, "theirs.txt", "x")
	gittest.Git(t, other, "push", "-q", "origin", "feature/x")
	m.tickWait(t)
	m.tick(context.Background())
	h = m.must(t, id)
	if added, removed := labelEdits(gh, "downrev-only"); added != 1 || removed != 1 {
		t.Errorf("label added %d, removed %d", added, removed)
	}
	if h.State != StateNeedsHuman || h.DownrevHead != "" || gittest.Git(t, h.Worktree, "rev-parse", "HEAD") != pre {
		t.Errorf("handoff %+v", h)
	}
}

func TestDownrevLabelOffOnCancelAndInNeedsHuman(t *testing.T) {
	t.Parallel()
	m, gh, _, id, _ := labelled(t)
	if _, err := m.Cancel(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, removed := labelEdits(gh, "downrev-only"); removed != 1 || m.must(t, id).DownrevHead != "" {
		t.Errorf("cancel: removed %d, %+v", removed, m.must(t, id))
	}

	m, gh, _, id, _ = labelled(t)
	m.handoffs[0].State = StateNeedsHuman
	p := gh.Snapshots[prRef.Key()]
	p.HeadSHA = "f00d" // a person's push while it waits for them
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if _, removed := labelEdits(gh, "downrev-only"); removed != 1 || m.must(t, id).DownrevHead != "" {
		t.Errorf("needs-human: removed %d, %+v", removed, m.must(t, id))
	}
}

func TestDownrevApprovalCarriesThroughConsecutiveBumps(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	m, gh, _, id, pre := labelled(t)
	m.handoffs[0].Autonomy.Merge = MergeApproved
	b := m.must(t, id).DownrevHead
	approved := func(head, state string) {
		p := mergeable()
		p.HeadSHA, p.MergeState, p.NodeID = head, state, ""
		p.Reviews = []github.SnapReview{{User: "carol", State: "APPROVED", CommitSHA: pre}}
		gh.Snapshots[prRef.Key()] = p
	}
	advanceMain(t, m.must(t, id).Worktree, versions+"/b3.py", revisionFile("b3", dr("b2")))
	approved(b, "BEHIND")
	m.tickWait(t)
	m.tick(ctx)
	h := m.must(t, id)
	c := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	if c == b || h.DownrevHead != c || h.DownrevFrom != b || h.DownrevRoot != pre || !strings.Contains(read(t, h.Worktree, "p1"), dr("b3")) {
		t.Fatalf("second bump: %+v", h)
	}
	approved(c, "CLEAN")
	m.tick(ctx)
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
		t.Errorf("approval of %s didn't carry over two bumps: merged %v, handoff %+v", pre, gh.Merged, m.must(t, id))
	}
}

// fixOnLabelledHead has the fix agent commit on top of a labelled bump.
func fixOnLabelledHead(t *testing.T, failRemove bool) (m *Manager, w *pushWatch, id, head string) {
	t.Helper()
	m, gh, w, id, _ := labelled(t)
	w.failRemove = failRemove
	head = m.must(t, id).DownrevHead
	m.handoffs[0].Autonomy.FixCI = true
	m.Tasks = &fakeTasks{result: okFix, edit: func(dir string) { os.WriteFile(filepath.Join(dir, "a.txt"), []byte("fixed"), 0o644) }}
	gh.Logs = map[int64]string{22: "FAILED test_a\n"}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: head, Author: "me", BaseRef: "main", Checks: []github.Check{failCheck("unit", "22")}}
	m.tickWait(t)
	m.tick(context.Background())
	return m, w, id, head
}

func TestDownrevLabelOffBeforeAgentPush(t *testing.T) {
	t.Parallel()
	m, w, id, head := fixOnLabelledHead(t, false)
	h := m.must(t, id)
	local := gittest.Git(t, h.Worktree, "rev-parse", "HEAD")
	if local == head || gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x") != local {
		t.Fatalf("fix not pushed: %+v", h)
	}
	if !slices.Equal(w.removedAt, []string{head}) {
		t.Errorf("origin had %v when the label came off, want the labelled head %s", w.removedAt, head)
	}
	if h.DownrevHead != "" || h.DownrevFrom != "" || h.DownrevRoot != "" {
		t.Errorf("downrev kept: %+v", h)
	}
}

func TestDownrevLabelRemovalFailureBlocksPush(t *testing.T) {
	t.Parallel()
	m, _, id, head := fixOnLabelledHead(t, true)
	h := m.must(t, id)
	if h.State != StateNeedsHuman || !strings.Contains(h.Reason, "couldn't remove the downrev-only label before pushing") {
		t.Fatalf("handoff %s: %s", h.State, h.Reason)
	}
	if gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x") != head || gittest.Git(t, h.Worktree, "rev-parse", "HEAD") != head {
		t.Error("pushed, or the worktree wasn't put back")
	}
}

// A label left on with no record of it (a crash, a failed push) still comes off before TLM pushes.
func TestDownrevLabelOffBeforePushWithoutRecord(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: okFix, edit: func(dir string) { os.WriteFile(filepath.Join(dir, "a.txt"), []byte("fixed"), 0o644) }}
	m, gh, id, head := redFixture(t, tasks)
	w := &pushWatch{FakeGitHub: gh, dir: m.must(t, id).Worktree}
	m.GH = w
	m.tickWait(t)
	m.tick(context.Background())
	h := m.must(t, id)
	if local := gittest.Git(t, h.Worktree, "rev-parse", "HEAD"); local == head || gittest.Git(t, h.Worktree, "rev-parse", "origin/feature/x") != local {
		t.Fatalf("fix not pushed: %+v", h)
	}
	if !slices.Equal(w.removedAt, []string{head}) {
		t.Errorf("origin had %v when the label came off, want the head before the push %s", w.removedAt, head)
	}
}

// Only the directories the PR adds migrations to are read: a base file there that can't be
// parsed doesn't matter when the PR adds no migration beside it.
func TestBumpDownrevIgnoresDirWithoutPRMigrations(t *testing.T) {
	t.Parallel()
	dir := rebased(t, [][2]string{{"b1", "down_revision = None"}, {"b2", "down_revision = pick()"}}, nil)
	os.WriteFile(filepath.Join(dir, versions, "helpers.py"), []byte("def helper():\n    pass\n"), 0o644)
	gittest.Git(t, dir, "add", "-A")
	gittest.Git(t, dir, "commit", "-qm", "helper")
	before := gittest.Git(t, dir, "rev-parse", "HEAD")
	if changed, err := bumpDownrev(context.Background(), dir, "main", migrationGlobs); err != nil || len(changed) != 0 {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	if gittest.Git(t, dir, "rev-parse", "HEAD") != before {
		t.Error("committed")
	}
}
