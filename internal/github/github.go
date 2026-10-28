package github

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/proc"
)

// PRRef identifies a PR. Repo is "owner/name".
type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r PRRef) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

type Review struct {
	User        string
	Bot         bool
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	SubmittedAt time.Time
}

type Comment struct {
	User      string
	Bot       bool
	Body      string
	CreatedAt time.Time
}

type PR struct {
	PRRef
	URL, Title, Author string
	State              string // "open" | "closed"
	Merged, Draft      bool
	HeadRef, HeadSHA   string
	HeadRepo           string   // "owner/name"; "" if the fork was deleted
	RequestedReviewers []string // logins of individually requested reviewers
	RequestedTeams     []string // slugs of requested teams
	Reviews            []Review
	Comments           []Comment // issue + inline review comments
}

// Client is everything sisyphus needs from Client; swap the gh CLI out here.
type Client interface {
	CurrentUser(ctx context.Context) (string, error)
	// Candidates finds open PRs that may await user's review. Search results lag
	// and are fuzzy: only the PR payload (PR) decides.
	Candidates(ctx context.Context, org, user string) ([]PRRef, error)
	// Teams returns the slugs of user's teams in org.
	Teams(ctx context.Context, org string) ([]string, error)
	PR(ctx context.Context, ref PRRef) (PR, error)
}

// CLI implements Client by shelling out to an authenticated `gh`.
type CLI struct{}

func (CLI) CurrentUser(ctx context.Context) (string, error) {
	return proc.Run(ctx, "", "gh", "api", "user", "-q", ".login")
}

func (CLI) Candidates(ctx context.Context, org, user string) ([]PRRef, error) {
	out, err := proc.Run(ctx, "", "gh", "search", "prs", "--state=open", "--owner="+org, "--review-requested="+user,
		"--json", "repository,number", "--limit", "200")
	if err != nil {
		return nil, err
	}
	refs, err := parseSearch([]byte(out))
	if err != nil {
		return nil, err
	}
	slices.SortFunc(refs, func(a, b PRRef) int { return strings.Compare(a.Key(), b.Key()) })
	return refs, nil
}

func (CLI) Teams(ctx context.Context, org string) ([]string, error) {
	out, err := proc.Run(ctx, "", "gh", "api", "user/teams", "--paginate",
		"--jq", fmt.Sprintf(`.[] | select(.organization.login == %q) | .slug`, org))
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// ponytail: per_page=100, no pagination: a PR with more reviews or comments loses the newest; paginate then.
func (c CLI) PR(ctx context.Context, ref PRRef) (PR, error) {
	d, err := c.Discussion(ctx, ref)
	if err != nil {
		return PR{}, err
	}
	return parsePR(d[0].Body, d[1].Body, d[2].Body, d[3].Body)
}

// OpenPR is an open PR a search found.
type OpenPR struct {
	Number    int       `json:"number"`
	HeadSHA   string    `json:"headRefOid"`
	UpdatedAt time.Time `json:"updatedAt"`
	Fork      bool      `json:"isCrossRepository"`
}

// OpenPRs are repo's open PRs with phrase in their title or body.
func (CLI) OpenPRs(ctx context.Context, repo, phrase string) ([]OpenPR, error) {
	out, err := proc.Run(ctx, "", "gh", "pr", "list", "--repo", repo, "--state", "open",
		"--search", fmt.Sprintf("%q in:title,body", phrase), "--json", "number,headRefOid,updatedAt,isCrossRepository", "--limit", "20")
	if err != nil {
		return nil, err
	}
	var prs []OpenPR
	return prs, json.Unmarshal([]byte(out), &prs)
}

// DiscussionFile is one raw GitHub API response about a PR.
type DiscussionFile struct {
	Name string // pr.json, reviews.json, review_comments.json, issue_comments.json
	Body []byte
}

// Discussion fetches a PR, its reviews, its inline review comments and its
// issue comments, as the GitHub API returns them.
func (CLI) Discussion(ctx context.Context, ref PRRef) ([]DiscussionFile, error) {
	base := fmt.Sprintf("repos/%s/pulls/%d", ref.Repo, ref.Number)
	files := []DiscussionFile{
		{Name: "pr.json"}, {Name: "reviews.json"}, {Name: "review_comments.json"}, {Name: "issue_comments.json"},
	}
	paths := []string{base, base + "/reviews?per_page=100", base + "/comments?per_page=100",
		fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", ref.Repo, ref.Number)}
	for i, p := range paths {
		out, err := proc.Run(ctx, "", "gh", "api", p)
		if err != nil {
			return nil, err
		}
		files[i].Body = []byte(out)
	}
	return files, nil
}

// CreateReview posts a review from file (the GitHub create-review payload)
// and returns the review's URL.
func (CLI) CreateReview(ctx context.Context, ref PRRef, file string) (string, error) {
	return proc.Run(ctx, "", "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/reviews", ref.Repo, ref.Number), "--input", file, "--jq", ".html_url")
}

// ReplyToComment answers in the thread of an inline review comment.
func (CLI) ReplyToComment(ctx context.Context, ref PRRef, commentID int64, body string) error {
	_, err := proc.Run(ctx, "", "gh", "api", "--method", "POST",
		fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", ref.Repo, ref.Number, commentID), "-f", "body="+body)
	return err
}

func parseSearch(out []byte) ([]PRRef, error) {
	var rows []struct {
		Number     int `json:"number"`
		Repository struct {
			NameWithOwner string `json:"nameWithOwner"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, fmt.Errorf("parse search: %w", err)
	}
	refs := make([]PRRef, len(rows))
	for i, r := range rows {
		refs[i] = PRRef{Repo: r.Repository.NameWithOwner, Number: r.Number}
	}
	return refs, nil
}

type ghUser struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type ghRepo struct {
	FullName string `json:"full_name"`
}

func parsePR(pull, reviews, reviewComments, issueComments []byte) (PR, error) {
	var p struct {
		Number             int      `json:"number"`
		State              string   `json:"state"`
		Merged             bool     `json:"merged"`
		Draft              bool     `json:"draft"`
		HTMLURL            string   `json:"html_url"`
		Title              string   `json:"title"`
		User               ghUser   `json:"user"`
		RequestedReviewers []ghUser `json:"requested_reviewers"`
		RequestedTeams     []struct {
			Slug string `json:"slug"`
		} `json:"requested_teams"`
		Head struct {
			Ref  string  `json:"ref"`
			SHA  string  `json:"sha"`
			Repo *ghRepo `json:"repo"`
		} `json:"head"`
		Base struct {
			Repo ghRepo `json:"repo"`
		} `json:"base"`
	}
	if err := json.Unmarshal(pull, &p); err != nil {
		return PR{}, fmt.Errorf("parse pull: %w", err)
	}
	pr := PR{
		PRRef: PRRef{Repo: p.Base.Repo.FullName, Number: p.Number},
		URL:   p.HTMLURL, Title: p.Title, Author: p.User.Login,
		State: p.State, Merged: p.Merged, Draft: p.Draft,
		HeadRef: p.Head.Ref, HeadSHA: p.Head.SHA,
	}
	for _, u := range p.RequestedReviewers {
		pr.RequestedReviewers = append(pr.RequestedReviewers, u.Login)
	}
	for _, t := range p.RequestedTeams {
		pr.RequestedTeams = append(pr.RequestedTeams, t.Slug)
	}
	if p.Head.Repo != nil {
		pr.HeadRepo = p.Head.Repo.FullName
	}

	var rs []struct {
		User        ghUser    `json:"user"`
		State       string    `json:"state"`
		Body        string    `json:"body"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	if err := json.Unmarshal(reviews, &rs); err != nil {
		return PR{}, fmt.Errorf("parse reviews: %w", err)
	}
	for _, r := range rs {
		pr.Reviews = append(pr.Reviews, Review{User: r.User.Login, Bot: r.User.Type == "Bot",
			State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt})
	}

	for _, raw := range [][]byte{reviewComments, issueComments} {
		var cs []struct {
			User      ghUser    `json:"user"`
			Body      string    `json:"body"`
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &cs); err != nil {
			return PR{}, fmt.Errorf("parse comments: %w", err)
		}
		for _, c := range cs {
			pr.Comments = append(pr.Comments, Comment{User: c.User.Login, Bot: c.User.Type == "Bot", Body: c.Body, CreatedAt: c.CreatedAt})
		}
	}
	return pr, nil
}
