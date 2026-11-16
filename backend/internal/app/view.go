package app

import (
	"cmp"
	"fmt"
	"slices"
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
	GroupAuthor    = "author"  // a design review you've reviewed, waiting on its author
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
	case tr.Design != nil:
		return designVerdict(tr)
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

// designVerdict places a design review by where the user's own review stands:
// it needs them when new, or when something came in since they reviewed.
func designVerdict(tr tracker.Tracked) (label, tone, group string, at time.Time) {
	d := *tr.Design
	switch {
	case d.ReviewedAt.IsZero():
		return "Design · new", ToneLive, GroupAttention, tr.TrackedAt
	case d.Updated():
		return "Design · updated", ToneLive, GroupAttention, tr.LastSeenAt
	case d.Verdict == "APPROVED":
		return "You approved", ToneOK, GroupAuthor, d.ReviewedAt
	case d.Verdict == "CHANGES_REQUESTED":
		return "You requested changes", ToneChanges, GroupAuthor, d.ReviewedAt
	}
	return "You commented", ToneNeutral, GroupAuthor, d.ReviewedAt
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

// Board is the dashboard pages' reading of a Status: what needs you, the PR groups and the counts.
type Board struct{ Status }

// PRGroup is one heading of the PR list.
type PRGroup struct {
	Label string   `json:"label"`
	PRs   []PRView `json:"prs"`
}

var groupLabels = []struct{ key, label string }{
	{GroupAttention, "Needs you"}, {GroupReviewing, "Reviewing"}, {GroupReviewed, "Reviewed"}, {GroupAuthor, "Waiting on the author"}, {GroupWaiting, "Not reviewed yet"},
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

// AgentsRunning lists the handoffs with an agent run in flight, oldest run first.
func (b Board) AgentsRunning() []lastmile.View {
	out := slices.DeleteFunc(slices.Clone(b.Handoffs), func(v lastmile.View) bool { return v.AgentRunning == "" })
	slices.SortFunc(out, func(a, b lastmile.View) int { return a.AgentStarted.Compare(b.AgentStarted) })
	return out
}

// Merged is the last few merged handoffs, newest first.
func (b Board) Merged() []lastmile.View {
	out := slices.DeleteFunc(slices.Clone(b.Handoffs), func(v lastmile.View) bool { return v.State != lastmile.StateMerged })
	slices.SortFunc(out, func(a, b lastmile.View) int { return b.Updated.Compare(a.Updated) })
	return out[:min(len(out), 3)]
}

// BoardView is a Board as the UI reads it: the status, and what needs you and
// the PR groups worked out from it.
type BoardView struct {
	Status
	Groups        []PRGroup       `json:"groups"`
	NeedsHuman    []lastmile.View `json:"needs_human"`
	Failing       []PRView        `json:"failing"`
	AwaitingYou   []PRView        `json:"awaiting_you"`
	NeedsCount    int             `json:"needs_count"`
	AgentsRunning []lastmile.View `json:"agents_running"`
	Merged        []lastmile.View `json:"merged"`
	Queued        int             `json:"queued"`
}

func (b Board) View() BoardView {
	return BoardView{Status: b.Status, Groups: b.Groups(), NeedsHuman: b.NeedsHuman(), Failing: b.Failing(), AwaitingYou: b.AwaitingYou(),
		NeedsCount: b.NeedsCount(), AgentsRunning: b.AgentsRunning(), Merged: b.Merged(), Queued: b.Queued()}
}
