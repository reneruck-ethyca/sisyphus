package github

// The design review page's calls: render a PR's docs, and read and write its
// review threads as the user. Ported from the gh design-review extension.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// DesignPR is what the design page needs about a PR, read fresh.
type DesignPR struct {
	ID       string // GraphQL node id, for mutations
	Title    string
	URL      string
	HeadSHA  string
	BaseSHA  string
	HeadRepo string // "owner/name" where the head commits live; the base repo if the fork is gone
	Viewer   string
}

func splitRepo(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok {
		return "", "", fmt.Errorf("bad repo %q", repo)
	}
	return owner, name, nil
}

func (a *API) DesignPR(ctx context.Context, ref PRRef) (DesignPR, error) {
	owner, name, err := splitRepo(ref.Repo)
	if err != nil {
		return DesignPR{}, err
	}
	var resp struct {
		Viewer     struct{ Login string }
		Repository struct {
			PullRequest struct {
				ID, Title, URL         string
				HeadRefOid, BaseRefOid string
				HeadRepository         *struct{ NameWithOwner string }
			}
		}
	}
	err = a.graphql(ctx, `query($owner:String!,$repo:String!,$number:Int!){ viewer { login }
		repository(owner:$owner,name:$repo){ pullRequest(number:$number){
			id title url headRefOid baseRefOid headRepository { nameWithOwner } } } }`,
		map[string]any{"owner": owner, "repo": name, "number": ref.Number}, &resp)
	if err != nil {
		return DesignPR{}, err
	}
	p := resp.Repository.PullRequest
	d := DesignPR{ID: p.ID, Title: p.Title, URL: p.URL, HeadSHA: p.HeadRefOid, BaseSHA: p.BaseRefOid, HeadRepo: ref.Repo, Viewer: resp.Viewer.Login}
	if p.HeadRepository != nil {
		d.HeadRepo = p.HeadRepository.NameWithOwner
	}
	return d, nil
}

// ChangedFile is a file a PR (or a comparison) changes, with its patch; Patch
// is nil when GitHub leaves it out (binary, or too large).
type ChangedFile struct {
	Filename string  `json:"filename"`
	Status   string  `json:"status"`
	Patch    *string `json:"patch"`
}

func (a *API) PRFiles(ctx context.Context, ref PRRef) ([]ChangedFile, error) {
	return pages[ChangedFile](ctx, a, pullPath(ref)+"/files?per_page=100")
}

// CompareFiles lists what changed between two commits.
// ponytail: one page, GitHub's 300-file cap; a design PR touching more is not a design PR.
func (a *API) CompareFiles(ctx context.Context, repo, base, head string) ([]ChangedFile, error) {
	var resp struct{ Files []ChangedFile }
	err := a.get(ctx, fmt.Sprintf("repos/%s/compare/%s...%s?per_page=300", repo, url.PathEscape(base), url.PathEscape(head)), &resp)
	return resp.Files, err
}

// Content returns a file's bytes at ref. Past 1 MB the contents API leaves the
// content out, so it's read as a git blob.
func (a *API) Content(ctx context.Context, repo, path, ref string) ([]byte, error) {
	segs := strings.Split(path, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	var f struct {
		Type, Encoding, Content, SHA string
	}
	if err := a.get(ctx, fmt.Sprintf("repos/%s/contents/%s?ref=%s", repo, strings.Join(segs, "/"), url.QueryEscape(ref)), &f); err != nil {
		return nil, err
	}
	if f.Type != "file" {
		return nil, fmt.Errorf("%s is a %s, not a file", path, f.Type)
	}
	if f.Encoding != "base64" {
		var blob struct{ Content string }
		if err := a.get(ctx, fmt.Sprintf("repos/%s/git/blobs/%s", repo, f.SHA), &blob); err != nil {
			return nil, err
		}
		f.Content = blob.Content
	}
	return base64.StdEncoding.DecodeString(strings.ReplaceAll(f.Content, "\n", ""))
}

type DocComment struct {
	ID         string `json:"id"`
	DatabaseID string `json:"databaseId"`
	Author     string `json:"author"`
	AvatarURL  string `json:"avatarUrl"`
	BodyHTML   string `json:"bodyHTML"`
	CreatedAt  string `json:"createdAt"`
	URL        string `json:"url"`
	State      string `json:"state"`
}

// DocAnchor is the exact lines a comment was made on, from the marker the page puts in its body.
type DocAnchor struct {
	StartLine int    `json:"startLine"`
	Line      int    `json:"line"`
	Sha       string `json:"sha"`
}

type DocThread struct {
	ID                string       `json:"id"`
	Path              string       `json:"path"`
	SubjectType       string       `json:"subjectType"`
	StartLine         *int         `json:"startLine"`
	Line              *int         `json:"line"`
	OriginalStartLine *int         `json:"originalStartLine"`
	OriginalLine      *int         `json:"originalLine"`
	IsResolved        bool         `json:"isResolved"`
	IsOutdated        bool         `json:"isOutdated"`
	Anchor            *DocAnchor   `json:"anchor"`
	Comments          []DocComment `json:"comments"`
}

var markerRe = regexp.MustCompile(`<!-- design-review:L(\d+)-L(\d+)@([0-9a-f]+) -->`)

func parseMarker(body string) *DocAnchor {
	m := markerRe.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	s, _ := strconv.Atoi(m[1])
	e, _ := strconv.Atoi(m[2])
	return &DocAnchor{s, e, m[3]}
}

const threadsQuery = `query($owner:String!,$repo:String!,$number:Int!,$cursor:String){
  repository(owner:$owner,name:$repo){ pullRequest(number:$number){
    reviewThreads(first:50, after:$cursor){
      pageInfo { hasNextPage endCursor }
      nodes {
        id path subjectType isResolved isOutdated
        line startLine originalLine originalStartLine
        comments(first:100){ nodes {
          id fullDatabaseId body bodyHTML createdAt url state
          author { login avatarUrl }
        } }
      }
    }
  } } }`

// threadNode is a reviewThreads node as GraphQL returns it. Comments needs the
// explicit tag: without it the embedded DocThread.Comments wins the name match
// and {"nodes": [...]} fails to decode into []DocComment.
type threadNode struct {
	DocThread
	Comments struct {
		Nodes []struct {
			ID, Body, BodyHTML, CreatedAt, URL, State string
			FullDatabaseID                            string // BigInt, sent as a string
			Author                                    *struct{ Login, AvatarURL string }
		}
	} `json:"comments"`
}

// Threads returns the PR's review threads on path, or on every file when path is "".
func (a *API) Threads(ctx context.Context, ref PRRef, path string) ([]DocThread, error) {
	owner, name, err := splitRepo(ref.Repo)
	if err != nil {
		return nil, err
	}
	out := []DocThread{}
	var cursor *string
	for {
		var resp struct {
			Repository struct {
				PullRequest struct {
					ReviewThreads struct {
						PageInfo struct {
							HasNextPage bool
							EndCursor   string
						}
						Nodes []threadNode
					}
				}
			}
		}
		vars := map[string]any{"owner": owner, "repo": name, "number": ref.Number, "cursor": cursor}
		if err := a.graphql(ctx, threadsQuery, vars, &resp); err != nil {
			return nil, err
		}
		rt := resp.Repository.PullRequest.ReviewThreads
		for _, n := range rt.Nodes {
			if path != "" && n.Path != path {
				continue
			}
			t := n.DocThread
			t.Comments = []DocComment{}
			for i, c := range n.Comments.Nodes {
				if i == 0 {
					t.Anchor = parseMarker(c.Body)
				}
				author, avatar := "ghost", ""
				if c.Author != nil {
					author, avatar = c.Author.Login, c.Author.AvatarURL
				}
				t.Comments = append(t.Comments, DocComment{c.ID, c.FullDatabaseID, author, avatar, c.BodyHTML, c.CreatedAt, c.URL, c.State})
			}
			out = append(out, t)
		}
		if !rt.PageInfo.HasNextPage {
			return out, nil
		}
		cursor = &rt.PageInfo.EndCursor
	}
}

// NewThread is a comment that starts a thread. Mode "now" posts it on its own;
// anything else adds it to the user's pending review.
type NewThread struct {
	Path        string `json:"path"`
	Body        string `json:"body"`
	SubjectType string `json:"subjectType"` // LINE | FILE
	StartLine   int    `json:"startLine,omitempty"`
	Line        int    `json:"line,omitempty"`
	Mode        string `json:"mode"`
}

// reviewMu keeps two writes from each creating a pending review: GitHub allows one per user.
// ponytail: one lock for every PR; per-PR locks if writes ever queue up behind it.
var reviewMu sync.Mutex

func (a *API) CreateThread(ctx context.Context, ref PRRef, pr DesignPR, in NewThread) error {
	if in.Mode == "now" {
		body := map[string]any{"commit_id": pr.HeadSHA, "path": in.Path, "body": in.Body}
		if in.SubjectType == "FILE" {
			body["subject_type"] = "file"
		} else {
			body["line"], body["side"] = in.Line, "RIGHT"
			if in.StartLine != 0 && in.StartLine != in.Line {
				body["start_line"], body["start_side"] = in.StartLine, "RIGHT"
			}
		}
		return a.send(ctx, http.MethodPost, pullPath(ref)+"/comments", body, nil)
	}
	return a.withReview(ctx, ref, pr, func(reviewID string) error {
		input := map[string]any{"pullRequestReviewId": reviewID, "path": in.Path, "body": in.Body, "subjectType": in.SubjectType}
		if in.SubjectType == "LINE" {
			input["line"], input["side"] = in.Line, "RIGHT"
			if in.StartLine != 0 && in.StartLine != in.Line {
				input["startLine"], input["startSide"] = in.StartLine, "RIGHT"
			}
		}
		return a.mutate(ctx, `mutation($input:AddPullRequestReviewThreadInput!){ addPullRequestReviewThread(input:$input){ clientMutationId } }`, input)
	})
}

// ReplyThread replies in a thread; mode "now" needs the database id of the thread's first comment.
func (a *API) ReplyThread(ctx context.Context, ref PRRef, pr DesignPR, threadID, firstCommentID, body, mode string) error {
	if mode == "now" {
		if _, err := strconv.ParseInt(firstCommentID, 10, 64); err != nil {
			return fmt.Errorf("comment id %q: %w", firstCommentID, err)
		}
		return a.send(ctx, http.MethodPost, fmt.Sprintf("%s/comments/%s/replies", pullPath(ref), firstCommentID), map[string]any{"body": body}, nil)
	}
	return a.withReview(ctx, ref, pr, func(reviewID string) error {
		return a.mutate(ctx, `mutation($input:AddPullRequestReviewThreadReplyInput!){ addPullRequestReviewThreadReply(input:$input){ clientMutationId } }`,
			map[string]any{"pullRequestReviewThreadId": threadID, "pullRequestReviewId": reviewID, "body": body})
	})
}

func (a *API) SetResolved(ctx context.Context, threadID string, resolved bool) error {
	op := "unresolveReviewThread"
	if resolved {
		op = "resolveReviewThread"
	}
	return a.mutate(ctx, fmt.Sprintf(`mutation($input:%sInput!){ %s(input:$input){ clientMutationId } }`, strings.ToUpper(op[:1])+op[1:], op),
		map[string]any{"threadId": threadID})
}

// PendingReview is the user's unsubmitted review: its id and how many comments it holds.
type PendingReview struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// Pending returns the viewer's pending review on the PR, or nil.
func (a *API) Pending(ctx context.Context, ref PRRef, viewer string) (*PendingReview, error) {
	owner, name, err := splitRepo(ref.Repo)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Repository struct {
			PullRequest struct {
				Reviews struct {
					Nodes []struct {
						ID       string
						Author   *struct{ Login string }
						Comments struct{ TotalCount int }
					}
				}
			}
		}
	}
	err = a.graphql(ctx, `query($owner:String!,$repo:String!,$number:Int!){
		repository(owner:$owner,name:$repo){ pullRequest(number:$number){
			reviews(states:PENDING, first:10){ nodes { id author { login } comments { totalCount } } } } } }`,
		map[string]any{"owner": owner, "repo": name, "number": ref.Number}, &resp)
	if err != nil {
		return nil, err
	}
	for _, n := range resp.Repository.PullRequest.Reviews.Nodes {
		if n.Author != nil && n.Author.Login == viewer {
			return &PendingReview{n.ID, n.Comments.TotalCount}, nil
		}
	}
	return nil, nil
}

// withReview runs fn with the user's pending review, started on github.com or
// here, creating one when there is none.
func (a *API) withReview(ctx context.Context, ref PRRef, pr DesignPR, fn func(reviewID string) error) error {
	reviewMu.Lock()
	defer reviewMu.Unlock()
	p, err := a.Pending(ctx, ref, pr.Viewer)
	if err != nil {
		return err
	}
	id := ""
	if p != nil {
		id = p.ID
	} else {
		var resp struct {
			AddPullRequestReview struct{ PullRequestReview struct{ ID string } }
		}
		err := a.graphql(ctx, `mutation($input:AddPullRequestReviewInput!){ addPullRequestReview(input:$input){ pullRequestReview { id } } }`,
			map[string]any{"input": map[string]any{"pullRequestId": pr.ID, "commitOID": pr.HeadSHA}}, &resp)
		if err != nil {
			return err
		}
		id = resp.AddPullRequestReview.PullRequestReview.ID
	}
	return fn(id)
}

var ErrNoPendingReview = errors.New("no pending review")

// SubmitReview submits the pending review as event, or posts a fresh review
// (a bare Approve, say) when there is none.
func (a *API) SubmitReview(ctx context.Context, ref PRRef, pr DesignPR, event, body string) error {
	reviewMu.Lock()
	defer reviewMu.Unlock()
	p, err := a.Pending(ctx, ref, pr.Viewer)
	if err != nil {
		return err
	}
	if p == nil {
		input := map[string]any{"pullRequestId": pr.ID, "commitOID": pr.HeadSHA, "event": event}
		if body != "" {
			input["body"] = body
		}
		return a.mutate(ctx, `mutation($input:AddPullRequestReviewInput!){ addPullRequestReview(input:$input){ clientMutationId } }`, input)
	}
	input := map[string]any{"pullRequestReviewId": p.ID, "event": event}
	if body != "" {
		input["body"] = body
	}
	return a.mutate(ctx, `mutation($input:SubmitPullRequestReviewInput!){ submitPullRequestReview(input:$input){ clientMutationId } }`, input)
}

func (a *API) DiscardReview(ctx context.Context, ref PRRef, viewer string) error {
	reviewMu.Lock()
	defer reviewMu.Unlock()
	p, err := a.Pending(ctx, ref, viewer)
	if err != nil {
		return err
	}
	if p == nil {
		return ErrNoPendingReview
	}
	return a.mutate(ctx, `mutation($input:DeletePullRequestReviewInput!){ deletePullRequestReview(input:$input){ clientMutationId } }`,
		map[string]any{"pullRequestReviewId": p.ID})
}

func (a *API) mutate(ctx context.Context, query string, input map[string]any) error {
	var resp any
	return a.graphql(ctx, query, map[string]any{"input": input}, &resp)
}

// Hunk is a commentable range of lines on the new side of a patch, 1-based and inclusive.
type Hunk struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

var hunkHeader = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ParsePatch returns a unified diff's new-side hunks and the lines it adds.
func ParsePatch(patch string) (hunks []Hunk, added []int) {
	hunks, added = []Hunk{}, []int{}
	right := 0
	for _, l := range strings.Split(patch, "\n") {
		if m := hunkHeader.FindStringSubmatch(l); m != nil {
			c, _ := strconv.Atoi(m[1])
			d := 1
			if m[2] != "" {
				d, _ = strconv.Atoi(m[2])
			}
			right = c
			if d > 0 {
				hunks = append(hunks, Hunk{c, c + d - 1})
			}
			continue
		}
		if l == "" || right == 0 {
			continue
		}
		switch l[0] {
		case '+':
			added = append(added, right)
			right++
		case ' ':
			right++
		}
	}
	return hunks, added
}
