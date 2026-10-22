package github

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/reneruck/waterboy/internal/proc"
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

// Client is everything waterboy needs from Client; swap the gh CLI out here.
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
	seen := map[string]bool{}
	var refs []PRRef
	for _, filter := range []string{"--review-requested=" + user} {
		out, err := proc.Run(ctx, "", "gh", "search", "prs", "--state=open", "--owner="+org, filter,
			"--json", "repository,number", "--limit", "200")
		if err != nil {
			return nil, err
		}
		found, err := parseSearch([]byte(out))
		if err != nil {
			return nil, err
		}
		for _, r := range found {
			if !seen[r.Key()] {
				seen[r.Key()] = true
				refs = append(refs, r)
			}
		}
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Key() < refs[j].Key() })
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

// ponytail: per_page=100, no pagination — same ceiling as pr-review-queue.sh.
func (CLI) PR(ctx context.Context, ref PRRef) (PR, error) {
	base := fmt.Sprintf("repos/%s/pulls/%d", ref.Repo, ref.Number)
	paths := []string{
		base,
		base + "/reviews?per_page=100",
		base + "/comments?per_page=100",
		fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", ref.Repo, ref.Number),
	}
	bodies := make([][]byte, len(paths))
	for i, p := range paths {
		out, err := proc.Run(ctx, "", "gh", "api", p)
		if err != nil {
			return PR{}, err
		}
		bodies[i] = []byte(out)
	}
	return parsePR(bodies[0], bodies[1], bodies[2], bodies[3])
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
			CreatedAt time.Time `json:"created_at"`
		}
		if err := json.Unmarshal(raw, &cs); err != nil {
			return PR{}, fmt.Errorf("parse comments: %w", err)
		}
		for _, c := range cs {
			pr.Comments = append(pr.Comments, Comment{User: c.User.Login, Bot: c.User.Type == "Bot", CreatedAt: c.CreatedAt})
		}
	}
	return pr, nil
}
