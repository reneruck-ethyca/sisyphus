package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/review"
	"github.com/reneruck/waterboy/internal/testkit"
	"github.com/reneruck/waterboy/internal/tracker"
)

func TestHealth(t *testing.T) {
	for want, tr := range map[string]tracker.Tracked{
		"conflict": {HeadSHA: "b", ConflictHeadSHA: "b", LastError: "conflict"},
		"error":    {HeadSHA: "b", ConflictHeadSHA: "a", LastError: "502"},
		"ok":       {HeadSHA: "b"},
	} {
		if got := health(tr); got != want {
			t.Errorf("health(%+v) = %q, want %q", tr, got, want)
		}
	}
}

func TestServiceSnapshot(t *testing.T) {
	refB := github.PRRef{Repo: "ethyca/b", Number: 2}
	gh := &github.Fake{Cands: []github.PRRef{testkit.RefA, refB}, PRs: map[string]github.PR{
		testkit.RefA.Key(): {PRRef: testkit.RefA, State: "open", HeadSHA: "x", RequestedReviewers: []string{"me"}},
		refB.Key():         {PRRef: refB, State: "open", HeadSHA: "y", RequestedReviewers: []string{"me"}},
	}}
	a := &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x", SyncedSHA: "x"}
	cfg, tr := testkit.Tracker(t, gh,
		&tracker.Tracked{PRRef: refB, HeadSHA: "y", SyncedSHA: "y"},
		a,
	)
	s := NewService(cfg, tr, nil, NewLogBuffer(10))
	var changes int
	s.OnChange(func(Status) { changes++ })

	s.RunOnce(context.Background())

	st := s.Status()
	if st.Running || st.LastRun.IsZero() || st.LastError != "" || changes == 0 {
		t.Fatalf("status = %+v, changes = %d", st, changes)
	}
	if len(st.PRs) != 2 || st.PRs[0].ID != testkit.RefA.Key() || st.PRs[1].ID != refB.Key() {
		t.Fatalf("PRs not sorted by ID: %+v", st.PRs)
	}
	if st.Summary() != "2 tracked" {
		t.Fatalf("summary = %q", st.Summary())
	}
	if pr, ok := s.PR(refB.Key()); !ok || pr.Health != "ok" {
		t.Fatalf("PR(b) = %+v, %v", pr, ok)
	}
	if _, ok := s.PR("ethyca/nope#9"); ok {
		t.Fatal("unknown PR found")
	}

	// Snapshots are copies: mutating tracker state doesn't leak into them.
	a.Title = "changed"
	if s.Status().PRs[0].Title != "" {
		t.Fatal("snapshot aliases tracker state")
	}
}

func TestServiceTickError(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{CandErr: errors.New("offline")})
	s := NewService(cfg, tr, nil, NewLogBuffer(10))
	s.RunOnce(context.Background())
	if st := s.Status(); st.LastError == "" || st.LastRun.IsZero() {
		t.Fatalf("status = %+v", st)
	}
}

func TestServiceRunNow(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{})
	s := NewService(cfg, tr, nil, NewLogBuffer(10))
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

func TestServiceShowsReviewState(t *testing.T) {
	gh := &github.Fake{Cands: []github.PRRef{testkit.RefA}, PRs: map[string]github.PR{
		testkit.RefA.Key(): {PRRef: testkit.RefA, State: "open", HeadSHA: "x", RequestedReviewers: []string{"me"}}}}
	cfg, tr := testkit.Tracker(t, gh, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x", SyncedSHA: "x"})
	rev, err := review.New(config.Review{MaxConcurrent: 1, Repos: map[string]config.RepoReview{testkit.RefA.Repo: {Image: "i"}}}, &container.Fake{}, tr.Locks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewService(cfg, tr, rev, NewLogBuffer(10))
	s.RunOnce(context.Background()) // announces the PR; the reviewer isn't running, so it stays queued
	st := s.Status()
	if !st.Reviews || st.PRs[0].Review.State != "queued" || st.Summary() != "1 tracked · 1 review queued" {
		t.Fatalf("status = %+v, summary %q", st, st.Summary())
	}
}
