package lastmile

import (
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
)

// CI rollup of a snapshot's checks.
const (
	CIPending = "pending"
	CIGreen   = "green"
	CIRed     = "red"
)

// noChecksGrace is how long a PR without any check counts as pending: checks
// register a moment after the PR opens, and a repo without CI turns green after it.
const noChecksGrace = 5 * time.Minute

// Approval is a reviewer's latest standing approval.
type Approval struct {
	User      string `json:"user"`
	CommitSHA string `json:"commit"`
	Stale     bool   `json:"stale"` // the head moved on after it
}

// Snapshot is the PR as TLM sees it, built from one `gh pr view` plus local
// git (design §4). Review threads, failed job logs and the body-edit guard
// join it in later phases. Who TLM requested and who was removed live on the
// Handoff.
type Snapshot struct {
	Head           string         `json:"head"`
	Author         string         `json:"author"`
	Draft          bool           `json:"draft"`
	Mergeable      string         `json:"mergeable"`
	MergeState     string         `json:"merge_state"`
	ReviewDecision string         `json:"review_decision"`
	Approvals      []Approval     `json:"approvals,omitempty"`
	Requested      []string       `json:"requested,omitempty"` // users GitHub shows as requested now
	Checks         []github.Check `json:"checks,omitempty"`
	CI             string         `json:"ci"`
	Base           string         `json:"base"`
	Behind         int            `json:"behind"` // commits of origin/<base> the branch lacks
	Additions      int            `json:"additions"`
	Deletions      int            `json:"deletions"`
}

// Ready says the PR is out of draft.
func (s Snapshot) Ready() bool { return !s.Draft }

// Approved says the PR has the approval it needs: GitHub's decision, or, on a
// repo that requires no review, an approval of the current head.
func (s Snapshot) Approved() bool {
	fresh := slices.ContainsFunc(s.Approvals, func(a Approval) bool { return !a.Stale })
	return s.ReviewDecision == "APPROVED" || (s.ReviewDecision == "" && fresh)
}

// buildSnapshot reads gh's view of the PR into a Snapshot; the caller adds Behind.
func buildSnapshot(p github.PRSnapshot, opened, now time.Time) Snapshot {
	s := Snapshot{Head: p.HeadSHA, Author: p.Author, Draft: p.Draft, Mergeable: p.Mergeable, MergeState: p.MergeState,
		ReviewDecision: p.ReviewDecision, Requested: p.RequestedUsers, Checks: p.Checks, Base: p.BaseRef,
		Additions: p.Additions, Deletions: p.Deletions}
	s.CI = rollup(p.Checks, opened, now)

	// A user's latest approval or change request stands; a plain comment doesn't replace it.
	latest := map[string]github.SnapReview{}
	reviews := slices.SortedStableFunc(slices.Values(p.Reviews), func(a, b github.SnapReview) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, r := range reviews {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[r.User] = r
		case "DISMISSED":
			delete(latest, r.User)
		}
	}
	for _, r := range reviews { // in review order, once per user
		if l, ok := latest[r.User]; ok && l.State == "APPROVED" && l == r {
			s.Approvals = append(s.Approvals, Approval{User: r.User, CommitSHA: r.CommitSHA, Stale: r.CommitSHA != p.HeadSHA})
		}
	}
	return s
}

// rollup is red when any check failed, pending while any still runs, else green.
// Skipped and neutral checks pass. A failure outranks a still-running check, so
// a red PR is reported without waiting for the rest.
func rollup(checks []github.Check, opened, now time.Time) string {
	if len(checks) == 0 {
		if now.Sub(opened) < noChecksGrace {
			return CIPending
		}
		return CIGreen
	}
	pending := false
	for _, c := range checks {
		switch {
		case c.Status != "COMPLETED":
			pending = true
		case !passing(c.Conclusion):
			return CIRed
		}
	}
	if pending {
		return CIPending
	}
	return CIGreen
}

func passing(conclusion string) bool {
	return conclusion == "SUCCESS" || conclusion == "SKIPPED" || conclusion == "NEUTRAL"
}

// failedChecks names the checks that failed, for a reason.
func failedChecks(checks []github.Check) string {
	var names []string
	for _, c := range checks {
		if c.Status == "COMPLETED" && !passing(c.Conclusion) {
			names = append(names, c.Name)
		}
	}
	return strings.Join(names, ", ")
}

// nextState is where a handoff with an open PR goes after reading s, and why.
// The agent-driven moves (feedback, conflict, fixing) come in later phases.
// needs-human is sticky: only a person (or a later phase) moves it on.
func nextState(h Handoff, s Snapshot) (state, reason string) {
	switch {
	case h.State == StateNeedsHuman:
		return h.State, h.Reason
	case s.CI == CIPending:
		return StateCIRunning, ""
	case s.CI == CIRed && h.Autonomy.FixCI:
		return StateCIRed, "fix agent arrives in M1"
	case s.CI == CIRed:
		return StateNeedsHuman, "CI failed: " + failedChecks(s.Checks)
	case s.Approved():
		return StateApproved, ""
	case h.HumansRequested:
		return StateAwaitingReview, ""
	}
	return StateCIGreen, ""
}
