package tracker

import (
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
)

// latestStates returns each human reviewer's effective review state, the way
// GitHub computes it: COMMENTED and PENDING never override an earlier verdict.
func latestStates(reviews []github.Review) map[string]string {
	rs := append([]github.Review(nil), reviews...)
	slices.SortStableFunc(rs, func(a, b github.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
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
	if r := closedReason(pr); r != "" {
		return r
	}
	if reviewedByMe(pr, me) {
		return ""
	}
	// Not reviewed by me yet: someone else's approval means it doesn't need me.
	states := latestStates(pr.Reviews)
	for _, u := range slices.Sorted(maps.Keys(states)) {
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

// closedReason is why a PR tracked by hand is dropped: only when it's merged or closed.
func closedReason(pr github.PR) string {
	if pr.Merged {
		return "merged"
	}
	if pr.State != "open" {
		return "closed"
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

// SisyphusMark ends everything sisyphus posts; such posts are never replies.
const SisyphusMark = "🤖 Reviewed by sisyphus"

// newReplies counts human comments and review bodies not by me that are newer
// than since. latest is the newest timestamp seen on the PR (or since), used to
// advance the cursor.
func newReplies(pr github.PR, me string, since time.Time) (count int, latest time.Time) {
	latest = since
	consider := func(user string, bot bool, body string, ts time.Time, counts bool) {
		if ts.After(latest) {
			latest = ts
		}
		if counts && !bot && user != me && !strings.Contains(body, SisyphusMark) && ts.After(since) {
			count++
		}
	}
	for _, c := range pr.Comments {
		consider(c.User, c.Bot, c.Body, c.CreatedAt, true)
	}
	for _, r := range pr.Reviews {
		// An empty COMMENTED review is inline comments dated at drafting, maybe before since.
		consider(r.User, r.Bot, r.Body, r.SubmittedAt, r.Body != "" || r.State == "COMMENTED" || r.State == "CHANGES_REQUESTED")
	}
	return count, latest
}

var (
	designTitle = regexp.MustCompile(`(?i)\b(design|adrs?|rfcs?)\b`)
	designDir   = regexp.MustCompile(`(?i)(^|/)(designs?|adrs?|rfcs?|plans)/`)
)

// designReview says whether a PR asks for a review of a design, not of code:
// it changes only prose, and its title or a doc's path says design (ADR, RFC).
// ponytail: path and title heuristic; a design PR with a code spike in it still gets automatic reviews.
func designReview(title string, files []string) bool {
	signal := designTitle.MatchString(title)
	docs := 0
	for _, f := range files {
		switch {
		case strings.HasPrefix(f, "changelog/"): // the fragment every fidesplus PR carries
		case strings.HasPrefix(f, "docs/") || slices.Contains([]string{".md", ".mdx", ".rst"}, strings.ToLower(path.Ext(f))):
			docs++
			signal = signal || designDir.MatchString(f)
		default:
			return false
		}
	}
	return docs > 0 && signal
}

// designState reads where my review of a design PR stands. My last word is my
// latest review or comment: replies count from then on.
func designState(pr github.PR, me string) DesignState {
	var d DesignState
	var said time.Time
	mine := func(user, body string) bool { return user == me && !strings.Contains(body, SisyphusMark) }
	rs := slices.Clone(pr.Reviews)
	slices.SortStableFunc(rs, func(a, b github.Review) int { return a.SubmittedAt.Compare(b.SubmittedAt) })
	for _, r := range rs {
		if !mine(r.User, r.Body) || r.State == "PENDING" || r.State == "DISMISSED" {
			continue
		}
		d.ReviewedAt, d.ReviewedSHA = r.SubmittedAt, r.CommitID
		if r.State != "COMMENTED" || d.Verdict == "" {
			d.Verdict = r.State // GitHub's rule: a comment doesn't undo an earlier verdict
		}
	}
	if d.ReviewedAt.IsZero() {
		return d
	}
	said = d.ReviewedAt
	for _, c := range pr.Comments {
		if mine(c.User, c.Body) && c.CreatedAt.After(said) {
			said = c.CreatedAt
		}
	}
	d.NewCommits = d.ReviewedSHA != "" && d.ReviewedSHA != pr.HeadSHA
	d.Replies, _ = newReplies(pr, me, said)
	return d
}
