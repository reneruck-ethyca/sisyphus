package lastmile

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// shadowRefresh is how often an unchanged verdict is logged again.
const shadowRefresh = time.Hour

// verdictLine and outcomeLine are what lastmile-shadow.jsonl holds; shadowLine
// is both, for reading (an outcome has Outcome set).
type verdictLine struct {
	Time       time.Time `json:"time"`
	HandoffID  string    `json:"handoff_id"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Head       string    `json:"head"`
	WouldMerge bool      `json:"would_merge"`
	Blockers   []string  `json:"blockers"`
	Signals    Signals   `json:"signals"`
}

type outcomeLine struct {
	Time               time.Time `json:"time"`
	HandoffID          string    `json:"handoff_id"`
	Repo               string    `json:"repo"`
	PR                 int       `json:"pr"`
	Outcome            string    `json:"outcome"` // merged | closed
	MergedBy           string    `json:"merged_by,omitempty"`
	LastVerdict        *Verdict  `json:"last_verdict"` // null when none was ever made
	LastVerdictAt      time.Time `json:"last_verdict_at,omitzero"`
	VerdictHeadMatches bool      `json:"verdict_head_matches"`
}

type shadowLine struct {
	Time       time.Time `json:"time"`
	HandoffID  string    `json:"handoff_id"`
	Repo       string    `json:"repo"`
	PR         int       `json:"pr"`
	Head       string    `json:"head,omitempty"`
	WouldMerge bool      `json:"would_merge,omitempty"`
	Blockers   []string  `json:"blockers,omitempty"`
	Signals    *Signals  `json:"signals,omitempty"`

	Outcome            string    `json:"outcome,omitempty"`
	MergedBy           string    `json:"merged_by,omitempty"`
	LastVerdict        *Verdict  `json:"last_verdict,omitempty"`
	LastVerdictAt      time.Time `json:"last_verdict_at,omitzero"`
	VerdictHeadMatches bool      `json:"verdict_head_matches,omitempty"`
}

func (m *Manager) shadowPath() string {
	return filepath.Join(filepath.Dir(m.path), "lastmile-shadow.jsonl")
}

func (m *Manager) appendShadow(l any) {
	b, err := json.Marshal(l)
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(m.shadowPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_, err = f.Write(append(b, '\n'))
			f.Close()
		}
	}
	if err != nil {
		slog.Warn("lastmile shadow log failed", "err", err)
	}
}

// judge evaluates the merge policy on h's open, ready PR. The verdict kept on h
// (and, for merge=shadow, logged) only changes when it differs from the last or
// is an hour old, so a quiet PR neither grows the log nor rewrites the store.
func (m *Manager) judge(h *Handoff) {
	s := h.Snapshot
	if h.PRNumber == 0 || s == nil || s.Draft || Terminal(h.State) {
		return
	}
	v := evaluate(*h, *s, m.cfg.LastMile, m.now())
	if p := h.Shadow; p != nil && p.Head == v.Head && p.WouldMerge == v.WouldMerge && slices.Equal(p.Blockers, v.Blockers) &&
		v.At.Sub(p.At) < shadowRefresh {
		return
	}
	h.Shadow = &v
	if h.Autonomy.Merge == MergeShadow {
		m.appendShadow(verdictLine{Time: v.At, HandoffID: h.ID, Repo: h.Repo, PR: h.PRNumber, Head: v.Head,
			WouldMerge: v.WouldMerge, Blockers: v.Blockers, Signals: v.Signals})
	}
}

// outcome records what a human did with a merge=shadow handoff's PR, next to the
// last verdict. A close counts: "closed by human" is part of the comparison.
func (m *Manager) outcome(h Handoff, mergedBy, head, outcome string, at time.Time) {
	if h.Autonomy.Merge != MergeShadow {
		return
	}
	if at.IsZero() {
		at = m.now()
	}
	l := outcomeLine{Time: at, HandoffID: h.ID, Repo: h.Repo, PR: h.PRNumber, Outcome: outcome, MergedBy: mergedBy, LastVerdict: h.Shadow}
	if h.Shadow != nil {
		l.LastVerdictAt = h.Shadow.At
		l.VerdictHeadMatches = head != "" && h.Shadow.Head == head
	}
	m.appendShadow(l)
}

// ShadowQuery narrows the report to one PR ("owner/repo#n") or one handoff.
type ShadowQuery struct {
	PR      string
	Handoff string
}

// Agreement counts PRs by what the human did.
type Agreement struct {
	Merged int `json:"merged"`
	Closed int `json:"closed"`
	Open   int `json:"open"`
}

// ShadowPR is one PR's row: its latest verdict and, once there is one, its outcome.
type ShadowPR struct {
	HandoffID          string    `json:"handoff_id"`
	Repo               string    `json:"repo"`
	PR                 int       `json:"pr"`
	Head               string    `json:"head"`
	VerdictAt          time.Time `json:"verdict_at"`
	WouldMerge         bool      `json:"would_merge"`
	Blockers           []string  `json:"blockers"`
	Signals            Signals   `json:"signals"`
	Outcome            string    `json:"outcome"` // merged | closed | "" while open
	OutcomeAt          time.Time `json:"outcome_at,omitzero"`
	MergedBy           string    `json:"merged_by,omitempty"`
	VerdictHeadMatches bool      `json:"verdict_head_matches"`
}

// ShadowReport summarises the shadow log. The agreement matrix is by each PR's
// last verdict; a PR without any verdict is counted in the totals only.
type ShadowReport struct {
	Totals struct {
		Observed int `json:"observed"`
		Merged   int `json:"merged"`
		Closed   int `json:"closed"`
		Open     int `json:"open"`
	} `json:"totals"`
	Agreement struct {
		WouldMergeYes Agreement `json:"would_merge_yes"`
		WouldMergeNo  Agreement `json:"would_merge_no"`
	} `json:"agreement"`
	MeanTimeInReviewSec float64      `json:"mean_time_in_review_sec"` // over merged PRs, at their last verdict
	PRs                 []ShadowPR   `json:"prs"`                     // in order of first sight
	History             []shadowLine `json:"history,omitempty"`       // with a query: every verdict and outcome line of the match
}

// ShadowReport reads lastmile-shadow.jsonl line by line; a corrupt line is
// logged and skipped. A missing file is an empty report.
func (m *Manager) ShadowReport(q ShadowQuery) (ShadowReport, error) {
	rep := ShadowReport{PRs: []ShadowPR{}}
	var wantRepo string
	var wantPR int
	if q.PR != "" {
		r, n, ok := strings.Cut(q.PR, "#")
		if _, err := fmt.Sscanf(n, "%d", &wantPR); !ok || err != nil || r == "" {
			return rep, InvalidError(fmt.Sprintf("pr %q must be owner/repo#n", q.PR))
		}
		wantRepo = r
	}
	f, err := os.Open(m.shadowPath())
	if errors.Is(err, os.ErrNotExist) {
		return rep, nil
	}
	if err != nil {
		return rep, err
	}
	defer f.Close()

	rows := map[string]*ShadowPR{}
	var order []string
	br := bufio.NewReader(f)
	for n := 1; ; n++ {
		raw, rerr := br.ReadBytes('\n')
		if len(strings.TrimSpace(string(raw))) > 0 {
			var l shadowLine
			switch jerr := json.Unmarshal(raw, &l); {
			case jerr != nil || l.HandoffID == "":
				slog.Warn("lastmile shadow log: skipping corrupt line", "line", n, "err", jerr)
			case (q.Handoff != "" && l.HandoffID != q.Handoff) || (q.PR != "" && (l.Repo != wantRepo || l.PR != wantPR)):
			default:
				row := rows[l.HandoffID]
				if row == nil {
					row = &ShadowPR{HandoffID: l.HandoffID, Repo: l.Repo, PR: l.PR, Blockers: []string{}}
					rows[l.HandoffID] = row
					order = append(order, l.HandoffID)
				}
				apply(row, l)
				if q.PR != "" || q.Handoff != "" {
					rep.History = append(rep.History, l)
				}
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				return rep, rerr
			}
			break
		}
	}

	var sum float64
	var merged int
	for _, id := range order {
		r := *rows[id]
		rep.PRs = append(rep.PRs, r)
		rep.Totals.Observed++
		var a *Agreement
		switch {
		case r.VerdictAt.IsZero():
		case r.WouldMerge:
			a = &rep.Agreement.WouldMergeYes
		default:
			a = &rep.Agreement.WouldMergeNo
		}
		switch r.Outcome {
		case "merged":
			rep.Totals.Merged++
			sum += float64(r.Signals.TimeInReviewSec)
			merged++
			if a != nil {
				a.Merged++
			}
		case "closed":
			rep.Totals.Closed++
			if a != nil {
				a.Closed++
			}
		default:
			rep.Totals.Open++
			if a != nil {
				a.Open++
			}
		}
	}
	if merged > 0 {
		rep.MeanTimeInReviewSec = sum / float64(merged)
	}
	return rep, nil
}

// apply folds one log line into its PR's row (lines come oldest first).
func apply(r *ShadowPR, l shadowLine) {
	if l.Outcome == "" {
		r.Head, r.VerdictAt, r.WouldMerge, r.Blockers = l.Head, l.Time, l.WouldMerge, append([]string{}, l.Blockers...)
		if l.Signals != nil {
			r.Signals = *l.Signals
		}
		return
	}
	r.Outcome, r.OutcomeAt, r.MergedBy, r.VerdictHeadMatches = l.Outcome, l.Time, l.MergedBy, l.VerdictHeadMatches
	if v := l.LastVerdict; v != nil && r.VerdictAt.IsZero() { // the log lost the verdict lines; the outcome kept the last one
		r.Head, r.VerdictAt, r.WouldMerge, r.Blockers, r.Signals = v.Head, v.At, v.WouldMerge, v.Blockers, v.Signals
	}
}
