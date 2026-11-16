package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// PRRef identifies a PR. Repo is "owner/name".
type PRRef struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
}

func (r PRRef) Key() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// ParseKey reads a Key back: "owner/name#n".
func ParseKey(key string) (PRRef, error) {
	repo, n, ok := strings.Cut(key, "#")
	num, err := strconv.Atoi(n)
	if !ok || err != nil || num <= 0 || strings.Count(repo, "/") != 1 {
		return PRRef{}, fmt.Errorf("%q isn't owner/repo#number", key)
	}
	return PRRef{Repo: repo, Number: num}, nil
}

type Review struct {
	User        string
	Bot         bool
	State       string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED | PENDING
	Body        string
	SubmittedAt time.Time
	CommitID    string // the head it was submitted on
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

// Client is what the tracker needs from GitHub; API implements it, Fake stands in for tests.
type Client interface {
	CurrentUser(ctx context.Context) (string, error)
	// Candidates finds open PRs that may await user's review. Search results lag
	// and are fuzzy: only the PR payload (PR) decides.
	Candidates(ctx context.Context, org, user string) ([]PRRef, error)
	// Teams returns the slugs of user's teams in org.
	Teams(ctx context.Context, org string) ([]string, error)
	PR(ctx context.Context, ref PRRef) (PR, error)
	// Files lists the paths a PR changes.
	Files(ctx context.Context, ref PRRef) ([]string, error)
}

func (a *API) CurrentUser(ctx context.Context) (string, error) {
	var u struct {
		Login string `json:"login"`
	}
	return u.Login, a.get(ctx, "user", &u)
}

// maxCandidates matches the limit `gh search prs` ran with.
const maxCandidates = 200

func (a *API) Candidates(ctx context.Context, org, user string) ([]PRRef, error) {
	q := url.Values{"q": {fmt.Sprintf("is:pr is:open user:%s review-requested:%s", org, user)}, "per_page": {"100"}}
	var refs []PRRef
	for path := "search/issues?" + q.Encode(); path != "" && len(refs) < maxCandidates; {
		b, next, err := a.do(ctx, http.MethodGet, path, nil)
		if err != nil {
			return nil, err
		}
		page, err := parseSearch(b)
		if err != nil {
			return nil, err
		}
		refs, path = append(refs, page...), next
	}
	slices.SortFunc(refs, func(a, b PRRef) int { return strings.Compare(a.Key(), b.Key()) })
	return refs, nil
}

func (a *API) Teams(ctx context.Context, org string) ([]string, error) {
	ts, err := pages[struct {
		Slug         string `json:"slug"`
		Organization struct {
			Login string `json:"login"`
		} `json:"organization"`
	}](ctx, a, "user/teams?per_page=100")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, t := range ts {
		if t.Organization.Login == org {
			out = append(out, t.Slug)
		}
	}
	return out, nil
}

// ponytail: per_page=100, no pagination: a PR with more reviews or comments loses the newest; paginate then.
func (a *API) PR(ctx context.Context, ref PRRef) (PR, error) {
	d, err := a.Discussion(ctx, ref)
	if err != nil {
		return PR{}, err
	}
	return parsePR(d[0].Body, d[1].Body, d[2].Body, d[3].Body)
}

func (a *API) Files(ctx context.Context, ref PRRef) ([]string, error) {
	fs, err := pages[struct {
		Filename string `json:"filename"`
	}](ctx, a, fmt.Sprintf("repos/%s/pulls/%d/files?per_page=100", ref.Repo, ref.Number))
	if err != nil {
		return nil, err
	}
	out := make([]string, len(fs))
	for i, f := range fs {
		out[i] = f.Filename
	}
	return out, nil
}

// OpenPR is an open PR a search found.
type OpenPR struct {
	Number    int       `json:"number"`
	HeadSHA   string    `json:"headRefOid"`
	UpdatedAt time.Time `json:"updatedAt"`
	Fork      bool      `json:"isCrossRepository"`
}

const openPRsQuery = `query($q:String!){search(query:$q,type:ISSUE,first:20){nodes{
... on PullRequest{number headRefOid updatedAt isCrossRepository}}}}`

// OpenPRs are repo's open PRs with phrase in their title or body.
func (a *API) OpenPRs(ctx context.Context, repo, phrase string) ([]OpenPR, error) {
	var v struct {
		Search struct{ Nodes []OpenPR }
	}
	q := fmt.Sprintf("repo:%s is:pr is:open %q in:title,body", repo, phrase)
	if err := a.graphql(ctx, openPRsQuery, map[string]any{"q": q}, &v); err != nil {
		return nil, err
	}
	return v.Search.Nodes, nil
}

// DiscussionFile is one raw GitHub API response about a PR.
type DiscussionFile struct {
	Name string // pr.json, reviews.json, review_comments.json, issue_comments.json
	Body []byte
}

// Discussion fetches a PR, its reviews, its inline review comments and its
// issue comments, as the GitHub API returns them.
func (a *API) Discussion(ctx context.Context, ref PRRef) ([]DiscussionFile, error) {
	base := fmt.Sprintf("repos/%s/pulls/%d", ref.Repo, ref.Number)
	files := []DiscussionFile{
		{Name: "pr.json"}, {Name: "reviews.json"}, {Name: "review_comments.json"}, {Name: "issue_comments.json"},
	}
	paths := []string{base, base + "/reviews?per_page=100", base + "/comments?per_page=100",
		fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", ref.Repo, ref.Number)}
	for i, p := range paths {
		b, _, err := a.do(ctx, http.MethodGet, p, nil)
		if err != nil {
			return nil, err
		}
		files[i].Body = b
	}
	return files, nil
}

// CreateReview posts a review from file (the GitHub create-review payload)
// and returns the review's URL.
func (a *API) CreateReview(ctx context.Context, ref PRRef, file string) (string, error) {
	payload, err := os.ReadFile(file)
	if err != nil {
		return "", err
	}
	var r struct {
		HTMLURL string `json:"html_url"`
	}
	return r.HTMLURL, a.send(ctx, http.MethodPost, fmt.Sprintf("repos/%s/pulls/%d/reviews", ref.Repo, ref.Number), payload, &r)
}

// ReplyToComment answers in the thread of an inline review comment.
func (a *API) ReplyToComment(ctx context.Context, ref PRRef, commentID int64, body string) error {
	_, err := a.Reply(ctx, ref, commentID, body)
	return err
}

// Reply is ReplyToComment that returns the new comment's id.
func (a *API) Reply(ctx context.Context, ref PRRef, commentID int64, body string) (int64, error) {
	var c struct {
		ID int64 `json:"id"`
	}
	return c.ID, a.send(ctx, http.MethodPost, fmt.Sprintf("repos/%s/pulls/%d/comments/%d/replies", ref.Repo, ref.Number, commentID),
		map[string]string{"body": body}, &c)
}

// parseSearch reads one page of search/issues.
func parseSearch(b []byte) ([]PRRef, error) {
	var page struct {
		Items []struct {
			Number        int    `json:"number"`
			RepositoryURL string `json:"repository_url"` // https://api.github.com/repos/owner/name
		} `json:"items"`
	}
	if err := json.Unmarshal(b, &page); err != nil {
		return nil, fmt.Errorf("parse search: %w", err)
	}
	refs := make([]PRRef, 0, len(page.Items))
	for _, it := range page.Items {
		_, repo, ok := strings.Cut(it.RepositoryURL, "/repos/")
		if !ok {
			return nil, fmt.Errorf("parse search: repository_url %q", it.RepositoryURL)
		}
		refs = append(refs, PRRef{Repo: repo, Number: it.Number})
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
		CommitID    string    `json:"commit_id"`
	}
	if err := json.Unmarshal(reviews, &rs); err != nil {
		return PR{}, fmt.Errorf("parse reviews: %w", err)
	}
	for _, r := range rs {
		pr.Reviews = append(pr.Reviews, Review{User: r.User.Login, Bot: r.User.Type == "Bot",
			State: r.State, Body: r.Body, SubmittedAt: r.SubmittedAt, CommitID: r.CommitID})
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
