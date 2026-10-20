package main

import (
	"sort"
	"time"
)

// latestStates returns each human reviewer's effective review state, the way
// GitHub computes it: COMMENTED and PENDING never override an earlier verdict.
func latestStates(reviews []Review) map[string]string {
	rs := append([]Review(nil), reviews...)
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
func dropReason(pr PR, me string, isCandidate, includeDrafts bool) string {
	if pr.Merged {
		return "merged"
	}
	if pr.State != "open" {
		return "closed"
	}
	states := latestStates(pr.Reviews)
	if states[me] == "APPROVED" {
		return "approved by you"
	}
	if states[me] != "CHANGES_REQUESTED" {
		users := make([]string, 0, len(states))
		for u := range states {
			users = append(users, u)
		}
		sort.Strings(users)
		for _, u := range users {
			if u != me && states[u] == "APPROVED" {
				return "approved by " + u
			}
		}
	}
	if pr.Draft && !includeDrafts {
		return "draft"
	}
	if !isCandidate {
		return "no longer requested"
	}
	return ""
}

// newReplies counts human comments and review bodies not by me that are newer
// than since. latest is the newest timestamp seen on the PR (or since), used to
// advance the cursor.
func newReplies(pr PR, me string, since time.Time) (count int, latest time.Time) {
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
