package app

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// Tones colour a verdict or a handoff state in the UIs.
const (
	ToneOK      = "ok"      // approved
	ToneChanges = "changes" // changes requested
	ToneNeutral = "neutral" // commented, not posted, cancelled
	ToneLive    = "live"    // reviewing or queued
	ToneBad     = "bad"     // needs a person
	ToneOpen    = "open"    // PR open, TLM has it in hand
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

// HandoffTone is the chip colour of a handoff state.
func HandoffTone(state string) string {
	switch state {
	case lastmile.StateCIRed, lastmile.StateConflict, lastmile.StateNeedsHuman:
		return ToneBad
	case lastmile.StateCIGreen, lastmile.StateApproved, lastmile.StateMerged:
		return ToneOK
	case lastmile.StatePROpen:
		return ToneOpen
	}
	return ToneNeutral
}

// HandoffIcon is a one-glyph cue for a handoff state, matching its tone.
func HandoffIcon(state string) string {
	switch state {
	case lastmile.StateMerged:
		return "✓✓"
	case lastmile.StateCIGreen, lastmile.StateApproved, lastmile.StateMergeable:
		return "✓"
	case lastmile.StateCIRed, lastmile.StateConflict:
		return "✗"
	case lastmile.StateNeedsHuman:
		return "!"
	case lastmile.StateClosed, lastmile.StateCancelled:
		return "⊘"
	case lastmile.StateCIRunning, lastmile.StateAwaitingReview:
		return "…"
	}
	return "→"
}

// StateEvent is a "state" timeline event split into the state entered and why.
type StateEvent struct{ State, Reason string }

// SplitStateEvent splits a "state" event's text, written as "state" or "state: reason".
func SplitStateEvent(text string) StateEvent {
	st, why, _ := strings.Cut(text, ": ")
	return StateEvent{st, why}
}

var reasons = map[string]string{review.ReasonNew: "PR first seen", review.ReasonCommits: "new commits",
	review.ReasonReplies: "new replies", review.ReasonManual: "run by you"}

// ReasonLabel says why a review run started.
func ReasonLabel(r string) string { return cmp.Or(reasons[r], r) }

// RunDuration is how long a run took, or has been running.
func RunDuration(r review.Run) string {
	end := r.Finished
	if r.Status == review.Running || end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(r.Started).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// PRView shows a history entry with the tracked PRs' page.
func (h HistoryRow) PRView() PRView {
	return PRView{ID: h.ID, Review: review.PRStatus{Runs: h.Runs}, Tracked: tracker.Tracked{
		PRRef: h.PRRef, URL: h.URL, Title: h.Title, Author: h.Author, TrackedAt: h.TrackedAt}}
}

// Board is the dashboard pages' reading of a Status: what needs you, the PR groups and the counts.
type Board struct{ Status }

// PRGroup is one heading of the PR list.
type PRGroup struct {
	Label string
	PRs   []PRView
}

var groupLabels = []struct{ key, label string }{
	{GroupAttention, "Needs you"}, {GroupReviewing, "Reviewing"}, {GroupReviewed, "Reviewed"}, {GroupWaiting, "Not reviewed yet"},
}

// Groups is the PR list by what the user has to do, empty groups left out.
func (b Board) Groups() []PRGroup {
	var out []PRGroup
	for _, g := range groupLabels {
		if prs := b.InGroup(g.key); len(prs) > 0 {
			out = append(out, PRGroup{g.label, prs})
		}
	}
	return out
}

// InGroup lists the PRs in one Group*.
func (b Board) InGroup(group string) []PRView {
	var out []PRView
	for _, pr := range b.PRs {
		if pr.Group == group {
			out = append(out, pr)
		}
	}
	return out
}

// AwaitingYou lists the PRs sisyphus reviewed that wait for your verdict, newest first.
func (b Board) AwaitingYou() []PRView {
	out := slices.DeleteFunc(b.InGroup(GroupReviewed), func(p PRView) bool {
		return p.Tone != ToneOK && p.Tone != ToneChanges
	})
	slices.SortFunc(out, func(a, b PRView) int { return b.VerdictAt.Compare(a.VerdictAt) })
	return out
}

// Failing lists the PRs whose review or sync failed.
func (b Board) Failing() []PRView { return b.InGroup(GroupAttention) }

// NeedsHuman lists the handoffs waiting for a person.
func (b Board) NeedsHuman() []lastmile.View {
	return slices.DeleteFunc(slices.Clone(b.Handoffs), func(v lastmile.View) bool { return v.State != lastmile.StateNeedsHuman })
}

// NeedsCount is the number of items in the "Needs you" list.
func (b Board) NeedsCount() int {
	return len(b.NeedsHuman()) + len(b.Failing()) + len(b.AwaitingYou())
}

// Queued counts the PRs waiting for a run slot.
func (b Board) Queued() (n int) {
	for _, p := range b.PRs {
		if p.Review.State == review.StateQueued {
			n++
		}
	}
	return n
}

// Recent is the latest few review runs.
func (b Board) Recent() []Activity { return b.Activity[:min(len(b.Activity), 3)] }

// AgentsRunning lists the handoffs with an agent run in flight, oldest run first.
func (b Board) AgentsRunning() []lastmile.View {
	out := slices.DeleteFunc(slices.Clone(b.Handoffs), func(v lastmile.View) bool { return v.AgentRunning == "" })
	slices.SortFunc(out, func(a, b lastmile.View) int { return a.AgentStarted.Compare(b.AgentStarted) })
	return out
}

// LMCount counts the open handoffs in any of the states; none = every open one.
func (b Board) LMCount(states ...string) (n int) {
	for _, v := range b.Handoffs {
		if len(states) == 0 && !v.Over || slices.Contains(states, v.State) {
			n++
		}
	}
	return n
}

// Merged is the last few merged handoffs, newest first.
func (b Board) Merged() []lastmile.View {
	out := slices.DeleteFunc(slices.Clone(b.Handoffs), func(v lastmile.View) bool { return v.State != lastmile.StateMerged })
	slices.SortFunc(out, func(a, b lastmile.View) int { return b.Updated.Compare(a.Updated) })
	return out[:min(len(out), 3)]
}
