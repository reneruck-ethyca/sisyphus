package lastmile

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"

	"github.com/reneruck/sisyphus/internal/github"
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
