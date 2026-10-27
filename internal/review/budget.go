package review

import (
	"cmp"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"
)

// Window is one reading of the subscription's 5-hour rate-limit window: the
// share of it used so far (0–1, by the user and waterboy together) and when it resets.
type Window struct {
	ResetsAt time.Time
	Used     float64
}

// plausible rejects readings no real window has. The container's output also
// carries setup steps running the PR's code, which could print fake readings;
// this keeps one from holding reviews back past the current window.
func (w Window) plausible(now time.Time) bool {
	return w.Used >= 0 && w.Used <= 1 && w.ResetsAt.After(now) && w.ResetsAt.Before(now.Add(5*time.Hour+time.Minute))
}

// Span is how far a run saw one window's utilization rise, from its first
// reading to its last. Replaying a run's log (after a restart) yields the same span.
// ponytail: the first reading already includes the run's first call, so a run's
// share is undercounted by under a percentage point; readings come in whole points.
type Span struct {
	ResetsAt time.Time `json:"resets_at"`
	First    float64   `json:"first"`
	Last     float64   `json:"last"`
}

func observe(spans []Span, w Window) []Span {
	for i := range spans {
		if spans[i].ResetsAt.Equal(w.ResetsAt) {
			spans[i].First, spans[i].Last = min(spans[i].First, w.Used), max(spans[i].Last, w.Used)
			return spans
		}
	}
	return append(spans, Span{ResetsAt: w.ResetsAt, First: w.Used, Last: w.Used})
}

// Budget is waterboy's use of the current 5-hour window against its share.
type Budget struct {
	Share    float64   `json:"share"`     // what waterboy may use of each window, 0–1; 0 = no limit
	Used     float64   `json:"used"`      // what its runs used of the current window
	Account  float64   `json:"account"`   // the window's latest reading: everyone's use
	ResetsAt time.Time `json:"resets_at"` // zero = no reading of a current window yet
}

func (b Budget) Exhausted() bool { return b.Share > 0 && b.Used >= b.Share-1e-9 }

// Budget is the current window's budget, from the run records.
func (r *Reviewer) Budget() Budget {
	r.mu.Lock()
	defer r.mu.Unlock()
	return budget(r.st.Runs, float64(r.cfg.FiveHourPct)/100, time.Now())
}

// budget adds up the rises runs saw in the latest window. Waterboy's use is the
// union of its runs' spans: utilization only grows within a window, so runs that
// overlapped in time overlap here too and aren't counted twice. Whatever the user
// used meanwhile counts as waterboy's.
// ponytail: runs trimmed past keepRuns, or wiped by a reset, drop out of the sum.
func budget(runs map[string][]Run, share float64, now time.Time) Budget {
	b := Budget{Share: share}
	var spans []Span
	for _, rs := range runs {
		for _, run := range rs {
			for _, s := range run.Usage {
				if s.ResetsAt.After(b.ResetsAt) {
					b.ResetsAt, b.Account, spans = s.ResetsAt, 0, nil
				}
				if s.ResetsAt.Equal(b.ResetsAt) {
					b.Account = max(b.Account, s.Last)
					spans = append(spans, s)
				}
			}
		}
	}
	if !b.ResetsAt.After(now) {
		return Budget{Share: share} // that window is over: a new one starts unused
	}
	slices.SortFunc(spans, func(x, y Span) int { return cmp.Compare(x.First, y.First) })
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.First > cur.Last {
			b.Used += cur.Last - cur.First
			cur = s
		}
		cur.Last = max(cur.Last, s.Last)
	}
	b.Used += cur.Last - cur.First
	return b
}

// logUsage appends one of Claude's rate-limit or result lines, verbatim, to
// usage.jsonl: raw data for working out better accounting later. It outlives
// run records, which are trimmed and wiped by resets.
// ponytail: a log replayed after a restart appends its lines again, timed at the
// replay; dedupe on the line's uuid, keeping the first.
func (r *Reviewer) logUsage(prID, runID, line string) {
	b, err := json.Marshal(struct {
		At   time.Time       `json:"at"`
		PR   string          `json:"pr"`
		Run  string          `json:"run"`
		Line json.RawMessage `json:"line"`
	}{time.Now().UTC(), prID, runID, json.RawMessage(line)})
	if err == nil {
		var f *os.File
		if f, err = os.OpenFile(filepath.Join(r.dir, "usage.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
			_, err = f.Write(append(b, '\n')) // one write per line: concurrent runs don't interleave
			f.Close()
		}
	}
	if err != nil {
		slog.Warn("logging usage", "run", runID, "err", err)
	}
}
