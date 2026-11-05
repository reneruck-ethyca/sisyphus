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
		Draft: true, Notify: "true", NotifyOn: []string{EventPROpened}})
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
	m.tick(context.Background())
	if got = m.must(t, h.ID); len(gh.Created) != 1 || got.PRNumber != 9 || got.State != StatePROpen {
		t.Errorf("after restart: created %d, handoff = %+v", len(gh.Created), got)
	}
}

func TestOpenPRFailureStays(t *testing.T) {
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
		{"red with fix-ci", "fix-ci", github.PRSnapshot{Checks: []github.Check{chk("lint", "COMPLETED", "TIMED_OUT")}}, StateCIRed, "fix agent arrives in M1", false, nil},
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
	m, gh, events, id := opened(t, Request{Reviewers: []string{"carol"}})
	snap := github.PRSnapshot{State: "OPEN", HeadSHA: "h", Author: "me", Draft: true, Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	gh.Snapshots[prRef.Key()] = snap
	tick := func() Handoff { m.tick(context.Background()); return m.must(t, id) }

	// The handoff wasn't asked to be draft, so TLM makes the PR ready, asks the bot, then the humans at green.
	h := tick()
	if len(gh.Ready) != 1 || h.State != StateCIGreen || !h.BotRequested || !h.HumansRequested ||
		!reflect.DeepEqual(h.Requested, []string{"EthycaBot", "carol"}) {
		t.Fatalf("first green: ready %v, handoff %+v", gh.Ready, h)
	}
	if len(gh.Edits) != 2 || !reflect.DeepEqual(gh.Edits[0].AddReviewers, []string{"EthycaBot"}) || !reflect.DeepEqual(gh.Edits[1].AddReviewers, []string{"carol"}) {
		t.Errorf("edits = %+v", gh.Edits)
	}
	snap.Draft, snap.RequestedUsers = false, []string{"EthycaBot", "carol"}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateAwaitingReview || len(gh.Edits) != 2 || h.Snapshot == nil || h.Snapshot.CI != CIGreen {
		t.Errorf("awaiting: %+v edits %d", h, len(gh.Edits))
	}

	snap.Checks = []github.Check{chk("a", "COMPLETED", "FAILURE")}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateNeedsHuman {
		t.Errorf("red: %s", h.State)
	}
	// needs-human is sticky but still sees the merge.
	snap.Checks = []github.Check{chk("a", "COMPLETED", "SUCCESS")}
	gh.Snapshots[prRef.Key()] = snap
	if h = tick(); h.State != StateNeedsHuman || h.PrevState != StateAwaitingReview {
		t.Errorf("sticky: %+v", h)
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

func TestDraftHandoffStaysDraft(t *testing.T) {
	m, gh, _, id := opened(t, Request{Draft: true, Reviewers: []string{"carol"}})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Draft: true, Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); len(gh.Ready) != 0 || len(gh.Edits) != 0 || h.State != StateCIGreen || h.BotRequested {
		t.Errorf("ready %v edits %v handoff %+v", gh.Ready, gh.Edits, h)
	}
}

func TestNoChecksGrace(t *testing.T) {
	now := time.Now()
	if rollup(nil, now, now.Add(time.Minute)) != CIPending || rollup(nil, now, now.Add(noChecksGrace)) != CIGreen {
		t.Error("a PR without checks is pending only at first")
	}
}

func TestSnapshotApprovals(t *testing.T) {
	t0 := time.Now()
	s := buildSnapshot(github.PRSnapshot{HeadSHA: "new", Reviews: []github.SnapReview{
		{User: "a", State: "APPROVED", CommitSHA: "old", SubmittedAt: t0},
		{User: "b", State: "APPROVED", CommitSHA: "new", SubmittedAt: t0},
		{User: "b", State: "CHANGES_REQUESTED", CommitSHA: "new", SubmittedAt: t0.Add(time.Second)},
		{User: "c", State: "APPROVED", CommitSHA: "new", SubmittedAt: t0},
		{User: "c", State: "COMMENTED", CommitSHA: "new", SubmittedAt: t0.Add(time.Second)},
	}}, t0, t0)
	want := []Approval{{User: "a", CommitSHA: "old", Stale: true}, {User: "c", CommitSHA: "new"}}
	if !reflect.DeepEqual(s.Approvals, want) || !s.Approved() {
		t.Errorf("approvals = %+v", s.Approvals)
	}
	if (Snapshot{Approvals: want[:1]}).Approved() {
		t.Error("a stale approval doesn't count")
	}
}

func TestPick(t *testing.T) {
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
		got, next := pick(roster, c.count, c.cursor, c.author)
		if !slices.Equal(got, c.want) || next != c.next {
			t.Errorf("pick(%d,%d,%q) = %v,%d; want %v,%d", c.count, c.cursor, c.author, got, next, c.want, c.next)
		}
	}
}

func TestRosterRoundRobinPersists(t *testing.T) {
	m, gh, _, id := opened(t, Request{})
	m.cfg.LastMile.Reviewers.Roster = []string{"me", "bob", "carol"}
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); !reflect.DeepEqual(h.Requested, []string{"EthycaBot", "bob"}) {
		t.Fatalf("requested = %v", h.Requested)
	}
	again, err := New(m.cfg, gh, m.locks, filepath.Dir(m.path))
	if err != nil || again.cursors["ethyca/fidesplus"] != 2 {
		t.Errorf("cursor not persisted: %v %v", again.cursors, err)
	}
}

func TestNoReviewersWarns(t *testing.T) {
	m, gh, _, id := opened(t, Request{})
	gh.Snapshots[prRef.Key()] = github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	m.tick(context.Background())
	if h := m.must(t, id); h.Warning == "" || !h.HumansRequested || h.State != StateCIGreen {
		t.Errorf("handoff = %+v", h)
	}
}

func TestRemovedNeverReAdded(t *testing.T) {
	m, gh, _, id := opened(t, Request{Reviewers: []string{"alice", "bob"}})
	snap := github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")},
		RequestedUsers: []string{"EthycaBot", "alice", "bob", "dave"}} // dave: added by a human
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	// A human removes bob; alice has reviewed, which also takes her off the request list.
	snap.RequestedUsers = []string{"EthycaBot", "dave"}
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
	snap.RequestedUsers = []string{"EthycaBot", "dave", "alice", "erin"} // what GitHub shows after the edit
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
	m, gh, _, id := opened(t, Request{Reviewers: []string{"alice"}})
	if _, err := m.SetReviewers(id, []string{"bob", "bob"}); err != nil { // before the request: replaces the plan
		t.Fatal(err)
	}
	snap := github.PRSnapshot{State: "OPEN", Author: "me", Checks: []github.Check{chk("a", "COMPLETED", "SUCCESS")}}
	gh.Snapshots[prRef.Key()] = snap
	m.tick(context.Background())
	if h := m.must(t, id); !reflect.DeepEqual(h.Requested, []string{"EthycaBot", "bob"}) || h.ReviewersPending {
		t.Fatalf("handoff = %+v", h)
	}

	snap.RequestedUsers = []string{"EthycaBot", "bob", "dave"}
	gh.Snapshots[prRef.Key()] = snap
	if _, err := m.SetReviewers(id, []string{"carol"}); err != nil { // after: the diff is applied
		t.Fatal(err)
	}
	edits := len(gh.Edits)
	m.tick(context.Background())
	h := m.must(t, id)
	if len(gh.Edits) != edits+1 || !reflect.DeepEqual(gh.Edits[edits], github.PREdit{AddReviewers: []string{"carol"}, RemoveReviewers: []string{"bob"}}) ||
		!reflect.DeepEqual(h.Requested, []string{"EthycaBot", "carol"}) || h.ReviewersPending {
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
	m, _, _, id := opened(t, Request{})
	before := m.must(t, id)
	after := before
	after.State = StateCIRunning
	if _, err := m.SetReviewers(id, []string{"zed"}); err != nil {
		t.Fatal(err)
	}
	m.commit(before, after)
	if h := m.must(t, id); h.State != StateCIRunning || !reflect.DeepEqual(h.Reviewers, []string{"zed"}) {
		t.Errorf("handoff = %+v", h)
	}
}
