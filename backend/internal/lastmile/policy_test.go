package lastmile

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
)

// okSnap is a PR the policy would merge.
func okSnap() Snapshot {
	return Snapshot{Head: "h", CI: CIGreen, Approvals: []Approval{{User: "carol", CommitSHA: "h"}}, ReviewDecision: "APPROVED",
		Additions: 10, Deletions: 5, Files: []string{"a.go", "b/c.go"}, MergeState: "CLEAN"}
}

func policyCfg() config.LastMile {
	return config.LastMile{MigrationPaths: []string{"**/migrations/**", "**/alembic/versions/**"}, RiskyPaths: []string{"**/migrations/**", "**/auth/**", ".github/**", "**/Dockerfile*", "infra/**", "**/*.tf"}}
}

func TestEvaluate(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC) // a Thursday
	cases := []struct {
		name    string
		mut     func(*Snapshot, *config.LastMile)
		blocker string // substring; "" = would merge
	}{
		{"all clear", func(*Snapshot, *config.LastMile) {}, ""},
		{"no approval", func(s *Snapshot, _ *config.LastMile) { s.Approvals, s.ReviewDecision = nil, "" }, "no human approval"},
		{"bot approval only", func(s *Snapshot, _ *config.LastMile) { s.Approvals[0].Bot = true }, "no human approval"},
		{"stale approval GitHub kept", func(s *Snapshot, _ *config.LastMile) { s.Approvals[0].Stale = true }, ""},
		{"stale approval, no review required", func(s *Snapshot, _ *config.LastMile) { s.Approvals[0].Stale, s.ReviewDecision = true, "" }, "no human approval"},
		{"changes requested", func(s *Snapshot, _ *config.LastMile) { s.ReviewDecision = "CHANGES_REQUESTED" }, "changes requested"},
		{"review required", func(s *Snapshot, _ *config.LastMile) { s.ReviewDecision = "REVIEW_REQUIRED" }, "review required"},
		{"ci red", func(s *Snapshot, _ *config.LastMile) { s.CI = CIRed }, "checks not green"},
		{"ci pending", func(s *Snapshot, _ *config.LastMile) { s.CI = CIPending }, "checks not green"},
		{"unanswered", func(s *Snapshot, _ *config.LastMile) { s.Unanswered = 2 }, "2 unanswered"},
		{"migrations", func(s *Snapshot, _ *config.LastMile) { s.Files = append(s.Files, "app/db/migrations/0001.py") }, "risky paths: app/db/migrations/0001.py"},
		{"workflow", func(s *Snapshot, _ *config.LastMile) { s.Files = []string{".github/workflows/ci.yml"} }, "risky paths"},
		{"terraform", func(s *Snapshot, _ *config.LastMile) { s.Files = []string{"deploy/x.tf"} }, "risky paths"},
		{"dockerfile", func(s *Snapshot, _ *config.LastMile) { s.Files = []string{"svc/Dockerfile.dev"} }, "risky paths"},
		{"risky off", func(s *Snapshot, c *config.LastMile) { s.Files, c.RiskyPaths = []string{"infra/x"}, nil }, ""},
		{"freeze", func(_ *Snapshot, c *config.LastMile) {
			c.MergeFreeze = []config.FreezeWindow{{Start: "11:00", End: "13:00"}}
		}, "merge freeze"},
		{"outside freeze", func(_ *Snapshot, c *config.LastMile) {
			c.MergeFreeze = []config.FreezeWindow{{Start: "13:00", End: "14:00"}}
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, cfg := okSnap(), policyCfg()
			c.mut(&s, &cfg)
			v := evaluate(Handoff{}, s, cfg, now)
			if c.blocker == "" {
				if !v.WouldMerge || len(v.Blockers) != 0 {
					t.Errorf("verdict = %+v", v)
				}
				return
			}
			const want = 1
			if v.WouldMerge || len(v.Blockers) != want || !slices.ContainsFunc(v.Blockers, func(b string) bool { return strings.Contains(b, c.blocker) }) {
				t.Errorf("blockers = %v, want %q (%d)", v.Blockers, c.blocker, want)
			}
		})
	}
}

func TestFreezeWindows(t *testing.T) {
	t.Parallel()
	ny := "America/New_York"
	cases := []struct {
		name string
		w    config.FreezeWindow
		at   time.Time
		want bool
	}{
		{"inside", config.FreezeWindow{Start: "09:00", End: "10:00"}, time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC), true},
		{"end exclusive", config.FreezeWindow{Start: "09:00", End: "10:00"}, time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), false},
		{"tz shifts it", config.FreezeWindow{Start: "09:00", End: "10:00", TZ: ny}, time.Date(2026, 11, 5, 14, 30, 0, 0, time.UTC), true}, // 09:30 EST
		{"tz outside", config.FreezeWindow{Start: "09:00", End: "10:00", TZ: ny}, time.Date(2026, 11, 5, 9, 30, 0, 0, time.UTC), false},   // 04:30 EST
		{"weekday match", config.FreezeWindow{Weekday: "Friday", Start: "00:00", End: "23:59"}, time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC), true},
		{"weekday miss", config.FreezeWindow{Weekday: "friday", Start: "00:00", End: "23:59"}, time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), false},
		{"weekday in tz", config.FreezeWindow{Weekday: "friday", Start: "00:00", End: "12:00", TZ: "Asia/Tokyo"}, time.Date(2026, 11, 5, 18, 0, 0, 0, time.UTC), true}, // Fri 03:00 JST
	}
	for _, c := range cases {
		if got := c.w.Active(c.at); got != c.want {
			t.Errorf("%s: Active = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSignals(t *testing.T) {
	t.Parallel()
	now := time.Now()
	h := Handoff{FixRounds: map[string]int{TriggerCI: 2}, FlakeTotal: 3, ReadyAt: now.Add(-90 * time.Second),
		Events: []Event{{Kind: "commit"}, {Kind: "reply"}, {Kind: "commit"}}}
	s := okSnap()
	s.Approvals = []Approval{{User: "carol"}, {User: "ethycabot", Bot: true, Stale: true}}
	s.Unresolved, s.Unanswered, s.Behind = 1, 1, 2
	g := evaluate(h, s, policyCfg(), now).Signals
	if g.Additions != 10 || g.Deletions != 5 || g.Files != 2 || g.FixRounds[TriggerCI] != 2 || g.FlakeReruns != 3 || g.AgentCommits != 2 ||
		g.TimeInReviewSec != 90 || g.Unresolved != 1 || g.Unanswered != 1 || g.Behind != 2 || !g.ApprovalsStale ||
		!slices.Equal(g.Approvers, []Approver{{"carol", false}, {"ethycabot", true}}) {
		t.Errorf("signals = %+v", g)
	}
	// Empty collections encode as [] and {} for the UI.
	b, _ := json.Marshal(evaluate(Handoff{}, okSnap(), policyCfg(), now))
	if !strings.Contains(string(b), `"risky_paths":[]`) || !strings.Contains(string(b), `"fix_rounds":{}`) || !strings.Contains(string(b), `"blockers":[]`) {
		t.Errorf("json = %s", b)
	}
}

// shadowTicks sets up a merge=shadow handoff with an open ready PR.
func shadowTicks(t *testing.T) (*Manager, *FakeGitHub, string, func()) {
	m, gh, _, id := opened(t, Request{Autonomy: "merge=shadow", Reviewers: []string{"carol"}})
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now }
	snap := github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")},
		Additions: 5, Files: []string{"a.go"}}
	gh.Snapshots[prRef.Key()] = snap
	return m, gh, id, func() { m.tick(context.Background()) }
}

func shadowLines(t *testing.T, m *Manager) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(m.shadowPath())
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var v map[string]any
		if err := json.Unmarshal([]byte(l), &v); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

func TestShadowLogChangeOnlyAndHourly(t *testing.T) {
	t.Parallel()
	m, gh, id, tick := shadowTicks(t)
	at := m.now()
	tick()
	h := m.must(t, id)
	if h.Shadow == nil || h.Shadow.WouldMerge || len(shadowLines(t, m)) != 1 {
		t.Fatalf("first tick: %+v, %d lines", h.Shadow, len(shadowLines(t, m)))
	}
	first := shadowLines(t, m)[0]
	if first["handoff_id"] != id || first["repo"] != "ethyca/fidesplus" || first["pr"] != float64(7) || first["head"] != "h" ||
		first["would_merge"] != false || first["signals"] == nil || first["blockers"] == nil || first["time"] == nil {
		t.Errorf("line = %v", first)
	}
	at = at.Add(10 * time.Minute)
	m.now = func() time.Time { return at }
	tick()
	if len(shadowLines(t, m)) != 1 {
		t.Error("an unchanged verdict was logged again")
	}
	// A new blocker (unchanged would_merge=false) is a change.
	s := gh.Snapshots[prRef.Key()]
	s.Files = []string{".github/workflows/x.yml"}
	gh.Snapshots[prRef.Key()] = s
	tick()
	if len(shadowLines(t, m)) != 2 {
		t.Error("a changed blocker was not logged")
	}
	at = at.Add(61 * time.Minute)
	tick()
	if len(shadowLines(t, m)) != 3 {
		t.Error("the hourly line is missing")
	}
}

func TestShadowOffAndDraft(t *testing.T) {
	t.Parallel()
	// merge=off: the verdict is kept for the UI, nothing is logged.
	m, gh, _, id := opened(t, Request{Reviewers: []string{"carol"}})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); h.Shadow == nil || shadowLines(t, m) != nil {
		t.Errorf("off: shadow %+v, lines %v", h.Shadow, shadowLines(t, m))
	}
	// A draft PR is not evaluated.
	m, gh, _, id = opened(t, Request{Autonomy: "merge=shadow"})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Draft: true, HeadSHA: "h", Author: "me"}
	m.tick(context.Background())
	if h := m.must(t, id); h.Shadow != nil || shadowLines(t, m) != nil {
		t.Errorf("draft: shadow %+v", h.Shadow)
	}
}

func TestShadowOutcome(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ state, outcome, by string }{{"MERGED", "merged", "carol"}, {"CLOSED", "closed", ""}} {
		t.Run(c.outcome, func(t *testing.T) {
			m, gh, _, tick := shadowTicks(t)
			tick()
			s := gh.Snapshots[prRef.Key()]
			s.State, s.MergedBy, s.HeadSHA = c.state, c.by, "h"
			s.MergedAt = m.now().Add(time.Minute)
			gh.Snapshots[prRef.Key()] = s
			tick()
			ls := shadowLines(t, m)
			if len(ls) != 2 {
				t.Fatalf("lines = %v", ls)
			}
			o := ls[1]
			if o["outcome"] != c.outcome || o["verdict_head_matches"] != true || o["last_verdict"] == nil || o["last_verdict_at"] == nil ||
				(o["merged_by"] != nil && o["merged_by"] != c.by) || (c.by != "" && o["merged_by"] != c.by) {
				t.Errorf("outcome = %v", o)
			}
		})
	}
	// A different head at merge: the verdict was about another commit.
	m, gh, _, tick := shadowTicks(t)
	tick()
	s := gh.Snapshots[prRef.Key()]
	s.State, s.HeadSHA = "MERGED", "other"
	gh.Snapshots[prRef.Key()] = s
	tick()
	if o := shadowLines(t, m)[1]; o["verdict_head_matches"] != false {
		t.Errorf("outcome = %v", o)
	}
}

func TestAgentCommitsOutlastTheEventCap(t *testing.T) {
	t.Parallel()
	var h Handoff
	logEvent(&h, time.Time{}, "commit", "pushed a")
	for range maxEvents {
		logEvent(&h, time.Time{}, "state", "x")
	}
	if g := evaluate(h, okSnap(), policyCfg(), time.Now()).Signals; g.AgentCommits != 1 {
		t.Errorf("AgentCommits = %d, want 1 though the event fell off the log", g.AgentCommits)
	}
}

func TestEvaluateAnsweredThreadsDontBlock(t *testing.T) {
	t.Parallel()
	s := okSnap()
	s.Unresolved, s.Unanswered = 2, 0
	if v := evaluate(Handoff{}, s, policyCfg(), time.Now()); !v.WouldMerge {
		t.Errorf("answered threads blocked: %v", v.Blockers)
	}
	s.Unanswered = 1
	if v := evaluate(Handoff{}, s, policyCfg(), time.Now()); !slices.Contains(v.Blockers, "1 unanswered threads") {
		t.Errorf("blockers = %v", v.Blockers)
	}
}

func TestEvaluateBehindWithoutMigrations(t *testing.T) {
	t.Parallel()
	has := func(s Snapshot, want string) bool {
		return slices.Contains(evaluate(Handoff{}, s, policyCfg(), time.Now()).Blockers, want)
	}
	s := okSnap()
	s.Behind, s.MergeState, s.Files = 3, "BEHIND", []string{"src/a.py"}
	if v := evaluate(Handoff{}, s, policyCfg(), time.Now()); !v.WouldMerge {
		t.Errorf("behind without migrations blocked: %v", v.Blockers)
	}
	s.Files = []string{"src/fides/api/alembic/versions/x.py"}
	if !has(s, "base not up to date (PR has migrations)") {
		t.Error("behind with migrations not blocked")
	}
	for _, f := range [][]string{{"src/a.py"}, {"src/fides/api/alembic/versions/x.py"}} {
		s.MergeState, s.Files = "DIRTY", f
		if !has(s, "conflicts with base") {
			t.Errorf("dirty with %v: no conflict blocker", f)
		}
	}
}

func TestEvaluateNoDiffSizeRule(t *testing.T) {
	t.Parallel()
	s := okSnap()
	s.Additions = 100000
	if v := evaluate(Handoff{}, s, policyCfg(), time.Now()); !v.WouldMerge {
		t.Errorf("size blocked: %v", v.Blockers)
	}
}
