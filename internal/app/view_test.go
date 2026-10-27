package app

import (
	"testing"

	"github.com/reneruck/waterboy/internal/review"
	"github.com/reneruck/waterboy/internal/tracker"
)

func TestVerdict(t *testing.T) {
	ran := func(status string, posted *review.Posted) review.PRStatus {
		return review.PRStatus{Runs: []review.Run{{Status: status, Posted: posted}}}
	}
	for _, c := range []struct {
		name, health string
		rs           review.PRStatus
		label, group string
	}{
		{"approved", "ok", ran(review.Succeeded, &review.Posted{Event: "APPROVE"}), "Approved", GroupReviewed},
		{"changes", "ok", ran(review.Succeeded, &review.Posted{Event: "REQUEST_CHANGES"}), "Changes requested", GroupReviewed},
		{"dry run", "ok", ran(review.Succeeded, nil), "Not posted", GroupReviewed},
		{"post failed", "ok", ran(review.Succeeded, &review.Posted{Event: "APPROVE", Error: "403"}), "Posting failed", GroupAttention},
		{"run failed", "ok", ran(review.Failed, nil), "Failed", GroupAttention},
		{"running", "ok", review.PRStatus{State: "running", Runs: []review.Run{{Status: review.Running}}}, "Reviewing", GroupReviewing},
		{"queued", "ok", review.PRStatus{State: "queued"}, "Queued", GroupReviewing},
		{"never", "ok", review.PRStatus{}, "Not reviewed", GroupWaiting},
		{"conflict beats a review", "conflict", ran(review.Succeeded, &review.Posted{Event: "APPROVE"}), "Merge conflict", GroupAttention},
	} {
		if label, _, group, _ := verdict(tracker.Tracked{}, c.health, c.rs); label != c.label || group != c.group {
			t.Errorf("%s: got %q/%q, want %q/%q", c.name, label, group, c.label, c.group)
		}
	}
}
