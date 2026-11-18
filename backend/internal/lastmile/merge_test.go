package lastmile

import (
	"context"
	"errors"
	"slices"
	"testing"

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
