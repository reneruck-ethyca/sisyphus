package app

import (
	"slices"
	"time"

	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// HistoryRow is a PR sisyphus no longer tracks, for the history page.
type HistoryRow struct {
	tracker.Dropped              // DroppedAt is zero when unknown
	ID              string       `json:"id"`
	Runs            []review.Run `json:"runs"` // newest first; none left after an untrack, reset or wipe
}

// History lists the PRs sisyphus tracked and no longer does, most recently picked up first. PRs
// reviewed before drops were recorded show with reason "unknown".
func (s *Service) History() []HistoryRow {
	var reviews map[string]review.PRStatus
	if s.rev != nil {
		reviews = s.rev.Status()
	}
	rows := map[string]HistoryRow{}
	for _, d := range s.tracker.History() {
		rows[d.Key()] = HistoryRow{Dropped: d}
	}
	for id, rs := range reviews {
		if _, ok := rows[id]; !ok && len(rs.Runs) > 0 {
			rows[id] = HistoryRow{Dropped: rs.Runs[0].Job.PR.AsDropped("unknown", time.Time{})}
		}
	}
	s.mu.Lock()
	for _, tr := range s.tracked { // re-tracked PRs too
		delete(rows, tr.Key())
	}
	s.mu.Unlock()
	out := make([]HistoryRow, 0, len(rows))
	for id, r := range rows {
		r.ID, r.Runs = id, reviews[id].Runs
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b HistoryRow) int { return b.TrackedAt.Compare(a.TrackedAt) })
	return out
}
