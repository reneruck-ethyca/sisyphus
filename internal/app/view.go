package app

import (
	"cmp"
	"fmt"
	"slices"
	"time"

	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// Tones colour a verdict in the UIs.
const (
	ToneOK      = "ok"      // approved
	ToneChanges = "changes" // changes requested
	ToneNeutral = "neutral" // commented, not posted, cancelled
	ToneLive    = "live"    // reviewing or queued
	ToneBad     = "bad"     // needs a person
)

// A tracked PR's sync health (PRView.Health).
const (
	HealthOK       = "ok"
	HealthConflict = "conflict" // the worktree can't be synced to the head without a person
	HealthError    = "error"    // the last sync or fetch failed
)

// Groups sort the PR list by what the user has to do.
const (
	GroupAttention = "attention"
	GroupReviewing = "reviewing"
	GroupReviewed  = "reviewed"
	GroupWaiting   = "waiting" // not reviewed yet
)

// Outcome says in a word or two what a run came to: what was posted, not how
// the container exited.
func Outcome(r review.Run) (label, tone string) {
	switch r.Status {
	case review.Running:
		return "Reviewing", ToneLive
	case review.Cancelled:
		return "Cancelled", ToneNeutral
	case review.Lost:
		return "Interrupted", ToneNeutral
	case review.Succeeded:
	default:
		return "Failed", ToneBad
	}
	switch p := r.Posted; {
	case p == nil:
		return "Not posted", ToneNeutral
	case p.Error != "":
		return "Posting failed", ToneBad
	case p.Event == "APPROVE":
		return "Approved", ToneOK
	case p.Event == "REQUEST_CHANGES":
		return "Changes requested", ToneChanges
	case p.Event == "COMMENT":
		return "Commented", ToneNeutral
	}
	return "Replied", ToneNeutral
}

// verdict places a PR in the list: its label, tone, group, and when that last changed.
func verdict(tr tracker.Tracked, health string, rs review.PRStatus) (label, tone, group string, at time.Time) {
	last := rs.Latest()
	switch {
	case health == HealthConflict:
		return "Merge conflict", ToneBad, GroupAttention, tr.LastSeenAt
	case health == HealthError:
		return "Sync failed", ToneBad, GroupAttention, tr.LastSeenAt
	case rs.State == review.StateRunning && last != nil:
		return "Reviewing", ToneLive, GroupReviewing, last.Started
	case rs.State != "": // queued, or a stopped run letting go
		return "Queued", ToneLive, GroupReviewing, time.Time{}
	case last == nil:
		return "Not reviewed", ToneNeutral, GroupWaiting, tr.TrackedAt
	}
	label, tone = Outcome(*last)
	group = GroupReviewed
	if tone == ToneBad {
		group = GroupAttention
	}
	return label, tone, group, last.Finished
}

// Activity is one finished or started review run, for the overview's feed.
type Activity struct {
	PR     string    `json:"pr"`
	Run    string    `json:"run"`
	Title  string    `json:"title"`
	Label  string    `json:"label"`
	Tone   string    `json:"tone"`
	Detail string    `json:"detail"` // inline comments, or why it failed
	At     time.Time `json:"at"`
}

const maxActivity = 8

func activity(prs []PRView) []Activity {
	var out []Activity
	for _, pr := range prs {
		for _, r := range pr.Review.Runs {
			label, tone := Outcome(r)
			a := Activity{PR: pr.ID, Run: r.ID, Title: pr.Title, Label: label, Tone: tone, At: cmp.Or(r.Finished, r.Started), Detail: r.Error}
			if p := r.Posted; p != nil && p.Error == "" && p.Comments > 0 {
				a.Detail = fmt.Sprintf("%d inline comment", p.Comments)
				if p.Comments > 1 {
					a.Detail += "s"
				}
			}
			out = append(out, a)
		}
	}
	slices.SortFunc(out, func(a, b Activity) int { return b.At.Compare(a.At) })
	return out[:min(len(out), maxActivity)]
}

// upToDate is true once the latest review covered the current head and nothing is pending.
func upToDate(tr tracker.Tracked, rs review.PRStatus) bool {
	last := rs.Latest()
	return rs.State == "" && last != nil && last.Status == review.Succeeded && last.Job.PR.HeadSHA == tr.HeadSHA
}
