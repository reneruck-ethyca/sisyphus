package app

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

func TestVerdict(t *testing.T) {
	ran := func(status string, posted *review.Posted) review.PRStatus {
		return review.PRStatus{Runs: []review.Run{{Status: status, Posted: posted}}}
	}
	for _, c := range []struct {
		name, health string
		rs           review.PRStatus
		label, group string
	}{
		{"approved", "ok", ran(review.Succeeded, &review.Posted{Event: "APPROVE"}), "Approved", GroupReviewed},
		{"changes", "ok", ran(review.Succeeded, &review.Posted{Event: "REQUEST_CHANGES"}), "Changes requested", GroupReviewed},
		{"dry run", "ok", ran(review.Succeeded, nil), "Not posted", GroupReviewed},
		{"post failed", "ok", ran(review.Succeeded, &review.Posted{Event: "APPROVE", Error: "403"}), "Posting failed", GroupAttention},
		{"run failed", "ok", ran(review.Failed, nil), "Failed", GroupAttention},
		{"running", "ok", review.PRStatus{State: "running", Runs: []review.Run{{Status: review.Running}}}, "Reviewing", GroupReviewing},
		{"queued", "ok", review.PRStatus{State: "queued"}, "Queued", GroupReviewing},
		{"never", "ok", review.PRStatus{}, "Not reviewed", GroupWaiting},
		{"conflict beats a review", "conflict", ran(review.Succeeded, &review.Posted{Event: "APPROVE"}), "Merge conflict", GroupAttention},
	} {
		if label, _, group, _ := verdict(tracker.Tracked{}, c.health, c.rs); label != c.label || group != c.group {
			t.Errorf("%s: got %q/%q, want %q/%q", c.name, label, group, c.label, c.group)
		}
	}
}

func ids[T any](vs []T, id func(T) string) []string {
	out := []string{}
	for _, v := range vs {
		out = append(out, id(v))
	}
	return out
}

func prIDs(prs []PRView) []string { return ids(prs, func(p PRView) string { return p.ID }) }
func handoffIDs(vs []lastmile.View) []string {
	return ids(vs, func(v lastmile.View) string { return v.ID })
}

func TestBoardNeedsYou(t *testing.T) {
	now := time.Now()
	pr := func(n int, group, tone string) PRView {
		return PRView{ID: "o/r#" + strconv.Itoa(n), Group: group, Tone: tone, Verdict: "v" + strconv.Itoa(n), VerdictAt: now.Add(time.Duration(n) * time.Minute)}
	}
	b := Board{Status{
		PRs: []PRView{pr(1, GroupReviewed, ToneOK), pr(2, GroupReviewed, ToneNeutral), pr(3, GroupAttention, ToneBad), pr(4, GroupReviewed, ToneChanges)},
		Handoffs: []lastmile.View{
			{Handoff: lastmile.Handoff{ID: "a", State: lastmile.StateNeedsHuman}},
			{Handoff: lastmile.Handoff{ID: "b", State: lastmile.StateCIRed}},
			{Handoff: lastmile.Handoff{ID: "c", State: lastmile.StateMerged}, Over: true}},
	}}
	if got := prIDs(b.AwaitingYou()); !slices.Equal(got, []string{"o/r#4", "o/r#1"}) {
		t.Errorf("awaiting you = %v, want the approved and changes-requested ones, newest first", got)
	}
	if got := prIDs(b.Failing()); !slices.Equal(got, []string{"o/r#3"}) {
		t.Errorf("failing = %v", got)
	}
	if got := handoffIDs(b.NeedsHuman()); !slices.Equal(got, []string{"a"}) {
		t.Errorf("needs human = %v", got)
	}
	if b.NeedsCount() != 4 || len(b.Merged()) != 1 {
		t.Errorf("needs %d, merged %d", b.NeedsCount(), len(b.Merged()))
	}
}

func TestBoardGroups(t *testing.T) {
	b := Board{Status{PRs: []PRView{
		{ID: "1", Group: GroupWaiting}, {ID: "2", Group: GroupAttention}, {ID: "3", Group: GroupWaiting}, {ID: "4", Group: GroupReviewed}}}}
	var got []string
	for _, g := range b.Groups() {
		got = append(got, g.Label+":"+strconv.Itoa(len(g.PRs)))
	}
	if want := []string{"Needs you:1", "Reviewed:1", "Not reviewed yet:2"}; !slices.Equal(got, want) {
		t.Errorf("groups = %v, want %v (in order, empty ones left out)", got, want)
	}
}

func TestBoardHandoffs(t *testing.T) {
	now := time.Now()
	merged := func(id string, ago int) lastmile.View {
		return lastmile.View{Handoff: lastmile.Handoff{ID: id, State: lastmile.StateMerged, Updated: now.Add(-time.Duration(ago) * time.Minute)}, Over: true}
	}
	b := Board{Status{Handoffs: []lastmile.View{
		merged("m1", 4), merged("m2", 1), merged("m3", 3), merged("m4", 2),
		{Handoff: lastmile.Handoff{ID: "late"}, AgentRunning: lastmile.TriggerCI, AgentStarted: now},
		{Handoff: lastmile.Handoff{ID: "early"}, AgentRunning: lastmile.TriggerReview, AgentStarted: now.Add(-time.Hour)},
	}}}
	if got := handoffIDs(b.Merged()); !slices.Equal(got, []string{"m2", "m4", "m3"}) {
		t.Errorf("merged = %v, want the newest three", got)
	}
	if got := handoffIDs(b.AgentsRunning()); !slices.Equal(got, []string{"early", "late"}) {
		t.Errorf("agents running = %v, want oldest run first", got)
	}
}

func TestBoardQueued(t *testing.T) {
	b := Board{Status{
		PRs: []PRView{{Review: review.PRStatus{State: review.StateQueued}}, {Review: review.PRStatus{State: review.StateRunning}}, {Review: review.PRStatus{State: review.StateQueued}}},
	}}
	if b.Queued() != 2 || b.View().Queued != 2 {
		t.Errorf("queued %d", b.Queued())
	}
}

