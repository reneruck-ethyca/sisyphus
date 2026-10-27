package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/testkit"
	"github.com/reneruck/sisyphus/internal/tracker"
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

func TestReviewNote(t *testing.T) {
	tr := tracker.Tracked{HeadSHA: "4f0b24f88e35"}
	done := func(status, head, err string) review.PRStatus {
		return review.PRStatus{Runs: []review.Run{{Status: status, Error: err, Job: review.Job{Reason: "new", PR: tracker.Tracked{HeadSHA: head}},
			Finished: time.Date(2026, 10, 23, 12, 0, 0, 0, time.Local)}}}
	}
	running := done(review.Running, "4f0b24f88e35", "")
	running.State = "running"
	queued := review.PRStatus{State: "queued"}
	retrying := done(review.Failed, "4f0b24f88e35", "exited 1 before Claude finished")
	retrying.State, retrying.Queued = "queued", &review.Job{Attempt: 2, NotBefore: time.Now().Add(time.Hour)}
	at := retrying.Queued.NotBefore.Local().Format("15:04")
	for _, c := range []struct {
		name             string
		rs               review.PRStatus
		reviews          bool
		blocked, runtime string
		want             string
	}{
		{"disabled", review.PRStatus{}, false, "", "", "reviews are disabled"},
		{"running", running, true, "", "", "reviewing (new)"},
		{"running, history just reset", review.PRStatus{State: "running"}, true, "", "", "stopping a review"},
		{"queued", queued, true, "", "", "queued"},
		{"retry waiting", retrying, true, "", "", "queued: retry 2 at " + at + " after: exited 1 before Claude finished"},
		{"queued, blocked", queued, true, "env file missing: /x", "", "queued, waiting: env file missing: /x"},
		{"queued, no runtime", queued, true, "", "docker down", "queued, waiting for the container runtime"},
		{"never, blocked", review.PRStatus{}, true, "no review.repos entry for o/r", "", "not reviewed: no review.repos entry for o/r"},
		{"never", review.PRStatus{}, true, "", "", "not reviewed yet: waits for the head to be synced"},
		{"failed", done(review.Failed, "4f0b24f88e35", "boom"), true, "", "", "last review failed: boom; next one on new commits or replies"},
		{"old head", done(review.Succeeded, "aaaaaaaaaaaa", ""), true, "", "", "reviewed aaaaaaaa; head 4f0b24f8 not synced yet"},
		{"up to date", done(review.Succeeded, "4f0b24f88e35", ""), true, "", "", "up to date: reviewed 4f0b24f8 at Oct 23 12:00, not posted; next one on new commits or replies"},
	} {
		if got := reviewNote(tr, c.rs, c.reviews, c.blocked, c.runtime); got != c.want {
			t.Errorf("%s: got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// Actions run on the loop between ticks; with the loop running they still finish.
func TestUntrackAndRetrackOnTheLoop(t *testing.T) {
	gh := &github.Fake{Cands: []github.PRRef{testkit.RefA}, PRs: map[string]github.PR{
		testkit.RefA.Key(): {PRRef: testkit.RefA, State: "open", HeadSHA: "x", RequestedReviewers: []string{"me"}}}}
	cfg, tr := testkit.Tracker(t, gh, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x", SyncedSHA: "x"})
	s := NewService(cfg, tr, nil, NewLogBuffer(10)) // reviews disabled: untracking still works
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go s.Run(ctx)
	for !s.looping.Load() {
		time.Sleep(time.Millisecond)
	}
	if err := s.Untrack(ctx, testkit.RefA.Key()); err != nil {
		t.Fatal(err)
	}
	st := s.Status()
	if len(st.PRs) != 0 || len(st.Ignored) != 1 || st.Ignored[0].Key() != testkit.RefA.Key() {
		t.Fatalf("PRs %+v, ignored %+v", st.PRs, st.Ignored)
	}
	if err := s.Untrack(ctx, testkit.RefA.Key()); err == nil {
		t.Fatal("untracked twice")
	}
	if err := s.Retrack(ctx, testkit.RefA.Key()); err != nil {
		t.Fatal(err)
	}
	if len(s.Status().Ignored) != 0 {
		t.Fatalf("still ignored: %+v", s.Status().Ignored)
	}
	if err := s.ResetReviews(ctx, testkit.RefA.Key()); err == nil {
		t.Fatal("reset with reviews disabled")
	}
}

func TestBudgetNotes(t *testing.T) {
	resets := time.Now().Add(time.Hour)
	at := resets.Local().Format("15:04")
	for _, c := range []struct {
		b          review.Budget
		note, wait string
	}{
		{review.Budget{}, "", ""},
		{review.Budget{Share: 0.25}, "Reviews may use 25% of each 5-hour window; none used in this one yet.", ""},
		{review.Budget{Share: 0.25, Used: 0.04, Account: 0.22, ResetsAt: resets},
			"5-hour window: reviews used 4% of their 25% · account at 22% · resets " + at, ""},
		{review.Budget{Share: 0.25, Used: 0.26, Account: 0.4, ResetsAt: resets},
			"5-hour window: reviews used 26% of their 25% · account at 40% · resets " + at,
			"Reviews used their 25% share of the 5-hour window. Queued reviews start after it resets at " + at + "."},
	} {
		if note, wait := budgetNotes(c.b); note != c.note || wait != c.wait {
			t.Errorf("%+v:\n got %q, %q\nwant %q, %q", c.b, note, wait, c.note, c.wait)
		}
	}
}
