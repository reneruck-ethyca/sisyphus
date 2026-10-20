package main

import (
	"testing"
	"time"
)

func at(m int) time.Time { return time.Date(2026, 10, 20, 12, m, 0, 0, time.UTC) }

func rv(user, state string, m int) Review {
	return Review{User: user, State: state, SubmittedAt: at(m)}
}

func TestDropReason(t *testing.T) {
	open := func(rs ...Review) PR { return PR{State: "open", Reviews: rs} }
	bot := rv("ci", "APPROVED", 1)
	bot.Bot = true
	cases := []struct {
		name         string
		pr           PR
		cand, drafts bool
		want         string
	}{
		{"open candidate", open(), true, false, ""},
		{"merged", PR{State: "closed", Merged: true}, true, false, "merged"},
		{"closed", PR{State: "closed"}, true, false, "closed"},
		{"approved by me", open(rv("me", "APPROVED", 1)), true, false, "approved by you"},
		{"approved then commented", open(rv("me", "APPROVED", 1), rv("me", "COMMENTED", 2)), true, false, "approved by you"},
		{"other approved", open(rv("alice", "APPROVED", 1)), true, false, "approved by alice"},
		{"other approved, I requested changes", open(rv("me", "CHANGES_REQUESTED", 1), rv("alice", "APPROVED", 2)), true, false, ""},
		{"other approval dismissed", open(rv("alice", "APPROVED", 1), rv("alice", "DISMISSED", 2)), true, false, ""},
		{"bot approval ignored", open(bot), true, false, ""},
		{"draft excluded", PR{State: "open", Draft: true}, true, false, "draft"},
		{"draft included", PR{State: "open", Draft: true}, true, true, ""},
		{"no longer candidate", open(), false, false, "no longer requested"},
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
	pr := PR{
		Comments: []Comment{{User: "alice", CreatedAt: at(1)}, {User: "alice", CreatedAt: at(5)},
			{User: "me", CreatedAt: at(6)}, {User: "ci", Bot: true, CreatedAt: at(7)}},
		Reviews: []Review{nit, rv("bob", "APPROVED", 8)},
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
func TestStillMine(t *testing.T) {
	if !stillMine(PR{RequestedReviewers: []string{"me"}}, "me") {
		t.Error("requested reviewer")
	}
	if !stillMine(PR{Reviews: []Review{rv("me", "COMMENTED", 1)}}, "me") {
		t.Error("reviewed by me")
	}
	if stillMine(PR{RequestedReviewers: []string{"alice"}, Reviews: []Review{rv("alice", "COMMENTED", 1)}}, "me") {
		t.Error("not mine")
	}
}
