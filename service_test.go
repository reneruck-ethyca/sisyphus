package main

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestHealth(t *testing.T) {
	for want, tr := range map[string]Tracked{
		"conflict": {HeadSHA: "b", ConflictHeadSHA: "b", LastError: "conflict", PendingUpdates: true},
		"error":    {HeadSHA: "b", ConflictHeadSHA: "a", LastError: "502", PendingUpdates: true},
		"pending":  {HeadSHA: "b", PendingUpdates: true},
		"ok":       {HeadSHA: "b"},
	} {
		if got := health(tr); got != want {
			t.Errorf("health(%+v) = %q, want %q", tr, got, want)
		}
	}
}

func TestServiceSnapshot(t *testing.T) {
	refB := PRRef{"ethyca/b", 2}
	gh := &fakeGitHub{cands: []PRRef{refA, refB}, prs: map[string]PR{
		refA.Key(): {PRRef: refA, State: "open", HeadSHA: "x", RequestedReviewers: []string{"me"}},
		refB.Key(): {PRRef: refB, State: "open", HeadSHA: "y", RequestedReviewers: []string{"me"}},
	}}
	tr := newTestTracker(t, gh,
		&Tracked{PRRef: refB, HeadSHA: "y", SyncedSHA: "y", PendingUpdates: true},
		&Tracked{PRRef: refA, HeadSHA: "x", SyncedSHA: "x"},
	)
	s := NewService(tr.cfg, tr, NewLogBuffer(10))
	var changes int
	s.OnChange(func(Status) { changes++ })

	s.tick(context.Background())

	st := s.Status()
	if st.Running || st.LastRun.IsZero() || st.LastError != "" || changes == 0 {
		t.Fatalf("status = %+v, changes = %d", st, changes)
	}
	if len(st.PRs) != 2 || st.PRs[0].ID != refA.Key() || st.PRs[1].ID != refB.Key() {
		t.Fatalf("PRs not sorted by ID: %+v", st.PRs)
	}
	if st.Summary() != "2 tracked · 1 pending" {
		t.Fatalf("summary = %q", st.Summary())
	}
	if pr, ok := s.PR(refB.Key()); !ok || pr.Health != "pending" {
		t.Fatalf("PR(b) = %+v, %v", pr, ok)
	}
	if _, ok := s.PR("ethyca/nope#9"); ok {
		t.Fatal("unknown PR found")
	}

	// Snapshots are copies: mutating tracker state doesn't leak into them.
	tr.state.PRs[refA.Key()].Title = "changed"
	if s.Status().PRs[0].Title != "" {
		t.Fatal("snapshot aliases tracker state")
	}
}

func TestServiceTickError(t *testing.T) {
	tr := newTestTracker(t, &fakeGitHub{candErr: errors.New("offline")})
	s := NewService(tr.cfg, tr, NewLogBuffer(10))
	s.tick(context.Background())
	if st := s.Status(); st.LastError == "" || st.LastRun.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

func TestServiceRunNow(t *testing.T) {
	tr := newTestTracker(t, &fakeGitHub{})
	tr.cfg.Interval = Duration{time.Hour}
	s := NewService(tr.cfg, tr, NewLogBuffer(10))
	ticks := make(chan struct{}, 10)
	s.OnChange(func(st Status) {
		if !st.Running && !st.LastRun.IsZero() {
			ticks <- struct{}{}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go s.Run(ctx)

	for i := range 2 {
		select {
		case <-ticks:
		case <-time.After(5 * time.Second):
			t.Fatalf("tick %d never happened", i+1)
		}
		s.RunNow()
	}
}
