package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// InboxComment is one comment on a PR: an inline review comment, a review's
// top-level body, or an issue comment.
type InboxComment struct {
	ID      int64 // GraphQL databaseId, the id the REST API knows
	Author  string
	Bot     bool // GitHub says the author is a Bot account; the caller adds its own list
	Body    string
	Created time.Time
	URL     string
	State   string // review bodies only: COMMENTED | CHANGES_REQUESTED | …
}

// ReviewThread is an inline conversation.
type ReviewThread struct {
	ID       string // GraphQL node id
	Resolved bool
	Outdated bool
	Path     string
	Line     int
	Comments []InboxComment // oldest first
}

// Inbox is everything people (and bots) said on a PR.
type Inbox struct {
	Threads []ReviewThread
	Reviews []InboxComment // non-empty bodies of COMMENTED / CHANGES_REQUESTED reviews
	Issue   []InboxComment // conversation comments
}

// ponytail: one page each (100 threads of 50 comments, the last 100 reviews and
// conversation comments); a PR with more is rare, add cursors when one shows up.
const inboxQuery = `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){pullRequest(number:$number){
reviewThreads(first:100){nodes{id isResolved isOutdated path line comments(first:50){nodes{databaseId body createdAt url author{login __typename}}}}}
reviews(last:100){nodes{databaseId state body createdAt url author{login __typename}}}
comments(last:100){nodes{databaseId body createdAt url author{login __typename}}}
}}}`

// Inbox reads the PR's review threads, review bodies and conversation comments.
func (a *API) Inbox(ctx context.Context, ref PRRef) (Inbox, error) {
	owner, name, err := splitRepo(ref.Repo)
	if err != nil {
		return Inbox{}, err
	}
	var data json.RawMessage
	if err := a.graphql(ctx, inboxQuery, map[string]any{"owner": owner, "name": name, "number": ref.Number}, &data); err != nil {
		return Inbox{}, err
	}
	return parseInbox(data)
}

type gqlComment struct {
	DatabaseID int64     `json:"databaseId"`
	State      string    `json:"state"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"createdAt"`
	URL        string    `json:"url"`
	Author     *struct {
		Login string `json:"login"`
		Type  string `json:"__typename"`
	} `json:"author"` // null for a deleted account
}

func (c gqlComment) comment() InboxComment {
	ic := InboxComment{ID: c.DatabaseID, Body: c.Body, Created: c.CreatedAt, URL: c.URL, State: c.State, Author: "ghost"}
	if c.Author != nil {
		ic.Author = c.Author.Login
		ic.Bot = c.Author.Type == "Bot" || strings.HasSuffix(c.Author.Login, "[bot]")
	}
	return ic
}

func parseInbox(b []byte) (Inbox, error) {
	var v struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						IsOutdated bool   `json:"isOutdated"`
						Path       string `json:"path"`
						Line       int    `json:"line"`
						Comments   struct {
							Nodes []gqlComment `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
				Reviews  struct{ Nodes []gqlComment } `json:"reviews"`
				Comments struct{ Nodes []gqlComment } `json:"comments"`
			} `json:"pullRequest"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return Inbox{}, fmt.Errorf("parse inbox: %w", err)
	}
	pr := v.Repository.PullRequest
	var in Inbox
	for _, t := range pr.ReviewThreads.Nodes {
		th := ReviewThread{ID: t.ID, Resolved: t.IsResolved, Outdated: t.IsOutdated, Path: t.Path, Line: t.Line}
		for _, c := range t.Comments.Nodes {
			th.Comments = append(th.Comments, c.comment())
		}
		in.Threads = append(in.Threads, th)
	}
	for _, r := range pr.Reviews.Nodes {
		if (r.State == "COMMENTED" || r.State == "CHANGES_REQUESTED") && strings.TrimSpace(r.Body) != "" {
			in.Reviews = append(in.Reviews, r.comment())
		}
	}
	for _, c := range pr.Comments.Nodes {
		in.Issue = append(in.Issue, c.comment())
	}
	return in, nil
}
