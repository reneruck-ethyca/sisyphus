package tracker

import (
	"slices"
	"sort"
	"time"

	"github.com/reneruck/waterboy/internal/github"
)

// latestStates returns each human reviewer's effective review state, the way
// GitHub computes it: COMMENTED and PENDING never override an earlier verdict.
func latestStates(reviews []github.Review) map[string]string {
	rs := append([]github.Review(nil), reviews...)
	sort.SliceStable(rs, func(i, j int) bool { return rs[i].SubmittedAt.Before(rs[j].SubmittedAt) })
	states := map[string]string{}
	for _, r := range rs {
		if r.Bot || r.State == "COMMENTED" || r.State == "PENDING" {
			continue
		}
		states[r.User] = r.State
	}
	return states
}

// dropReason says why a PR should not be (or no longer be) tracked; "" = keep.
// Once I've reviewed it, only merging or closing drops it.
func dropReason(pr github.PR, me string, isRequested, includeDrafts bool) string {
	if pr.Merged {
		return "merged"
	}
	if pr.State != "open" {
		return "closed"
	}
	if reviewedByMe(pr, me) {
		return ""
	}
	// Not reviewed by me yet: someone else's approval means it doesn't need me.
	states := latestStates(pr.Reviews)
	users := make([]string, 0, len(states))
	for u := range states {
		users = append(users, u)
	}
	sort.Strings(users)
	for _, u := range users {
		if states[u] == "APPROVED" {
			return "approved by " + u
		}
	}
	if pr.Draft && !includeDrafts {
		return "draft"
	}
	if !isRequested {
		return "review not requested from you"
	}
	return ""
}

// reviewedByMe: I've submitted a review (a pending draft review doesn't count).
func reviewedByMe(pr github.PR, me string) bool {
	return slices.ContainsFunc(pr.Reviews, func(r github.Review) bool { return r.User == me && r.State != "PENDING" })
}

// requested reports whether the PR payload asks me for a review: as a requested
// reviewer, or through a requested team I'm on. Needed to pick a PR up.
func requested(pr github.PR, me string, myTeams []string) bool {
	return slices.Contains(pr.RequestedReviewers, me) ||
		slices.ContainsFunc(pr.RequestedTeams, func(t string) bool { return slices.Contains(myTeams, t) })
}

// newReplies counts human comments and review bodies not by me that are newer
// than since. latest is the newest timestamp seen on the PR (or since), used to
// advance the cursor.
func newReplies(pr github.PR, me string, since time.Time) (count int, latest time.Time) {
	latest = since
	consider := func(user string, bot bool, ts time.Time, counts bool) {
		if ts.After(latest) {
			latest = ts
		}
		if counts && !bot && user != me && ts.After(since) {
			count++
		}
	}
	for _, c := range pr.Comments {
		consider(c.User, c.Bot, c.CreatedAt, true)
	}
	for _, r := range pr.Reviews {
		consider(r.User, r.Bot, r.SubmittedAt, r.Body != "")
	}
	return count, latest
}
