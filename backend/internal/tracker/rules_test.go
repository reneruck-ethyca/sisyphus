package tracker

import (
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
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
		// Once I've reviewed, only merging or closing drops it.
		{"approved by me", open(rv("me", "APPROVED", 1)), true, false, ""},
		{"reviewed by me, no longer requested", open(rv("me", "COMMENTED", 1)), false, false, ""},
		{"reviewed by me, now a draft", github.PR{State: "open", Draft: true, Reviews: []github.Review{rv("me", "COMMENTED", 1)}}, false, false, ""},
		{"reviewed by me, someone else approved", open(rv("me", "COMMENTED", 1), rv("alice", "APPROVED", 2)), false, false, ""},
		{"reviewed by me, merged", github.PR{State: "closed", Merged: true, Reviews: []github.Review{rv("me", "APPROVED", 1)}}, false, false, "merged"},
		{"my pending review doesn't count", open(rv("me", "PENDING", 1)), false, false, "review not requested from you"},
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

// What sisyphus posts never counts as a reply, whoever's account posted it
// (otherwise its own review triggers a follow-up review of itself).
func TestNewRepliesIgnoresSisyphussOwnPosts(t *testing.T) {
	own := github.Review{User: "poster", State: "COMMENTED", Body: "Looks off.\n\n" + SisyphusMark, SubmittedAt: at(2)}
	pr := github.PR{Reviews: []github.Review{own}, Comments: []github.Comment{
		{User: "poster", Body: "Thanks!\n\n" + SisyphusMark, CreatedAt: at(3)},
		{User: "alice", Body: "Fixed.", CreatedAt: at(4)},
	}}
	if n, latest := newReplies(pr, "me", at(1)); n != 1 || !latest.Equal(at(4)) {
		t.Fatalf("replies = %d, latest %v", n, latest)
	}
}

func TestDesignReview(t *testing.T) {
	for _, c := range []struct {
		title string
		files []string
		want  bool
	}{
		// The fidesplus design reviews this was written for.
		{"ENG-7181: Add the migration runtime safety ADR", []string{"changelog/6963-x.yaml", "docs/ADRs/2026-11-02-migration-runtime-safety.md"}, true},
		{"Data subject correspondence: design for the beta (design review)", []string{"docs/designs/privacy-requests/dsc-2026-04.md", "docs/designs/privacy-requests/dsc.md"}, true},
		{"Design review:  datastore sampling", []string{"docs/designs/discovery/datastore-sampling.md", "docs/plans/datastore-sampling-implementation-plan.md"}, true},
		{"ENG-4480: Event Publisher design for consent events", []string{"docs/designs/consent/event-publisher-design.md"}, true},
		{"ENG-1: an RFC", []string{"README.md"}, true},                        // the title says so
		{"ENG-1: write things down", []string{"docs/plans/rollout.md"}, true}, // the path says so
		{"ENG-1: fix a typo", []string{"docs/setup.md", "changelog/1.yaml"}, false},
		{"ENG-1: design tokens for buttons", []string{"clients/admin-ui/src/theme.ts"}, false},
		{"ENG-1: design + prototype", []string{"docs/designs/x.md", "src/fides/x.py"}, false},
		{"ENG-1: design changelog", []string{"changelog/1.yaml"}, false},
		{"ENG-1: designer", []string{"docs/x.md"}, false},
		{"ENG-1: design", nil, false},
	} {
		if got := designReview(c.title, c.files); got != c.want {
			t.Errorf("designReview(%q, %q) = %v, want %v", c.title, c.files, got, c.want)
		}
	}
}

// A review submitted with no body carries only inline comments, which GitHub
// dates at drafting time, possibly before the cursor: the review still counts.
func TestNewRepliesCountsInlineOnlyReview(t *testing.T) {
	pr := github.PR{Comments: []github.Comment{{User: "alice", Body: "why?", CreatedAt: at(2)}},
		Reviews: []github.Review{rv("alice", "COMMENTED", 5)}}
	if n, _ := newReplies(pr, "me", at(3)); n != 1 {
		t.Fatalf("replies = %d", n)
	}
}

func TestDesignState(t *testing.T) {
	on := func(r github.Review, sha string) github.Review { r.CommitID = sha; return r }
	cmt := func(user string, m int) github.Comment { return github.Comment{User: user, Body: "x", CreatedAt: at(m)} }
	pr := func(head string, rs []github.Review, cs ...github.Comment) github.PR {
		return github.PR{HeadSHA: head, Reviews: rs, Comments: cs}
	}
	ours := on(rv("me", "APPROVED", 1), "h1")
	ours.Body = "fine\n\n" + SisyphusMark
	cases := []struct {
		name    string
		pr      github.PR
		updated bool
		want    DesignState
	}{
		{"never reviewed", pr("h1", nil, cmt("alice", 1)), false, DesignState{}},
		{"sisyphus's review isn't mine", pr("h1", []github.Review{ours}), false, DesignState{}},
		{"reviewed, nothing since", pr("h1", []github.Review{on(rv("me", "CHANGES_REQUESTED", 2), "h1")}, cmt("alice", 1)), false,
			DesignState{ReviewedAt: at(2), ReviewedSHA: "h1", Verdict: "CHANGES_REQUESTED"}},
		{"new commits", pr("h2", []github.Review{on(rv("me", "COMMENTED", 2), "h1")}), true,
			DesignState{ReviewedAt: at(2), ReviewedSHA: "h1", Verdict: "COMMENTED", NewCommits: true}},
		{"replies after my review", pr("h1", []github.Review{on(rv("me", "COMMENTED", 2), "h1")}, cmt("alice", 3), cmt("bob", 4)), true,
			DesignState{ReviewedAt: at(2), ReviewedSHA: "h1", Verdict: "COMMENTED", Replies: 2}},
		{"my own later comment resets the count", pr("h1", []github.Review{on(rv("me", "COMMENTED", 2), "h1")}, cmt("alice", 3), cmt("me", 4)), false,
			DesignState{ReviewedAt: at(2), ReviewedSHA: "h1", Verdict: "COMMENTED"}},
		{"a later comment keeps my verdict", pr("h1", []github.Review{on(rv("me", "COMMENTED", 3), "h1"), on(rv("me", "CHANGES_REQUESTED", 2), "h0")}), false,
			DesignState{ReviewedAt: at(3), ReviewedSHA: "h1", Verdict: "CHANGES_REQUESTED"}},
		{"approved: replies don't wake it", pr("h1", []github.Review{on(rv("me", "APPROVED", 2), "h1")}, cmt("alice", 3)), false,
			DesignState{ReviewedAt: at(2), ReviewedSHA: "h1", Verdict: "APPROVED", Replies: 1}},
	}
	for _, c := range cases {
		got := designState(c.pr, "me")
		if got != c.want || got.Updated() != c.updated {
			t.Errorf("%s: got %+v (updated %v), want %+v (updated %v)", c.name, got, got.Updated(), c.want, c.updated)
		}
	}
}
