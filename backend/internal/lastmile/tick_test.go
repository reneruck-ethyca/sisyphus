package lastmile

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
)

var prRef = github.PRRef{Repo: "ethyca/fidesplus", Number: 7}

func chk(name, status, conclusion string) github.Check {
	return github.Check{Name: name, Status: status, Conclusion: conclusion}
}

// opened hands the clone off and ticks once, so its PR is open (#7, state pr-open).
func opened(t *testing.T, req Request) (*Manager, *FakeGitHub, *[]string, string) {
	t.Helper()
	m, clone, _ := fixture(t)
	gh := m.GH.(*FakeGitHub)
	gh.NextPR = prRef
	gh.Snapshots = map[string]github.PRSnapshot{}
	var events []string
	m.runHook = func(_ context.Context, _ string, _ time.Duration, _ string, env map[string]string) error {
		events = append(events, env["TLM_EVENT"])
		return nil
	}
	req.Worktree, req.Base = clone, "main"
	if req.Notify == "" {
		req.Notify, req.NotifyOn = "true", []string{EventPROpened, EventMerged, EventNeedsHuman, EventClosed}
	}
	h, err := m.Handoff(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	return m, gh, &events, h.ID
}

func (m *Manager) must(t *testing.T, id string) Handoff {
	t.Helper()
	h, ok := m.Get(id)
	if !ok {
		t.Fatal("no handoff")
	}
	return h
}

func TestOpenPR(t *testing.T) {
	t.Parallel()
	m, clone, _ := fixture(t)
	gh := m.GH.(*FakeGitHub)
	gh.NextPR = prRef
	var events []string
	m.runHook = func(_ context.Context, _ string, _ time.Duration, _ string, env map[string]string) error {
		events = append(events, env["TLM_EVENT"])
		return nil
	}
	os.MkdirAll(filepath.Join(clone, ".github"), 0o755)
	os.WriteFile(filepath.Join(clone, ".github", "pull_request_template.md"), []byte("## Steps\n"), 0o644)
	gittest.Git(t, clone, "add", ".github")
	gittest.Git(t, clone, "commit", "-qm", "template")
	h, err := m.Handoff(context.Background(), Request{Worktree: clone, Base: "main", Jira: "ENG-12", Title: "Do it", Labels: []string{"a"},
		Notify: "true", NotifyOn: []string{EventPROpened}})
	if err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	got := m.must(t, h.ID)
	if len(gh.Created) != 1 {
		t.Fatalf("created %d PRs", len(gh.Created))
	}
	c := gh.Created[0]
	if c.Title != "ENG-12: Do it" || c.Body != "## Steps" || !c.Draft || c.Head != "feature/x" || c.Base != "main" || !reflect.DeepEqual(c.Labels, []string{"a"}) {
		t.Errorf("created = %+v", c)
	}
	if got.State != StatePROpen || got.PRNumber != 7 || got.PRURL != "https://github.com/ethyca/fidesplus/pull/7" || got.LastBodyWritten != "## Steps" ||
		!reflect.DeepEqual(events, []string{EventPROpened}) {
		t.Errorf("handoff = %+v events = %v", got, events)
	}
	if prTitle("ENG-12 already", "ENG-12") != "ENG-12 already" || prTitle("x", "") != "x" {
		t.Error("title prefix")
	}

	// A crash between push and create (or after create, before the save): the PR is adopted.
	m.mu.Lock()
	m.handoffs[0].PRNumber, m.handoffs[0].State = 0, StateHandedOff
	m.mu.Unlock()
	gh.Existing = github.PRRef{Repo: "ethyca/fidesplus", Number: 9}
	m.cfg.User = "Me"
	gh.InboxData = github.Inbox{Threads: []github.ReviewThread{
		{ID: "T1", Comments: []github.InboxComment{cm(3, "alice", "a"), cm(8, "me", "b")}},
		{ID: "T2", Comments: []github.InboxComment{cm(9, "me", "f"), cm(10, "alice", "g")}},
		{ID: "T3", Resolved: true, Comments: []github.InboxComment{cm(11, "alice", "h")}},
		{ID: "T4", Outdated: true, Comments: []github.InboxComment{cm(12, "alice", "i")}},
	}, Reviews: []github.InboxComment{cm(4, "bob", "c")}, Issue: []github.InboxComment{cm(2, "carol", "d"), cm(6, "carol", "e")}}
	m.tick(context.Background())
	if got = m.must(t, h.ID); len(gh.Created) != 1 || got.PRNumber != 9 || got.State != StatePROpen {
		t.Errorf("after restart: created %d, handoff = %+v", len(gh.Created), got)
	}
	// Of what was said before the adoption, only an open, current thread waiting on me is feedback.
	if want := (Cursor{Threads: map[string]int64{"T1": 8, "T3": 11, "T4": 12}, Reviews: 4, Comments: 6}); !reflect.DeepEqual(got.Cursor, want) {
		t.Errorf("cursor = %+v, want %+v", got.Cursor, want)
	}
	if box := buildInbox(gh.InboxData, got, nil, ""); len(box.New) != 1 || box.New[0].ID != "T2" {
		t.Errorf("new after adoption = %+v", box.New)
	}
}

func TestOpenPRFailureStays(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	m.mu.Lock()
	m.handoffs[0].PRNumber, m.handoffs[0].State = 0, StateHandedOff
	m.mu.Unlock()
	gh.Err = errors.New("gh down")
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateHandedOff || h.Error == "" {
		t.Errorf("handoff = %+v", h)
	}
}

func TestStateMachine(t *testing.T) {
	t.Parallel()
	green := []github.Check{chk("a", "COMPLETED", "SUCCESS"), chk("b", "COMPLETED", "SKIPPED"), chk("c", "COMPLETED", "NEUTRAL")}
	cases := []struct {
		name      string
		autonomy  string
		snap      github.PRSnapshot
		wantState string
		reason    string
		label     bool
		events    []string
	}{
		{"pending", "", github.PRSnapshot{Checks: append([]github.Check{chk("p", "IN_PROGRESS", "")}, green...)}, StateCIRunning, "", false, nil},
		{"red outranks pending", "", github.PRSnapshot{Checks: []github.Check{chk("p", "IN_PROGRESS", ""), chk("lint", "COMPLETED", "FAILURE")}}, StateNeedsHuman, "CI failed: lint", true, []string{EventNeedsHuman}},
		{"red with fix-ci", "fix-ci", github.PRSnapshot{Checks: []github.Check{chk("lint", "COMPLETED", "TIMED_OUT")}}, StateNeedsHuman, "agents need reviews enabled", true, []string{EventNeedsHuman}},
		{"green", "", github.PRSnapshot{Checks: green}, StateCIGreen, "", false, nil},
		{"approved", "", github.PRSnapshot{Checks: green, ReviewDecision: "APPROVED"}, StateApproved, "", false, nil},
		{"merged", "", github.PRSnapshot{State: "MERGED", Checks: green}, StateMerged, "", false, []string{EventMerged}},
		{"closed", "", github.PRSnapshot{State: "CLOSED"}, StateClosed, "", false, []string{EventClosed}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, gh, events, id := opened(t, Request{Autonomy: c.autonomy})
			*events = nil
			if c.snap.State == "" {
				c.snap.State = "OPEN"
			}
			c.snap.HeadSHA, c.snap.Author = "h", "me"
			gh.Snapshots[prRef.Key()] = c.snap
			m.tick(context.Background())
			h := m.must(t, id)
			if h.State != c.wantState || h.Reason != c.reason || !slices.Equal(*events, c.events) {
				t.Errorf("state = %s (%q), events %v; want %s (%q), %v", h.State, h.Reason, *events, c.wantState, c.reason, c.events)
			}
			if c.label != (len(gh.Labels) == 1 && gh.Labels[0] == "tlm:needs-human") {
				t.Errorf("labels = %v, want label %v", gh.Labels, c.label)
			}
			if c.wantState == StateNeedsHuman && h.PrevState != StatePROpen {
				t.Errorf("prev = %q", h.PrevState)
			}
		})
	}
}

func TestLifecycle(t *testing.T) {
	t.Parallel()
	m, gh, events, id := opened(t, Request{Reviewers: []string{"carol"}})
	snap := github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Draft: true, Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	gh.Snapshots[prRef.Key()] = snap
	tick := func() Handoff { m.tick(context.Background()); return m.must(t, id) }

	// A draft gets the bot first; the humans only once it is ready.
	h := tick()
	if len(gh.Ready) != 0 || h.State != StateCIGreen || !h.BotRequested || h.HumansRequested || h.BotAsked.IsZero() {
		t.Fatalf("draft: ready %v, handoff %+v", gh.Ready, h)
	}
	// The bot reviewed the head and CI is green: ready, and the humans are asked.
	snap.Reviews = []github.SnapReview{{User: "ethycabot", State: "COMMENTED", CommitSHA: "h"}}
	gh.Snapshots[prRef.Key()] = snap
	h = tick()
	if len(gh.Ready) != 1 || !h.HumansRequested || !h.BotAsked.IsZero() || !reflect.DeepEqual(h.Requested, []string{"ethycabot", "carol"}) {
		t.Fatalf("ready: ready %v, handoff %+v", gh.Ready, h)
	}
	if len(gh.Edits) != 2 || !reflect.DeepEqual(gh.Edits[0].AddReviewers, []string{"ethycabot"}) || !reflect.DeepEqual(gh.Edits[1].AddReviewers, []string{"carol"}) {
		t.Errorf("edits = %+v", gh.Edits)
	}
	snap.Draft, snap.RequestedUsers = false, []string{"ethycabot", "carol"}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateAwaitingReview || len(gh.Edits) != 2 || h.Snapshot == nil || h.Snapshot.CI != CIGreen {
		t.Errorf("awaiting: %+v edits %d", h, len(gh.Edits))
	}

	snap.Checks = []github.Check{chk("a", "COMPLETED", "FAILURE")}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateNeedsHuman {
		t.Errorf("red: %s", h.State)
	}
	// CI sent it to needs-human, so green CI brings it back.
	snap.Checks = []github.Check{chk("a", "COMPLETED", "SUCCESS")}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StatePROpen || h.EscalatedBy != "" || !strings.Contains(eventKinds(h), "resumed") {
		t.Errorf("back from CI: %+v", h)
	}
	snap.State = "MERGED"
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateMerged || (*events)[len(*events)-1] != EventMerged {
		t.Errorf("merged: %+v %v", h, *events)
	}
	if _, err := m.Handoff(context.Background(), Request{Worktree: h.Worktree, Base: "main"}); err != nil {
		t.Errorf("branch not released: %v", err)
	}
}

func TestNeedsHumanStaysUnlessCISentIt(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.set(t, id, func(h *Handoff) { h.State, h.EscalatedBy, h.Reason = StateNeedsHuman, TriggerReview, "review feedback" })
	m.tick(context.Background())
	if h := m.must(t, id); h.State != StateNeedsHuman || h.Snapshot == nil || h.Snapshot.CI != CIGreen {
		t.Errorf("review escalation resumed on green: %+v", h)
	}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "MERGED", HeadSHA: "h", Author: "me"}
	if m.tick(context.Background()); m.must(t, id).State != StateMerged {
		t.Error("needs-human doesn't see the merge")
	}
}

func TestCancelledChecksAreRerun(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Autonomy: "fix-ci"})
	m.cfg.LastMile.FlakeReruns = 1
	cancelled := chk("a", "COMPLETED", "CANCELLED")
	cancelled.URL = "https://github.com/ethyca/fidesplus/actions/runs/5/job/6"
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{cancelled}}
	m.tick(context.Background())
	if h := m.must(t, id); !slices.Equal(gh.Reruns, []int64{5}) || h.State != StateCIRunning {
		t.Errorf("reruns %v, handoff %+v", gh.Reruns, h)
	}
}

func TestDraftWaitsForBot(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Reviewers: []string{"carol"}})
	snap := github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Draft: true, Checks: []github.Check{chk("a", "IN_PROGRESS", "")}}
	gh.Snapshots[prRef.Key()] = snap
	tick := func() Handoff { m.tick(context.Background()); return m.must(t, id) }
	asked := func() int {
		n := 0
		for _, e := range gh.Edits {
			if slices.Contains(e.AddReviewers, "ethycabot") {
				n++
			}
		}
		return n
	}
	tick()
	// Reviewed while CI runs: not ready yet.
	snap.Reviews = []github.SnapReview{{User: "ethycabot", State: "COMMENTED", CommitSHA: "h"}}
	gh.Snapshots[prRef.Key()] = snap
	if h := tick(); len(gh.Ready) != 0 || !h.BotAsked.IsZero() || asked() != 1 {
		t.Fatalf("pending: ready %v asked %d %+v", gh.Ready, asked(), h)
	}
	// A new head the bot hasn't seen is put to it again, once.
	snap.HeadSHA = "h2"
	gh.Snapshots[prRef.Key()] = snap
	tick()
	if h := tick(); asked() != 2 || h.BotAsked.IsZero() || len(gh.Ready) != 0 || !strings.Contains(eventKinds(h), "reviewer") {
		t.Fatalf("new head: asked %d %+v", asked(), h)
	}
	// Green, but the bot is quiet: ready once botWait has passed, not before.
	snap.Checks = []github.Check{chk("a", "COMPLETED", "SUCCESS")}
	gh.Snapshots[prRef.Key()] = snap
	if tick(); len(gh.Ready) != 0 {
		t.Fatal("ready before the bot had its say")
	}
	m.now = func() time.Time { return time.Now().Add(botWait) }
	h := tick()
	if len(gh.Ready) != 1 || !h.HumansRequested || !strings.Contains(h.Events[len(h.Events)-1].Text+h.Events[len(h.Events)-2].Text, "no review from ethycabot") {
		t.Errorf("quiet bot: ready %v %+v", gh.Ready, h.Events)
	}
}

func TestDraftGoesReady(t *testing.T) {
	t.Parallel()
	green := github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Draft: true, Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	cases := []struct {
		name  string
		setup func(m *Manager, h *Handoff)
		ticks int
	}{
		{"no bot reviewer", func(m *Manager, h *Handoff) { m.cfg.LastMile.BotReviewer = "" }, 1},
		{"bot rounds used up", func(m *Manager, h *Handoff) { h.BotRounds = maxBotRounds }, 2}, // the first tick asks the bot
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, gh, _, id := opened(t, Request{Reviewers: []string{"carol"}})
			m.set(t, id, func(h *Handoff) { c.setup(m, h) })
			gh.Snapshots[prRef.Key()] = green
			for range c.ticks {
				m.tick(context.Background())
			}
			if h := m.must(t, id); len(gh.Ready) != 1 || !h.HumansRequested {
				t.Errorf("ready %v, handoff %+v", gh.Ready, h)
			}
		})
	}
}

func TestReadyPRGetsHumansAtOnce(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Reviewers: []string{"carol"}})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Checks: []github.Check{chk("a", "IN_PROGRESS", "")}}
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.Ready) != 0 || !reflect.DeepEqual(h.Requested, []string{"ethycabot", "carol"}) || h.State != StateCIRunning {
		t.Errorf("ready %v, handoff %+v", gh.Ready, h)
	}
}

func TestNoChecksGrace(t *testing.T) {
	t.Parallel()
	now := time.Now()
	if rollup(nil, now, now.Add(time.Minute)) != CIPending || rollup(nil, now, now.Add(noChecksGrace)) != CIGreen {
		t.Error("a PR without checks is pending only at first")
	}
}

func TestRollupCountsRequiredChecks(t *testing.T) {
	t.Parallel()
	now := time.Now()
	req := func(c github.Check) github.Check { c.Required = true; return c }
	optionalRunning := []github.Check{req(chk("summary", "COMPLETED", "SUCCESS")), chk("job", "IN_PROGRESS", "")}
	if got := rollup(optionalRunning, now, now); got != CIGreen {
		t.Errorf("optional check still running: %s, want green", got)
	}
	if got := rollup([]github.Check{req(chk("summary", "COMPLETED", "FAILURE")), chk("job", "COMPLETED", "SUCCESS")}, now, now); got != CIRed {
		t.Errorf("required check failed: %s", got)
	}
	if got := rollup([]github.Check{chk("job", "IN_PROGRESS", "")}, now, now); got != CIPending {
		t.Errorf("nothing required: every check counts, got %s", got)
	}
}

func TestSnapshotApprovals(t *testing.T) {
	t.Parallel()
	t0 := time.Now()
	s := buildSnapshot(github.PRSnapshot{HeadSHA: "new", Reviews: []github.SnapReview{
		{User: "a", State: "APPROVED", CommitSHA: "old", SubmittedAt: t0},
		{User: "b", State: "APPROVED", CommitSHA: "new", SubmittedAt: t0},
		{User: "b", State: "CHANGES_REQUESTED", CommitSHA: "new", SubmittedAt: t0.Add(time.Second)},
		{User: "c", State: "APPROVED", CommitSHA: "new", SubmittedAt: t0},
		{User: "c", State: "COMMENTED", CommitSHA: "new", SubmittedAt: t0.Add(time.Second)},
	}}, t0, t0, nil)
	want := []Approval{{User: "a", CommitSHA: "old", Stale: true}, {User: "c", CommitSHA: "new"}}
	if !reflect.DeepEqual(s.Approvals, want) || !s.Approved() {
		t.Errorf("approvals = %+v", s.Approvals)
	}
	if (Snapshot{Approvals: want[:1]}).Approved() {
		t.Error("a stale approval doesn't count")
	}
}

func TestPick(t *testing.T) {
	t.Parallel()
	roster := []string{"alice", "bob", "carol"}
	cases := []struct {
		count, cursor int
		author        string
		want          []string
		next          int
	}{
		{1, 0, "", []string{"alice"}, 1},
		{1, 2, "", []string{"carol"}, 0},
		{1, 0, "alice", []string{"bob"}, 2},
		{2, 1, "carol", []string{"bob", "alice"}, 1},
		{5, 0, "bob", []string{"alice", "carol"}, 0},
	}
	for _, c := range cases {
		got, next := pick(roster, c.count, c.cursor, []string{c.author})
		if !slices.Equal(got, c.want) || next != c.next {
			t.Errorf("pick(%d,%d,%q) = %v,%d; want %v,%d", c.count, c.cursor, c.author, got, next, c.want, c.next)
		}
	}
}

func TestRosterRoundRobinPersists(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	m.cfg.LastMile.Reviewers.Roster = []string{"me", "bob", "carol"}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); !reflect.DeepEqual(h.Requested, []string{"ethycabot", "bob"}) {
		t.Fatalf("requested = %v", h.Requested)
	}
	again, err := New(m.cfg, gh, m.locks, filepath.Dir(m.path))
	if err != nil || again.cursors["ethyca/fidesplus"] != 2 {
		t.Errorf("cursor not persisted: %v %v", again.cursors, err)
	}
}

func TestRosterCountsReviewersAlreadyOnPR(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	m.cfg.LastMile.Reviewers.Roster = []string{"bob", "carol", "dave"}
	m.cfg.LastMile.Reviewers.Count = 2
	snap := github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")},
		RequestedUsers: []string{"ethycabot", "bob"}}
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	if h := m.must(t, id); !reflect.DeepEqual(h.Requested, []string{"ethycabot", "carol"}) || h.Warning != "" {
		t.Fatalf("requested = %v, warning %q", h.Requested, h.Warning)
	}

	// Enough people already: nobody is added and there is no warning.
	m, gh, _, id = opened(t, Request{})
	m.cfg.LastMile.Reviewers.Roster = []string{"bob", "carol", "dave"}
	snap.Reviews = []github.SnapReview{{User: "erin", State: "APPROVED"}}
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	if h := m.must(t, id); !h.HumansRequested || h.Warning != "" || slices.ContainsFunc(gh.Edits, func(e github.PREdit) bool {
		return slices.ContainsFunc(e.AddReviewers, func(u string) bool { return u != "ethycabot" })
	}) {
		t.Errorf("handoff = %+v, edits %+v", h, gh.Edits)
	}
}

func TestNoReviewersWarns(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); h.Warning == "" || !h.HumansRequested || h.State != StateCIGreen {
		t.Errorf("handoff = %+v", h)
	}
}

func TestRemovedNeverReAdded(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Reviewers: []string{"alice", "bob"}})
	snap := github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")},
		RequestedUsers: []string{"ethycabot", "alice", "bob", "dave"}} // dave: added by a human
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	// A human removes bob; alice has reviewed, which also takes her off the request list.
	snap.RequestedUsers = []string{"ethycabot", "dave"}
	snap.Reviews = []github.SnapReview{{User: "alice", State: "COMMENTED"}}
	gh.Snapshots[prRef.Key()] = snap
	edits := len(gh.Edits)
	m.tick(context.Background())
	m.tick(context.Background())
	h := m.must(t, id)
	if !reflect.DeepEqual(h.Removed, []string{"bob"}) || len(gh.Edits) != edits {
		t.Fatalf("removed %v, edits %+v", h.Removed, gh.Edits[edits:])
	}

	// An edit that keeps bob listed doesn't bring him back; naming him anew does.
	if _, err := m.SetReviewers(id, []string{"alice", "bob", "erin"}); err != nil {
		t.Fatal(err)
	}
	m.tick(context.Background())
	if len(gh.Edits) != edits+1 || !reflect.DeepEqual(gh.Edits[edits], github.PREdit{AddReviewers: []string{"alice", "erin"}}) {
		t.Errorf("edits %+v", gh.Edits[edits:])
	}
	snap.RequestedUsers = []string{"ethycabot", "dave", "alice", "erin"} // what GitHub shows after the edit
	gh.Snapshots[prRef.Key()] = snap
	for _, list := range [][]string{{"alice", "erin"}, {"alice", "erin", "bob"}} { // drop him from the list, then name him anew
		if _, err := m.SetReviewers(id, list); err != nil {
			t.Fatal(err)
		}
	}
	m.tick(context.Background())
	if last := gh.Edits[len(gh.Edits)-1]; !slices.Equal(last.AddReviewers, []string{"bob"}) {
		t.Errorf("last edit = %+v", last)
	}
}

func TestReviewerOverrideDiff(t *testing.T) {
	t.Parallel()
	m, gh, _, id := opened(t, Request{Reviewers: []string{"alice"}})
	if _, err := m.SetReviewers(id, []string{"bob", "bob"}); err != nil { // before the request: replaces the plan
		t.Fatal(err)
	}
	snap := github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	if h := m.must(t, id); !reflect.DeepEqual(h.Requested, []string{"ethycabot", "bob"}) || h.ReviewersPending {
		t.Fatalf("handoff = %+v", h)
	}

	snap.RequestedUsers = []string{"ethycabot", "bob", "dave"}
	gh.Snapshots[prRef.Key()] = snap
	if _, err := m.SetReviewers(id, []string{"carol"}); err != nil { // after: the diff is applied
		t.Fatal(err)
	}
	edits := len(gh.Edits)
	m.tick(context.Background())
	h := m.must(t, id)
	if len(gh.Edits) != edits+1 || !reflect.DeepEqual(gh.Edits[edits], github.PREdit{AddReviewers: []string{"carol"}, RemoveReviewers: []string{"bob"}}) ||
		!reflect.DeepEqual(h.Requested, []string{"ethycabot", "carol"}) || h.ReviewersPending {
		t.Errorf("edits %+v, handoff %+v", gh.Edits[edits:], h)
	}

	if _, err := m.SetReviewers(id, []string{"bad login"}); err == nil || !strings.Contains(err.Error(), "GitHub login") {
		t.Errorf("err = %v", err)
	}
	if _, err := m.SetReviewers("nope", nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestCommitKeepsConcurrentReviewerEdit(t *testing.T) {
	t.Parallel()
	m, _, _, id := opened(t, Request{})
	before := m.must(t, id)
	after := before
	after.State = StateCIRunning
	if _, err := m.SetReviewers(id, []string{"zed"}); err != nil {
		t.Fatal(err)
	}
	m.commit(context.Background(), before, after)
	if h := m.must(t, id); h.State != StateCIRunning || !reflect.DeepEqual(h.Reviewers, []string{"zed"}) {
		t.Errorf("handoff = %+v", h)
	}
}

func TestCommitKeepsConcurrentReAdd(t *testing.T) {
	t.Parallel()
	m, _, _, id := opened(t, Request{})
	if _, err := m.update(id, func(h *Handoff) error {
		h.Requested, h.Removed, h.HumansRequested = []string{"alice"}, []string{"alice"}, true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before := m.must(t, id)
	if _, err := m.SetReviewers(id, []string{"alice"}); err != nil { // asked for afresh mid-tick
		t.Fatal(err)
	}
	m.commit(context.Background(), before, before)
	if h := m.must(t, id); slices.Contains(h.Requested, "alice") || slices.Contains(h.Removed, "alice") {
		t.Errorf("stale tick kept alice as requested before: requested %v, removed %v", h.Requested, h.Removed)
	}
}

func TestCommitKeepsRunMarkedMidTick(t *testing.T) {
	t.Parallel()
	m, _, _, id := opened(t, Request{})
	before := m.must(t, id)
	m.markRun(id, &RunMark{Trigger: TriggerDirty}) // DirtyAgent launched while the tick ran
	m.commit(context.Background(), before, before)
	if m.must(t, id).Run == nil {
		t.Error("a stale tick dropped the run mark")
	}
}

func TestTickWaitsForGitHubToSeeAPush(t *testing.T) {
	t.Parallel()
	tasks := &fakeTasks{result: okFix}
	m, gh, id, head := redFixture(t, tasks)
	wt := m.must(t, id).Worktree
	os.WriteFile(filepath.Join(wt, "pushed.txt"), []byte("x"), 0o644)
	gittest.Git(t, wt, "add", "pushed.txt")
	gittest.Git(t, wt, "commit", "-qm", "pushed by a human")
	gittest.Git(t, wt, "push", "-q")
	m.tickWait(t)
	m.tickWait(t)
	if h := m.must(t, id); h.State == StateNeedsHuman || tasks.n() != 0 {
		t.Fatalf("acted on GitHub's stale head %.8s: %s %q, %d runs", head, h.State, h.Reason, tasks.n())
	}
	snap := gh.Snapshots[prRef.Key()]
	snap.HeadSHA = gittest.Git(t, wt, "rev-parse", "HEAD")
	gh.Snapshots[prRef.Key()] = snap
	m.tickWait(t)
	if tasks.n() != 1 {
		t.Fatalf("no fix run once GitHub caught up: %d", tasks.n())
	}
}
