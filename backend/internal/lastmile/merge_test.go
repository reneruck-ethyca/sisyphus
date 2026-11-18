package lastmile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
)

// mergeable is an open PR, approved by a human on its green head.
func mergeable() github.PRSnapshot {
	return github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", NodeID: "PR_7", ReviewDecision: "APPROVED",
		Checks:  []github.Check{chk("a", "COMPLETED", "SUCCESS")},
		Reviews: []github.SnapReview{{User: "carol", State: "APPROVED", CommitSHA: "h"}}}
}

func TestParseAutonomyMergeModes(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{MergeOff, MergeShadow, MergeAuto, MergeApproved} {
		if a, err := parseAutonomy("fix-ci,merge=" + mode); err != nil || a.Merge != mode {
			t.Errorf("merge=%s: %+v %v", mode, a, err)
		}
	}
	_, err := parseAutonomy("merge=bogus")
	invalidWith(t, err, "merge=bogus")
}

func TestMergeApprovedSquashes(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	gh.Snapshots[prRef.Key()] = mergeable()
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.Merged, []github.PRRef{prRef}) || h.State != StateApproved || h.Error != "" {
		t.Errorf("merged %v, handoff %s %q", gh.Merged, h.State, h.Error)
	}
}

func TestMergeAutoWaitsForEvaluator(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=auto"})
	gh.Snapshots[prRef.Key()] = mergeable()
	gh.InboxData = github.Inbox{Threads: []github.ReviewThread{th("T1", cm(1, "lint[bot]", "nit"))}}
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.Merged) != 0 || h.State != StateApproved || h.Snapshot.Unanswered != 1 {
		t.Fatalf("merged %v, handoff %s unanswered %d", gh.Merged, h.State, h.Snapshot.Unanswered)
	}
	gh.InboxData = github.Inbox{}
	m.tick(context.Background())
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
		t.Errorf("evaluator satisfied: merged %v", gh.Merged)
	}
}

func TestMergeNativeEnablesAutoMerge(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.AutoMerged, []string{"PR_7"}) || len(gh.Merged) != 0 || h.State != StateApproved ||
		!slices.ContainsFunc(h.Events, func(e Event) bool { return e.Kind == "merge" }) {
		t.Fatalf("auto-merged %v merged %v, handoff %s %+v", gh.AutoMerged, gh.Merged, h.State, h.Events)
	}
	p.AutoMerge = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if len(gh.AutoMerged) != 1 || len(gh.Merged) != 0 {
		t.Errorf("auto-merge already on: auto-merged %v merged %v", gh.AutoMerged, gh.Merged)
	}
}

func TestMergeQueueEnqueues(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	p := mergeable()
	p.MergeQueue, p.AutoMergeAllowed = true, true // the button merges itself, native auto-merge or not
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if _, err := m.Merge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.Enqueued, []string{"PR_7"}) || len(gh.Merged) != 0 || len(gh.AutoMerged) != 0 || h.State != StateQueued {
		t.Fatalf("enqueued %v merged %v auto %v, handoff %s", gh.Enqueued, gh.Merged, gh.AutoMerged, h.State)
	}
	p.InQueue = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateQueued || len(gh.Enqueued) != 1 || !h.Snapshot.InQueue {
		t.Errorf("in queue: handoff %s, enqueued %v", h.State, gh.Enqueued)
	}
	_, err := m.Merge(context.Background(), id)
	invalidWith(t, err, "queue")
}

func TestMergeRefusedUnlessApproved(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	_, err := m.Merge(context.Background(), id)
	invalidWith(t, err, "not approved")
	p := mergeable()
	p.ReviewDecision, p.Reviews = "", nil
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateCIGreen {
		t.Fatalf("state %s", h.State)
	}
	_, err = m.Merge(context.Background(), id)
	invalidWith(t, err, "not approved")
	if _, err = m.Merge(context.Background(), "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
}

func TestMergeOffDoesNothing(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	p := mergeable()
	p.AutoMergeAllowed = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.Merged)+len(gh.Enqueued)+len(gh.AutoMerged) != 0 || h.State != StateApproved {
		t.Fatalf("merged %v enqueued %v auto %v, handoff %s", gh.Merged, gh.Enqueued, gh.AutoMerged, h.State)
	}

	_, err := m.SetMerge(id, "bogus")
	invalidWith(t, err, "bogus")
	if h, err := m.SetMerge(id, MergeApproved); err != nil || h.Autonomy.Merge != MergeApproved {
		t.Fatalf("set merge: %+v %v", h.Autonomy, err)
	}
	m.tick(context.Background())
	if !slices.Equal(gh.AutoMerged, []string{"PR_7"}) {
		t.Errorf("after merge=approved: auto-merged %v", gh.AutoMerged)
	}
}

func TestMergeNativeIgnoresEvaluator(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=auto"})
	p := mergeable()
	p.AutoMergeAllowed, p.ReviewDecision, p.Reviews = true, "REVIEW_REQUIRED", nil
	p.Checks = []github.Check{chk("a", "IN_PROGRESS", "")}
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.AutoMerged, []string{"PR_7"}) || !h.AutoMergeSet || h.State != StateCIRunning {
		t.Fatalf("auto-merged %v, handoff %s set %v", gh.AutoMerged, h.State, h.AutoMergeSet)
	}

	// A draft can't have auto-merge.
	m, gh, _, _ = opened(t, Request{Autonomy: "merge=auto", Draft: true})
	p.Draft = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if len(gh.AutoMerged) != 0 {
		t.Errorf("draft: auto-merged %v", gh.AutoMerged)
	}
}

func TestMergeOffDisablesOwnAutoMerge(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	p.AutoMerge = true
	gh.Snapshots[prRef.Key()] = p
	if _, err := m.SetMerge(id, MergeOff); err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) || h.AutoMergeSet {
		t.Fatalf("auto-merge off %v, set %v", gh.AutoMergeOff, h.AutoMergeSet)
	}
	m.tick(context.Background()) // GitHub still shows it on: TLM doesn't own it any more
	if len(gh.AutoMergeOff) != 1 {
		t.Errorf("disabled again: %v", gh.AutoMergeOff)
	}

	// Auto-merge a human turned on is theirs.
	m, gh, _, _ = opened(t, Request{})
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if len(gh.AutoMergeOff)+len(gh.AutoMerged) != 0 {
		t.Errorf("human's auto-merge touched: off %v on %v", gh.AutoMergeOff, gh.AutoMerged)
	}
}

func TestMergeNeedsFreshHumanApproval(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]github.SnapReview{
		"stale": {User: "carol", State: "APPROVED", CommitSHA: "old"},
		"bot":   {User: "lint[bot]", State: "APPROVED", CommitSHA: "h"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
			p := mergeable()
			p.Reviews = []github.SnapReview{r}
			gh.Snapshots[prRef.Key()] = p
			m.tick(context.Background())
			if h := m.must(t, id); len(gh.Merged) != 0 || h.State != StateApproved {
				t.Fatalf("merged %v, handoff %s", gh.Merged, h.State)
			}
			_, err := m.Merge(context.Background(), id)
			invalidWith(t, err, "not approved")
		})
	}
}

func TestSetMergeOffClearsRequest(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	gh.Snapshots[prRef.Key()] = mergeable()
	m.tick(context.Background())
	if h, err := m.Merge(context.Background(), id); err != nil || !h.MergeRequested {
		t.Fatalf("merge: %v %v", h.MergeRequested, err)
	}
	if h, err := m.SetMerge(id, MergeShadow); err != nil || h.MergeRequested {
		t.Fatalf("shadow: requested %v %v", h.MergeRequested, err)
	}
	m.tick(context.Background())
	if len(gh.Merged) != 0 {
		t.Errorf("merged %v", gh.Merged)
	}
}

func TestMergeFreezeBlocksModeNotButton(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	m.cfg.LastMile.MergeFreeze = []config.FreezeWindow{{Start: "11:00", End: "13:00"}}
	m.now = func() time.Time { return time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC) }
	gh.Snapshots[prRef.Key()] = mergeable()
	m.tick(context.Background())
	if len(gh.Merged) != 0 {
		t.Fatalf("frozen: merged %v", gh.Merged)
	}
	if _, err := m.Merge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
		t.Errorf("button in a freeze: merged %v", gh.Merged)
	}
}

func TestQueuedKeepsWorkStates(t *testing.T) {
	t.Parallel()
	m := &Manager{}
	h := Handoff{}
	for _, st := range []string{StateCIRed, StateFeedback, StateConflict} {
		if got, _, _ := m.mergeStep(context.Background(), &h, Snapshot{InQueue: true}, st, "x"); got != st {
			t.Errorf("%s in queue = %s", st, got)
		}
	}
	if got, _, _ := m.mergeStep(context.Background(), &h, Snapshot{InQueue: true}, StateCIRunning, ""); got != StateQueued {
		t.Errorf("ci-running in queue = %s", got)
	}
}

func TestMergeNativeCleanMergesItself(t *testing.T) {
	t.Parallel()
	m, gh, _, _ := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed, p.MergeState = true, "CLEAN"
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) || len(gh.AutoMerged) != 0 {
		t.Errorf("CLEAN: merged %v auto-merged %v", gh.Merged, gh.AutoMerged)
	}

	// GitHub's refusal says the same: TLM merges.
	m, gh, _, _ = opened(t, Request{Autonomy: "merge=approved"})
	p.MergeState = "BLOCKED"
	gh.Snapshots[prRef.Key()] = p
	gh.AutoMergeErr = errors.New(`enablePullRequestAutoMerge: Pull request is in clean status`)
	m.tick(context.Background())
	if !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
		t.Errorf("clean-status error: merged %v", gh.Merged)
	}
}

func TestGivingUpDisarmsAutoMerge(t *testing.T) {
	t.Parallel()
	armed := func(t *testing.T) (*Manager, *FakeGitHub, string, github.PRSnapshot) {
		m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
		p := mergeable()
		p.AutoMergeAllowed = true
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		if h := m.must(t, id); !h.AutoMergeSet {
			t.Fatal("auto-merge not armed")
		}
		return m, gh, id, p
	}

	m, gh, id, p := armed(t)
	p.AutoMerge, p.Checks = true, []github.Check{chk("a", "COMPLETED", "FAILURE")}
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.AutoMergeSet || !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("escalated: %s set %v off %v", h.State, h.AutoMergeSet, gh.AutoMergeOff)
	}

	m, gh, id, _ = armed(t)
	h, err := m.Cancel(context.Background(), id)
	if err != nil || h.AutoMergeSet || !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("cancelled: %v set %v off %v", err, h.AutoMergeSet, gh.AutoMergeOff)
	}
}

// queued is a merge-queue handoff whose PR, approved on the worktree's head, TLM
// has put in the queue; it returns the PR as GitHub shows it.
func queued(t *testing.T, req Request, tasks *fakeTasks) (*Manager, *FakeGitHub, string, github.PRSnapshot) {
	t.Helper()
	m, gh, _, id := opened(t, req)
	if tasks != nil {
		m.Tasks = tasks
	}
	head := gittest.Git(t, m.must(t, id).Worktree, "rev-parse", "HEAD")
	p := mergeable()
	p.HeadSHA, p.Reviews[0].CommitSHA, p.BaseRef, p.MergeQueue, p.Mergeable = head, head, "main", true, "MERGEABLE"
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if _, err := m.Merge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if len(gh.Enqueued) != 1 {
		t.Fatalf("not enqueued: %v", gh.Enqueued)
	}
	return m, gh, id, p
}

// removed is p after GitHub took it out of the queue.
func removed(gh *FakeGitHub, p github.PRSnapshot, rs ...github.QueueRemoval) {
	p.QueueRemovals = append(p.QueueRemovals, rs...)
	gh.Snapshots[prRef.Key()] = p
}

func TestQueueEnqueueGrace(t *testing.T) {
	t.Parallel()
	m, gh, id, _ := queued(t, Request{}, nil)
	m.tick(context.Background()) // GitHub doesn't show it in the queue yet
	if h := m.must(t, id); len(gh.Enqueued) != 1 || h.State != StateQueued {
		t.Errorf("enqueued %v, handoff %s", gh.Enqueued, h.State)
	}
}

func TestQueueRemovalCountedOnce(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	now := time.Now()
	removed(gh, p, github.QueueRemoval{At: now, Reason: "Merge conflict", Actor: "github-merge-queue[bot]"},
		github.QueueRemoval{At: now.Add(time.Second), Reason: "merge conflict", Actor: "github-merge-queue[bot]", ActorBot: true})
	m.tick(context.Background())
	m.tick(context.Background())
	if h := m.must(t, id); h.QueueKicks != 2 || !h.QueueSeen.Equal(now.Add(time.Second)) {
		t.Errorf("kicks %d seen %v", h.QueueKicks, h.QueueSeen)
	}
}

func TestQueueTenthRemovalEscalates(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	m.mu.Lock()
	m.handoffs[0].QueueKicks = 9
	m.mu.Unlock()
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "merge conflict"})
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Reason != "removed from the merge queue 10 times" || h.QueueKicks != 10 {
		t.Errorf("handoff %s %q kicks %d", h.State, h.Reason, h.QueueKicks)
	}
}

func TestQueueFlakeRequeues(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	m.cfg.LastMile.Flakes = []string{`ECONNRESET`}
	gh.Logs = map[int64]string{22: "npm ERR! ECONNRESET while fetching\n"}
	gh.QueueChecks = []github.Check{failCheck("unit", "22")}
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "checks failed"})
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.Enqueued) != 2 || h.State != StateQueued || h.QueueKicks != 1 || len(gh.Reruns) != 0 || h.Run != nil {
		t.Errorf("enqueued %v reruns %v, handoff %s kicks %d run %v", gh.Enqueued, gh.Reruns, h.State, h.QueueKicks, h.Run)
	}
}

func TestQueueCIFailureRunsFix(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: okFix}
	m, gh, id, p := queued(t, Request{Autonomy: "fix-ci"}, tasks)
	gh.Logs = map[int64]string{22: "FAILED test_merge_group - AssertionError\n"}
	gh.QueueChecks = []github.Check{failCheck("unit", "22")}
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "checks failed"})
	m.tickWait(t)
	h := m.must(t, id)
	if tasks.n() != 1 || h.State != StateCIRed || h.FixRounds[TriggerCI] != 1 || h.Run == nil || h.Run.Trigger != TriggerCI {
		t.Fatalf("agent runs %d, handoff %s %q rounds %v run %+v", tasks.n(), h.State, h.Reason, h.FixRounds, h.Run)
	}
	if !strings.Contains(tasks.calls[0].Prompt, "test_merge_group") {
		t.Errorf("prompt lacks the merge-group log")
	}
	if len(gh.Enqueued) != 1 {
		t.Errorf("re-queued with CI failing: %v", gh.Enqueued)
	}

	m, gh, id, p = queued(t, Request{}, nil)
	gh.QueueChecks = []github.Check{failCheck("unit", "22")}
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "checks failed"})
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Reason != "merge queue CI failed: unit" {
		t.Errorf("without fix-ci: %s %q", h.State, h.Reason)
	}
}

func TestQueueUnknownCauseRunsQueueAgent(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: `{"action":"escalate","summary":"the queue's runner pool is gone"}`}
	m, gh, id, p := queued(t, Request{}, tasks)
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "something odd"})
	m.tickWait(t)
	h := m.must(t, id)
	if tasks.n() != 1 || h.State != StateQueued || h.Reason != "investigating removal from the merge queue" || h.Run == nil || h.Run.Trigger != TriggerQueue {
		t.Fatalf("agent runs %d, handoff %s %q run %+v", tasks.n(), h.State, h.Reason, h.Run)
	}
	if c := tasks.calls[0]; !strings.Contains(c.Prompt, "something odd") || c.Context["snapshot.json"] == "" {
		t.Errorf("task = %+v", c)
	}
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || !strings.Contains(h.Reason, "the queue's runner pool is gone") || len(gh.Enqueued) != 1 {
		t.Errorf("escalate: %s %q enqueued %v", h.State, h.Reason, gh.Enqueued)
	}

	tasks = &fakeTasks{result: `{"action":"requeue","summary":"a one-off"}`}
	m, gh, id, p = queued(t, Request{}, tasks)
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "something odd"})
	m.tickWait(t)
	m.tick(context.Background()) // applies the outcome
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateQueued || len(gh.Enqueued) != 2 {
		t.Errorf("requeue: %s %q enqueued %v", h.State, h.Reason, gh.Enqueued)
	}
}

func TestManualDequeueStopsMerging(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{Autonomy: "merge=approved"}, nil)
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "manually removed", Actor: "carol"})
	m.tick(context.Background())
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeOff || h.MergeRequested || h.QueueKicks != 0 || len(gh.Enqueued) != 1 {
		t.Errorf("merge %s requested %v kicks %d enqueued %v", h.Autonomy.Merge, h.MergeRequested, h.QueueKicks, gh.Enqueued)
	}
}

func TestQueueResumeResetsCap(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	m.mu.Lock()
	m.handoffs[0].QueueKicks = 9
	m.mu.Unlock()
	first := github.QueueRemoval{At: time.Now(), Reason: "merge conflict"}
	removed(gh, p, first)
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.EscalatedBy != TriggerQueue {
		t.Fatalf("not escalated: %s by %q", h.State, h.EscalatedBy)
	}
	p.QueueRemovals = []github.QueueRemoval{first, {At: first.At.Add(time.Second), Reason: "merge conflict"}} // while it waited for a human
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if _, err := m.Resume(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if h := m.must(t, id); h.QueueKicks != 0 || !h.QueueSeen.Equal(first.At.Add(time.Second)) {
		t.Fatalf("after resume: kicks %d seen %v", h.QueueKicks, h.QueueSeen)
	}
	removed(gh, p, github.QueueRemoval{At: time.Now().Add(time.Minute), Reason: "merge conflict"})
	m.tick(context.Background())
	if h := m.must(t, id); h.State == StateNeedsHuman || h.QueueKicks != 1 {
		t.Errorf("after resume: %s %q kicks %d", h.State, h.Reason, h.QueueKicks)
	}
}

func TestQueueRemovalBeforeHandoffIgnored(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	removed(gh, p, github.QueueRemoval{At: m.must(t, id).PROpened.Add(-time.Second), Reason: "something odd"})
	m.tick(context.Background())
	if h := m.must(t, id); h.QueueKicks != 0 || h.State != StateQueued || h.Run != nil {
		t.Errorf("kicks %d, handoff %s run %+v", h.QueueKicks, h.State, h.Run)
	}
}

func TestQueueRemovalWithoutMergeIntent(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: `{"action":"escalate","summary":"x"}`}
	m, gh, _, id := opened(t, Request{})
	m.Tasks = tasks
	p := mergeable()
	p.MergeQueue, p.BaseRef = true, "main"
	at := time.Now()
	p.QueueRemovals = []github.QueueRemoval{{At: at, Reason: "something odd"}} // a person queued it, GitHub took it out
	gh.Snapshots[prRef.Key()] = p
	m.tickWait(t)
	if h := m.must(t, id); h.QueueKicks != 0 || !h.QueueSeen.Equal(at) || tasks.n() != 0 || h.State == StateNeedsHuman || len(gh.Enqueued) != 0 {
		t.Errorf("kicks %d seen %v agent runs %d, handoff %s, enqueued %v", h.QueueKicks, h.QueueSeen, tasks.n(), h.State, gh.Enqueued)
	}
}

func TestQueueLostRunLogged(t *testing.T) {
	t.Parallel()
	m, _, id, _ := queued(t, Request{}, nil)
	h := m.must(t, id)
	h.Run = &RunMark{Trigger: TriggerQueue, Started: time.Now(), Head: gittest.Git(t, h.Worktree, "rev-parse", "HEAD")}
	m.lostRun(context.Background(), &h)
	if !slices.ContainsFunc(h.Events, func(e Event) bool { return strings.Contains(e.Text, "queue investigation lost to a restart") }) || h.State == StateNeedsHuman {
		t.Errorf("handoff %s, events %+v", h.State, h.Events)
	}
}

// mergedHandoff is a merged handoff whose worktree is a linked worktree of the
// fixture's clone, on feature/y tracking origin/feature/x, at the PR's head.
func mergedHandoff(t *testing.T) (*Manager, Handoff, *[][2]string) {
	t.Helper()
	m, clone, _ := fixture(t)
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Git(t, clone, "worktree", "add", "-q", "--track", "-b", "feature/y", wt, "origin/feature/x")
	var calls [][2]string
	m.removeWT = func(_ context.Context, dir, branch string) error {
		calls = append(calls, [2]string{dir, branch})
		return nil
	}
	h := Handoff{ID: "h1", Worktree: wt, Branch: "feature/y", State: StateMerged,
		Snapshot: &Snapshot{Head: gittest.Git(t, wt, "rev-parse", "HEAD")}}
	m.handoffs = []Handoff{h}
	return m, h, &calls
}

func TestCleanupRemovesMergedWorktree(t *testing.T) {
	t.Parallel()
	m, h, calls := mergedHandoff(t)
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	common := gittest.Git(t, h.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir")
	want := [][2]string{{filepath.Dir(common), "feature/y"}}
	if !slices.Equal(*calls, want) {
		t.Errorf("remover calls %v, want %v", *calls, want)
	}
	if got := m.must(t, "h1"); got.Warning != "" {
		t.Errorf("warning %q", got.Warning)
	}

	// The head moved since the last snapshot, but upstream has it: nothing is lost.
	m, h, calls = mergedHandoff(t)
	h.Snapshot = &Snapshot{Head: "older"}
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 1 {
		t.Errorf("pushed head: remover calls %v", *calls)
	}
}

func TestCleanupKeepsUnpushedCommits(t *testing.T) {
	t.Parallel()
	m, h, calls := mergedHandoff(t)
	gittest.Commit(t, h.Worktree, "late.txt", "after the merge")
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 0 {
		t.Errorf("unpushed commit removed: %v", *calls)
	}
	if got := m.must(t, "h1"); got.Warning != "worktree kept: it has unpushed commits" {
		t.Errorf("warning %q", got.Warning)
	}
}

func TestCleanupKeepsMainCheckout(t *testing.T) {
	t.Parallel()
	m, h, calls := mergedHandoff(t)
	h.Worktree = filepath.Dir(gittest.Git(t, h.Worktree, "rev-parse", "--path-format=absolute", "--git-common-dir"))
	h.Branch = "feature/x"
	h.Snapshot = &Snapshot{Head: gittest.Git(t, h.Worktree, "rev-parse", "HEAD")}
	m.handoffs = []Handoff{h}
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 0 {
		t.Errorf("main checkout removed: %v", *calls)
	}
	if got := m.must(t, "h1"); got.Warning != "worktree kept: not a linked worktree" {
		t.Errorf("warning %q", got.Warning)
	}
}

func TestCleanupKeepsDirtyWorktree(t *testing.T) {
	t.Parallel()
	m, h, calls := mergedHandoff(t)
	if err := os.WriteFile(filepath.Join(h.Worktree, "wtp.env"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	h.Untracked = []string{"wtp.env"}
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 1 {
		t.Fatalf("untracked-only worktree: remover calls %v", *calls)
	}

	*calls = nil
	if err := os.WriteFile(filepath.Join(h.Worktree, "a.txt"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 0 {
		t.Errorf("dirty worktree removed: %v", *calls)
	}
	if got := m.must(t, "h1"); got.Warning != "worktree kept: it has uncommitted changes" {
		t.Errorf("warning %q", got.Warning)
	}
}

func TestCleanupFailureWarns(t *testing.T) {
	t.Parallel()
	m, h, _ := mergedHandoff(t)
	m.removeWT = func(context.Context, string, string) error { return errors.New("boom") }
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if got := m.must(t, "h1"); !strings.Contains(got.Warning, "boom") {
		t.Errorf("warning %q", got.Warning)
	}
}

func hasEvent(h Handoff, text string) bool {
	return slices.ContainsFunc(h.Events, func(e Event) bool { return strings.Contains(e.Text, text) })
}

func TestAutoMergeTurnedOffOnGitHub(t *testing.T) {
	t.Parallel()
	armed := func(t *testing.T) (*Manager, *FakeGitHub, string, github.PRSnapshot) {
		m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
		p := mergeable()
		p.AutoMergeAllowed = true
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		p.AutoMerge = true
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		p.AutoMerge = false
		m.now = func() time.Time { return time.Now().Add(3 * time.Minute) } // past the grace after enabling
		return m, gh, id, p
	}

	m, gh, id, p := armed(t) // a person turned it off: same head, no removal
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeOff || h.AutoMergeSet || h.MergeRequested || len(gh.AutoMerged) != 1 || len(gh.Merged) != 0 ||
		!hasEvent(h, "auto-merge turned off on GitHub; merge mode set to off") {
		t.Errorf("turned off: merge %s set %v auto-merged %v merged %v events %+v", h.Autonomy.Merge, h.AutoMergeSet, gh.AutoMerged, gh.Merged, h.Events)
	}

	m, gh, id, p = armed(t) // a push: GitHub dropped it
	p.HeadSHA = "h2"
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeApproved || !h.AutoMergeSet || len(gh.AutoMerged) != 2 || h.AutoMergeHead != "h2" {
		t.Errorf("head moved: merge %s set %v head %q auto-merged %v", h.Autonomy.Merge, h.AutoMergeSet, h.AutoMergeHead, gh.AutoMerged)
	}

	m, gh, id, p = armed(t) // a removal from the merge queue: GitHub dropped it
	m.set(t, id, func(h *Handoff) { h.QueueKicks++ })
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeApproved || len(gh.AutoMerged) != 2 {
		t.Errorf("removed: merge %s auto-merged %v", h.Autonomy.Merge, gh.AutoMerged)
	}
}

func TestStaleTickReleasesWhatItArmed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ticked := func(t *testing.T) (*Manager, *FakeGitHub, string, Handoff, Handoff) {
		m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
		before := m.must(t, id)
		after := before
		after.Snapshot = &Snapshot{NodeID: "PR_7", Head: "h", AutoMerge: true}
		return m, gh, id, before, after
	}

	m, gh, id, before, after := ticked(t) // merge set off while the tick turned auto-merge on
	after.AutoMergeSet, after.AutoMergeHead = true, "h"
	if _, err := m.SetMerge(id, MergeOff); err != nil {
		t.Fatal(err)
	}
	if m.commit(ctx, before, after) {
		t.Fatal("stale tick stored")
	}
	if h := m.must(t, id); h.AutoMergeSet || !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("merge off: set %v off %v", h.AutoMergeSet, gh.AutoMergeOff)
	}

	m, gh, id, before, after = ticked(t) // cancelled while the tick queued it
	after.EnqueuedAt, after.EnqueuedHead = time.Now(), "h"
	if _, err := m.Cancel(ctx, id); err != nil {
		t.Fatal(err)
	}
	m.commit(ctx, before, after)
	if h := m.must(t, id); !h.EnqueuedAt.IsZero() || !slices.Equal(gh.Dequeued, []string{"PR_7"}) {
		t.Errorf("cancelled: enqueued %v dequeued %v", h.EnqueuedAt, gh.Dequeued)
	}

	m, gh, id, before, after = ticked(t) // an unrelated edit: TLM still merges, and remembers what it armed
	after.AutoMergeSet, after.AutoMergeHead, after.EnqueuedAt = true, "h", time.Now()
	if _, err := m.SetReviewers(id, []string{"alice"}); err != nil {
		t.Fatal(err)
	}
	m.commit(ctx, before, after)
	if h := m.must(t, id); !h.AutoMergeSet || h.AutoMergeHead != "h" || h.EnqueuedAt.IsZero() || len(gh.AutoMergeOff)+len(gh.Dequeued) != 0 {
		t.Errorf("kept: set %v head %q enqueued %v off %v dequeued %v", h.AutoMergeSet, h.AutoMergeHead, h.EnqueuedAt, gh.AutoMergeOff, gh.Dequeued)
	}
}

func TestLettingGoDequeues(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	inQueue := func(t *testing.T) (*Manager, *FakeGitHub, string, github.PRSnapshot) {
		m, gh, id, p := queued(t, Request{}, nil)
		p.InQueue = true
		gh.Snapshots[prRef.Key()] = p
		m.tick(ctx)
		return m, gh, id, p
	}

	m, gh, id, _ := inQueue(t)
	if _, err := m.Cancel(ctx, id); err != nil || !slices.Equal(gh.Dequeued, []string{"PR_7"}) {
		t.Errorf("cancel: %v dequeued %v", err, gh.Dequeued)
	}

	m, gh, id, _ = inQueue(t)
	h := m.must(t, id)
	m.escalate(ctx, &h, "x")
	if !slices.Equal(gh.Dequeued, []string{"PR_7"}) || !h.EnqueuedAt.IsZero() {
		t.Errorf("escalate: dequeued %v enqueued %v", gh.Dequeued, h.EnqueuedAt)
	}

	m, gh, id, p := inQueue(t)
	if _, err := m.SetMerge(id, MergeOff); err != nil {
		t.Fatal(err)
	}
	m.tick(ctx)
	if h := m.must(t, id); !slices.Equal(gh.Dequeued, []string{"PR_7"}) || !h.EnqueuedAt.IsZero() || h.DequeuedAt.IsZero() {
		t.Fatalf("merge off: dequeued %v enqueued %v dequeued at %v", gh.Dequeued, h.EnqueuedAt, h.DequeuedAt)
	}
	// GitHub credits TLM's dequeue to the user's login: not a person's dequeue.
	if _, err := m.SetMerge(id, MergeApproved); err != nil {
		t.Fatal(err)
	}
	p.InQueue = false
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "manually removed", Actor: "me"})
	m.tick(ctx)
	if h := m.must(t, id); h.Autonomy.Merge != MergeApproved || h.QueueKicks != 0 {
		t.Errorf("own dequeue: merge %s kicks %d", h.Autonomy.Merge, h.QueueKicks)
	}
}

func TestQueueRemovalWithCauseIsKickOut(t *testing.T) {
	t.Parallel()
	m, gh, id, p := queued(t, Request{}, nil)
	p.Mergeable = "CONFLICTING" // TLM queues as the user: their login on the removal says nothing
	removed(gh, p, github.QueueRemoval{At: time.Now(), Reason: "manually removed", Actor: "me"})
	m.tick(context.Background())
	if h := m.must(t, id); h.QueueKicks != 1 || !h.MergeRequested {
		t.Errorf("kicks %d requested %v", h.QueueKicks, h.MergeRequested)
	}
}

func TestQueueRemovalAfterPushIgnored(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: `{"action":"escalate","summary":"x"}`}
	m, gh, id, p := queued(t, Request{}, tasks)
	p.HeadSHA = "pushed"
	at := time.Now()
	removed(gh, p, github.QueueRemoval{At: at, Reason: "something odd"})
	m.tickWait(t)
	if h := m.must(t, id); h.QueueKicks != 0 || !h.QueueSeen.Equal(at) || tasks.n() != 0 || h.State == StateNeedsHuman {
		t.Errorf("kicks %d seen %v agent runs %d, handoff %s", h.QueueKicks, h.QueueSeen, tasks.n(), h.State)
	}
}

func TestMergeRefusedByGitHubEscalates(t *testing.T) {
	t.Parallel()
	refuse := func(t *testing.T, p github.PRSnapshot, err error) (*Manager, *FakeGitHub, string) {
		m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
		gh.MergeErr, gh.AutoMergeErr = err, err
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		return m, gh, id
	}
	policy := &api.HTTPError{StatusCode: 405, Message: "Base branch policy prohibits the merge"}

	m, gh, id := refuse(t, mergeable(), policy)
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || !strings.HasPrefix(h.Reason, "GitHub refused the merge: ") ||
		!strings.Contains(h.Reason, "prohibits") || h.Error != "" || len(gh.Merged) != 1 || !hasEvent(h, "GitHub refused the merge") {
		t.Errorf("squash: %s %q error %q merged %v", h.State, h.Reason, h.Error, gh.Merged)
	}

	p := mergeable()
	p.MergeQueue, p.BaseRef = true, "main"
	m, _, id = refuse(t, p, policy)
	if h := m.must(t, id); h.State != StateNeedsHuman {
		t.Errorf("enqueue: %s %q", h.State, h.Reason)
	}

	p = mergeable()
	p.AutoMergeAllowed = true
	m, _, id = refuse(t, p, errors.New("enablePullRequestAutoMerge: Pull request is not mergeable"))
	if h := m.must(t, id); h.State != StateNeedsHuman || h.AutoMergeSet {
		t.Errorf("auto-merge: %s %q set %v", h.State, h.Reason, h.AutoMergeSet)
	}

	// A head that moved under the merge is tried again on the next tick.
	m, _, id = refuse(t, mergeable(), &api.HTTPError{StatusCode: 409, Message: "Head branch was modified"})
	if h := m.must(t, id); h.State != StateApproved || h.Error != "" {
		t.Errorf("409: %s %q error %q", h.State, h.Reason, h.Error)
	}
}

func TestMergeNativeCleanStatesMergeItself(t *testing.T) {
	t.Parallel()
	for _, st := range []string{"UNSTABLE", "HAS_HOOKS"} {
		m, gh, _, _ := opened(t, Request{Autonomy: "merge=approved"})
		p := mergeable()
		p.AutoMergeAllowed, p.MergeState = true, st
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		if !slices.Equal(gh.Merged, []github.PRRef{prRef}) || len(gh.AutoMerged) != 0 {
			t.Errorf("%s: merged %v auto-merged %v", st, gh.Merged, gh.AutoMerged)
		}
	}
}

func TestMergeNativeOnlyWhereGitHubEnforcesReviews(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{MergeApproved, MergeAuto} {
		m, gh, _, _ := opened(t, Request{Autonomy: "merge=" + mode})
		p := mergeable()
		p.AutoMergeAllowed, p.ReviewDecision = true, "" // no review required: GitHub would merge unapproved
		p.Reviews = nil
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		if len(gh.AutoMerged)+len(gh.Merged) != 0 {
			t.Errorf("%s unapproved: auto-merged %v merged %v", mode, gh.AutoMerged, gh.Merged)
		}
		p.Reviews = mergeable().Reviews
		gh.Snapshots[prRef.Key()] = p
		m.tick(context.Background())
		if len(gh.AutoMerged) != 0 || !slices.Equal(gh.Merged, []github.PRRef{prRef}) {
			t.Errorf("%s approved: auto-merged %v merged %v", mode, gh.AutoMerged, gh.Merged)
		}
	}
}

func TestAutoMergeGraceAfterEnabling(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed = true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	m.tick(context.Background()) // GitHub doesn't show it on yet
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeApproved || !h.AutoMergeSet || len(gh.AutoMerged) != 1 {
		t.Fatalf("in the grace: merge %s set %v auto-merged %v", h.Autonomy.Merge, h.AutoMergeSet, gh.AutoMerged)
	}
	m.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeOff || h.AutoMergeSet || len(gh.AutoMerged) != 1 {
		t.Errorf("after the grace: merge %s set %v auto-merged %v", h.Autonomy.Merge, h.AutoMergeSet, gh.AutoMerged)
	}
}

func TestLettingGoInsideAutoMergeGrace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	lagging := func(t *testing.T) (*Manager, *FakeGitHub, string) {
		m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
		p := mergeable()
		p.AutoMergeAllowed = true
		gh.Snapshots[prRef.Key()] = p
		m.tick(ctx)
		m.tick(ctx) // GitHub doesn't show it on yet
		return m, gh, id
	}

	m, gh, id := lagging(t)
	if _, err := m.SetMerge(id, MergeOff); err != nil {
		t.Fatal(err)
	}
	m.tick(ctx)
	if !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("merge off: auto-merge off %v", gh.AutoMergeOff)
	}

	m, gh, id = lagging(t)
	if _, err := m.Cancel(ctx, id); err != nil || !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("cancel: %v auto-merge off %v", err, gh.AutoMergeOff)
	}

	m, gh, id = lagging(t)
	h := m.must(t, id)
	m.escalate(ctx, &h, "x")
	if !slices.Equal(gh.AutoMergeOff, []string{"PR_7"}) {
		t.Errorf("escalate: auto-merge off %v", gh.AutoMergeOff)
	}
}

func TestAutoMergeOffInQueueIsGitHubs(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "merge=approved"})
	p := mergeable()
	p.AutoMergeAllowed, p.MergeQueue = true, true
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	m.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	p.InQueue = true // GitHub may clear the auto-merge request as it queues the PR
	gh.Snapshots[prRef.Key()] = p
	m.tick(context.Background())
	if h := m.must(t, id); h.Autonomy.Merge != MergeApproved || !h.AutoMergeSet || len(gh.AutoMerged) != 1 || h.State != StateQueued {
		t.Errorf("merge %s set %v auto-merged %v, handoff %s", h.Autonomy.Merge, h.AutoMergeSet, gh.AutoMerged, h.State)
	}
}

func TestCleanupKeepsWorktreeOnAnotherBranch(t *testing.T) {
	t.Parallel()
	m, h, calls := mergedHandoff(t)
	h.Branch = "feature/other"
	m.handoffs = []Handoff{h}
	m.afterCommit(context.Background(), Handoff{ID: "h1", State: StatePROpen}, h)
	if len(*calls) != 0 {
		t.Errorf("removed: %v", *calls)
	}
	if got := m.must(t, "h1"); got.Warning != "worktree kept: it is not on feature/other" {
		t.Errorf("warning %q", got.Warning)
	}
}

func TestMergeAndModeAreLogged(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	gh.Snapshots[prRef.Key()] = mergeable()
	m.tick(context.Background())
	if _, err := m.Merge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SetMerge(id, MergeAuto); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range m.must(t, id).Events {
		if e.Kind == "merge" {
			got = append(got, e.Text)
		}
	}
	if want := []string{"merge requested from the dashboard", "merge mode set to auto"}; !slices.Equal(got, want) {
		t.Errorf("merge events %q, want %q", got, want)
	}
}
