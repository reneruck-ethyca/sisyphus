package tracker

import (
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/github"
)

func at(m int) time.Time { return time.Date(2026, 10, 20, 12, m, 0, 0, time.UTC) }

func rv(user, state string, m int) github.Review {
	return github.Review{User: user, State: state, SubmittedAt: at(m)}
}

func TestDropReason(t *testing.T) {
	open := func(rs ...github.Review) github.PR { return github.PR{State: "open", Reviews: rs} }
	bot := rv("ci", "APPROVED", 1)
	bot.Bot = true
	cases := []struct {
		name         string
		pr           github.PR
		cand, drafts bool
		want         string
	}{
		{"open candidate", open(), true, false, ""},
		{"merged", github.PR{State: "closed", Merged: true}, true, false, "merged"},
		{"closed", github.PR{State: "closed"}, true, false, "closed"},
		{"approved by me", open(rv("me", "APPROVED", 1)), true, false, "approved by you"},
		{"approved then commented", open(rv("me", "APPROVED", 1), rv("me", "COMMENTED", 2)), true, false, "approved by you"},
		{"other approved", open(rv("alice", "APPROVED", 1)), true, false, "approved by alice"},
		{"other approved, I requested changes", open(rv("me", "CHANGES_REQUESTED", 1), rv("alice", "APPROVED", 2)), true, false, ""},
		{"other approval dismissed", open(rv("alice", "APPROVED", 1), rv("alice", "DISMISSED", 2)), true, false, ""},
		{"bot approval ignored", open(bot), true, false, ""},
		{"draft excluded", github.PR{State: "open", Draft: true}, true, false, "draft"},
		{"draft included", github.PR{State: "open", Draft: true}, true, true, ""},
		{"not requested", open(), false, false, "review not requested from you"},
	}
	for _, c := range cases {
		if got := dropReason(c.pr, "me", c.cand, c.drafts); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

func TestNewReplies(t *testing.T) {
	nit := rv("bob", "COMMENTED", 4)
	nit.Body = "nit"
	pr := github.PR{
		Comments: []github.Comment{{User: "alice", CreatedAt: at(1)}, {User: "alice", CreatedAt: at(5)},
			{User: "me", CreatedAt: at(6)}, {User: "ci", Bot: true, CreatedAt: at(7)}},
		Reviews: []github.Review{nit, rv("bob", "APPROVED", 8)},
	}
	check := func(since time.Time, wantN int, wantLatest time.Time) {
		t.Helper()
		n, latest := newReplies(pr, "me", since)
		if n != wantN || !latest.Equal(wantLatest) {
			t.Errorf("since %v: got (%d, %v) want (%d, %v)", since, n, latest, wantN, wantLatest)
		}
	}
	check(at(3), 2, at(8))
	check(at(8), 0, at(8))
	check(time.Time{}, 3, at(8))
}

// Review #3: search lag must not drop a PR the payload still shows as ours.
func TestRequested(t *testing.T) {
	teams := []string{"eng"}
	for _, c := range []struct {
		name string
		pr   github.PR
		want bool
	}{
		{"requested from me", github.PR{RequestedReviewers: []string{"alice", "me"}}, true},
		{"requested from my team", github.PR{RequestedTeams: []string{"eng"}}, true},
		{"requested from another team", github.PR{RequestedTeams: []string{"fides-security-team"}}, false},
		{"only reviewed by me", github.PR{Reviews: []github.Review{rv("me", "CHANGES_REQUESTED", 1)}}, false},
		{"someone else", github.PR{RequestedReviewers: []string{"alice"}}, false},
	} {
		if got := requested(c.pr, "me", teams); got != c.want {
			t.Errorf("%s: requested = %v", c.name, got)
		}
	}
}
