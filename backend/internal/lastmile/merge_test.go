package lastmile

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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
	p.AutoMergeAllowed, p.ReviewDecision, p.Reviews = true, "", nil
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
