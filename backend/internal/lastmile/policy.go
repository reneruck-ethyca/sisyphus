package lastmile

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
)

// Approver is someone whose approval stands, and whether it is a bot's.
type Approver struct {
	User string `json:"user"`
	Bot  bool   `json:"bot"`
}

// Signals are what the merge policy looked at (design §7: collected from the
// start, even those no rule uses yet).
type Signals struct {
	Additions       int            `json:"additions"`
	Deletions       int            `json:"deletions"`
	Files           int            `json:"files"`
	RiskyPaths      []string       `json:"risky_paths"`
	FixRounds       map[string]int `json:"fix_rounds"` // by trigger
	Approvers       []Approver     `json:"approvers"`
	TimeInReviewSec int64          `json:"time_in_review_sec"` // since the PR was first ready
	FlakeReruns     int            `json:"flake_reruns"`       // over all heads
	AgentCommits    int            `json:"agent_commits"`
	Unresolved      int            `json:"unresolved"`
	Unanswered      int            `json:"unanswered"`
	Behind          int            `json:"behind"`
	ApprovalsStale  bool           `json:"approvals_stale"`
}

// Verdict is what the merge policy says about a PR at one moment: merge=auto
// merges on it, merge=shadow logs it.
type Verdict struct {
	At         time.Time `json:"at"`
	Head       string    `json:"head"`
	WouldMerge bool      `json:"would_merge"`
	Blockers   []string  `json:"blockers"`
	Signals    Signals   `json:"signals"`
}

// evaluate applies the merge rule set (design §7) to a snapshot. Pure: now is
// the only clock.
func evaluate(h Handoff, s Snapshot, cfg config.LastMile, now time.Time) Verdict {
	sig := Signals{Additions: s.Additions, Deletions: s.Deletions, Files: len(s.Files), RiskyPaths: []string{},
		FixRounds: map[string]int{}, Approvers: []Approver{}, FlakeReruns: h.FlakeTotal, Unresolved: s.Unresolved, Unanswered: s.Unanswered, Behind: s.Behind}
	for k, v := range h.FixRounds {
		sig.FixRounds[k] = v
	}
	for _, f := range s.Files {
		if slices.ContainsFunc(cfg.RiskyPaths, func(g string) bool { return globMatch(g, f) }) {
			sig.RiskyPaths = append(sig.RiskyPaths, f)
		}
	}
	logged := 0 // handoffs from before AgentCommits only have their events
	for _, e := range h.Events {
		if e.Kind == "commit" {
			logged++
		}
	}
	sig.AgentCommits = max(h.AgentCommits, logged)
	if !h.ReadyAt.IsZero() {
		sig.TimeInReviewSec = int64(now.Sub(h.ReadyAt).Seconds())
	}
	humanFresh := false
	for _, a := range s.Approvals {
		sig.Approvers = append(sig.Approvers, Approver{User: a.User, Bot: a.Bot})
		sig.ApprovalsStale = sig.ApprovalsStale || a.Stale
		humanFresh = humanFresh || (!a.Stale && !a.Bot)
	}

	b := []string{}
	switch {
	case s.ReviewDecision == "CHANGES_REQUESTED":
		b = append(b, "changes requested")
	case s.ReviewDecision == "REVIEW_REQUIRED":
		b = append(b, "review required")
	case !humanFresh:
		b = append(b, "no current human approval")
	}
	if sig.ApprovalsStale {
		b = append(b, "an approval is stale")
	}
	if s.CI != CIGreen {
		b = append(b, "checks not green ("+s.CI+")")
	}
	if s.Unanswered > 0 {
		b = append(b, fmt.Sprintf("%d unanswered threads", s.Unanswered))
	}
	switch {
	case s.MergeState == "DIRTY":
		b = append(b, "conflicts with base")
	case (s.Behind > 0 || s.MergeState == "BEHIND") && slices.ContainsFunc(s.Files, func(f string) bool {
		return slices.ContainsFunc(cfg.MigrationPaths, func(g string) bool { return globMatch(g, f) })
	}):
		b = append(b, "base not up to date (PR has migrations)")
	}
	if len(sig.RiskyPaths) > 0 {
		b = append(b, "risky paths: "+strings.Join(sig.RiskyPaths, ", "))
	}
	if slices.ContainsFunc(cfg.MergeFreeze, func(w config.FreezeWindow) bool { return w.Active(now) }) {
		b = append(b, "merge freeze")
	}
	return Verdict{At: now, Head: s.Head, WouldMerge: len(b) == 0, Blockers: b, Signals: sig}
}
