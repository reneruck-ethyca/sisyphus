package review

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/config"
)

func TestParseRateLimit(t *testing.T) {
	evs, _, w := parseLine(3, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1793118000,"rateLimitType":"five_hour",`+
		`"unifiedWindows":{"five_hour":{"utilization":0.22,"resetsAt":1793118000},"seven_day":{"utilization":0.03,"resetsAt":1793674800}}}}`)
	if w == nil || w.Used != 0.22 || !w.ResetsAt.Equal(time.Unix(1793118000, 0)) || len(evs) != 1 || evs[0].Kind != "system" {
		t.Fatalf("window %+v, events %+v", w, evs)
	}
	if _, _, w := parseLine(0, `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`); w != nil {
		t.Fatalf("no five_hour window, got %+v", w)
	}
}

func TestWindowPlausible(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		w    Window
		want bool
	}{
		{Window{now.Add(time.Hour), 0.22}, true},
		{Window{now.Add(5 * time.Hour), 1}, true},
		{Window{now.Add(-time.Second), 0.2}, false},     // already reset
		{Window{now.Add(1000 * time.Hour), 0.2}, false}, // would hold reviews back for weeks
		{Window{now.Add(time.Hour), 1.5}, false},
		{Window{now.Add(time.Hour), -0.1}, false},
	} {
		if got := c.w.plausible(now); got != c.want {
			t.Errorf("%+v: plausible = %v", c.w, got)
		}
	}
}

func TestBudget(t *testing.T) {
	now := time.Now()
	cur, old := now.Add(time.Hour), now.Add(-time.Hour)
	spans := func(readings ...Window) []Span {
		var s []Span
		for _, w := range readings {
			s = observe(s, w)
		}
		return s
	}
	runs := map[string][]Run{
		"o/r#1": {
			{Usage: spans(Window{old, 0.5}, Window{old, 0.9}, Window{cur, 0.00}, Window{cur, 0.04})},   // crossed into the current window
			{Usage: spans(Window{cur, 0.20}, Window{cur, 0.21}, Window{cur, 0.20}, Window{cur, 0.22})}, // out-of-order readings
		},
		"o/r#2": {
			{Usage: spans(Window{cur, 0.21}, Window{cur, 0.25})}, // overlapped the run above: 0.20–0.25 counts once
			{Usage: spans(Window{cur, 0.30}, Window{cur, 0.30})}, // saw no rise
		},
	}
	b := budget(runs, 0.09, now)
	if math.Abs(b.Used-0.09) > 1e-9 || b.Account != 0.30 || !b.ResetsAt.Equal(cur) || !b.Exhausted() {
		t.Fatalf("budget = %+v, want used 0.09 (0.04 + 0.05), account 0.30, exhausted", b)
	}
	if b := budget(runs, 0.1, cur.Add(time.Second)); b.Used != 0 || b.Exhausted() {
		t.Fatalf("after the reset: %+v", b)
	}
	if b := budget(runs, 0, now); b.Exhausted() {
		t.Fatal("share 0 means no limit")
	}
}

func TestBudgetHoldsReviewsUntilReset(t *testing.T) {
	f := newFixture(t, func(c *config.Review) { c.FiveHourPct = 5 })
	f.tr.AnnouncedSHA = f.tr.HeadSHA
	f.start(t)
	f.r.Tracked(f.tr)
	waitFor(t, "container start", f.started(1))
	resets := time.Now().Add(2 * time.Second).Unix()
	reading := func(used float64) string {
		return fmt.Sprintf(`{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"five_hour":{"utilization":%g,"resetsAt":%d}}}}`+"\n", used, resets)
	}
	f.succeed(f.containers()[0].ID, reading(0.10)+reading(0.16))
	waitFor(t, "run to finish", f.lastStatus(Succeeded))
	if b := f.r.Budget(); math.Abs(b.Used-0.06) > 1e-9 || !b.Exhausted() {
		t.Fatalf("budget = %+v", b)
	}

	if err := f.r.Trigger(f.tr); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(f.rt.Started()); n != 1 || f.r.Status()[refA.Key()].State != "queued" {
		t.Fatalf("started %d containers past the budget", n)
	}
	waitFor(t, "start after the window resets", func() bool { return len(f.rt.Started()) == 2 })
}
