package lastmile

import (
	"os"
	"strings"
	"testing"
)

// fixture: h1 would merge and was merged; h2 wouldn't and was closed; h3 wouldn't and is open
// (two verdicts, the last counts); h4 has only an outcome line; one line is corrupt.
const shadowFixture = `{"time":"2026-11-05T10:00:00Z","handoff_id":"h1","repo":"o/r","pr":1,"head":"a","would_merge":false,"blockers":["x"],"signals":{"time_in_review_sec":100}}
{"time":"2026-11-05T11:00:00Z","handoff_id":"h1","repo":"o/r","pr":1,"head":"a","would_merge":true,"blockers":[],"signals":{"time_in_review_sec":200}}
{"time":"2026-11-05T11:30:00Z","handoff_id":"h2","repo":"o/r","pr":2,"head":"b","would_merge":false,"blockers":["no current human approval"],"signals":{}}
this is not json
{"time":"2026-11-05T11:40:00Z","handoff_id":"h3","repo":"o/r","pr":3,"head":"c","would_merge":true,"blockers":[],"signals":{}}
{"time":"2026-11-05T11:50:00Z","handoff_id":"h3","repo":"o/r","pr":3,"head":"c","would_merge":false,"blockers":["merge freeze"],"signals":{}}
{"time":"2026-11-05T12:00:00Z","handoff_id":"h1","repo":"o/r","pr":1,"outcome":"merged","merged_by":"carol","last_verdict":{"at":"2026-11-05T11:00:00Z","head":"a","would_merge":true,"blockers":[],"signals":{"time_in_review_sec":200}},"last_verdict_at":"2026-11-05T11:00:00Z","verdict_head_matches":true}
{"time":"2026-11-05T12:10:00Z","handoff_id":"h2","repo":"o/r","pr":2,"outcome":"closed","last_verdict":null,"verdict_head_matches":false}
{"time":"2026-11-05T12:20:00Z","handoff_id":"h4","repo":"o/q","pr":4,"outcome":"merged","merged_by":"dave","last_verdict":{"at":"2026-11-05T12:00:00Z","head":"d","would_merge":true,"blockers":[],"signals":{"time_in_review_sec":400}},"verdict_head_matches":true}
`

func TestShadowReport(t *testing.T) {
	m, _, _ := fixture(t)
	if rep, err := m.ShadowReport(ShadowQuery{}); err != nil || rep.Totals.Observed != 0 || rep.PRs == nil {
		t.Fatalf("no log: %+v %v", rep, err)
	}
	os.WriteFile(m.shadowPath(), []byte(shadowFixture), 0o600)
	rep, err := m.ShadowReport(ShadowQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Totals.Observed != 4 || rep.Totals.Merged != 2 || rep.Totals.Closed != 1 || rep.Totals.Open != 1 {
		t.Errorf("totals = %+v", rep.Totals)
	}
	// h1 and h4 merged on a yes (h4's verdict came from its outcome line); h2 closed on a no; h3 open on a no.
	if y, n := rep.Agreement.WouldMergeYes, rep.Agreement.WouldMergeNo; y != (Agreement{Merged: 2}) || n != (Agreement{Closed: 1, Open: 1}) {
		t.Errorf("agreement yes %+v no %+v", y, n)
	}
	if rep.MeanTimeInReviewSec != 300 {
		t.Errorf("mean = %v", rep.MeanTimeInReviewSec)
	}
	if len(rep.PRs) != 4 || rep.History != nil {
		t.Fatalf("prs = %d, history %v", len(rep.PRs), rep.History)
	}
	if p := rep.PRs[0]; p.HandoffID != "h1" || !p.WouldMerge || p.Outcome != "merged" || p.MergedBy != "carol" || !p.VerdictHeadMatches {
		t.Errorf("h1 = %+v", p)
	}
	if p := rep.PRs[2]; p.WouldMerge || p.Outcome != "" || p.Blockers[0] != "merge freeze" {
		t.Errorf("h3 = %+v", p)
	}

	for _, q := range []ShadowQuery{{PR: "o/r#1"}, {Handoff: "h1"}} {
		rep, err = m.ShadowReport(q)
		if err != nil || len(rep.PRs) != 1 || len(rep.History) != 3 || rep.History[0].WouldMerge || rep.History[2].Outcome != "merged" {
			t.Errorf("%+v: %+v %v", q, rep, err)
		}
	}
	if _, err = m.ShadowReport(ShadowQuery{PR: "nonsense"}); err == nil || !strings.Contains(err.Error(), "owner/repo#n") {
		t.Errorf("bad pr: %v", err)
	}
}
