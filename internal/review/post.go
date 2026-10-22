package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/tracker"
)

// GitHub is what reviews need from GitHub, done on the host with the user's
// gh login: the agent reads the discussion from files and writes its review to
// a file, so it never needs the GitHub API itself.
type GitHub interface {
	Discussion(ctx context.Context, ref github.PRRef) ([]github.DiscussionFile, error)
	CreateReview(ctx context.Context, ref github.PRRef, file string) (url string, err error)
	ReplyToComment(ctx context.Context, ref github.PRRef, commentID int64, body string) error
}

// ReviewFile is what the agent writes to /out/review.json.
type ReviewFile struct {
	Event    string          `json:"event"` // APPROVE | REQUEST_CHANGES | COMMENT ("" = replies only)
	Body     string          `json:"body"`
	Comments []InlineComment `json:"comments"`
	Replies  []Reply         `json:"replies"`
}

type InlineComment struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Side      string `json:"side,omitempty"` // RIGHT (default) | LEFT
	StartLine int    `json:"start_line,omitempty"`
	StartSide string `json:"start_side,omitempty"`
	Body      string `json:"body"`
}

type Reply struct {
	CommentID int64  `json:"comment_id"` // an inline review comment's id
	Body      string `json:"body"`
}

// Posted records what waterboy posted for a run.
type Posted struct {
	URL      string `json:"url,omitempty"`
	Event    string `json:"event,omitempty"`
	Comments int    `json:"comments,omitempty"`
	Replies  int    `json:"replies,omitempty"`
	Fallback bool   `json:"fallback,omitempty"` // GitHub refused the review; posted as one plain comment
	Error    string `json:"error,omitempty"`
}

const footer = "\n\n" + tracker.WaterboyMark // also how the tracker knows not to count our posts as replies

var events = []string{"APPROVE", "REQUEST_CHANGES", "COMMENT"}

// writeDiscussion puts the PR's discussion into dir for the agent.
func (r *Reviewer) writeDiscussion(ctx context.Context, ref github.PRRef, dir string) error {
	files, err := r.GitHub.Discussion(ctx, ref)
	if err != nil {
		return fmt.Errorf("fetching the PR discussion: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.Name), f.Body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// post publishes the agent's out/review.json, if it wrote one. nil, nil means
// there was nothing to post.
func (r *Reviewer) post(ctx context.Context, run Run) (*Posted, error) {
	// The container wrote out/: os.Root refuses a review.json that is (or
	// passes through) a symlink leading outside it, so no host file gets posted.
	root, err := os.OpenRoot(filepath.Join(run.Dir, "out"))
	if err != nil {
		return nil, err
	}
	defer root.Close()
	b, err := root.ReadFile("review.json")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("review.json: %w", err)
	}
	var f ReviewFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("review.json: %w", err)
	}
	if f.Event != "" && !slices.Contains(events, f.Event) {
		return nil, fmt.Errorf("review.json: event %q isn't one of %s", f.Event, strings.Join(events, ", "))
	}
	ref := run.Job.PR.PRRef
	p := &Posted{Event: f.Event, Comments: len(f.Comments), Replies: len(f.Replies)}
	var errs []error
	if f.Event != "" {
		payload := map[string]any{"commit_id": run.Job.PR.HeadSHA, "event": f.Event, "body": f.Body + footer}
		if len(f.Comments) > 0 {
			payload["comments"] = f.Comments
		}
		url, err := r.createReview(ctx, ref, run.Dir, "post.json", payload)
		if err != nil {
			// e.g. an inline comment outside the diff, or approving your own PR.
			url, err = r.createReview(ctx, ref, run.Dir, "post-fallback.json", map[string]any{
				"commit_id": run.Job.PR.HeadSHA, "event": "COMMENT", "body": folded(f, err) + footer})
			p.Fallback = err == nil
		}
		p.URL = url
		errs = append(errs, err)
	}
	for _, rep := range f.Replies {
		if err := r.GitHub.ReplyToComment(ctx, ref, rep.CommentID, rep.Body+footer); err != nil {
			errs = append(errs, fmt.Errorf("reply to comment %d: %w", rep.CommentID, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		p.Error = err.Error()
		return p, err
	}
	return p, nil
}

func (r *Reviewer) createReview(ctx context.Context, ref github.PRRef, dir, name string, payload map[string]any) (string, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, name)
	if err := os.WriteFile(file, b, 0o644); err != nil {
		return "", err
	}
	return r.GitHub.CreateReview(ctx, ref, file)
}

// githubReason keeps only GitHub's own reason from a gh error (status and
// message), not the command line, local paths or anything else from the host.
var githubReason = regexp.MustCompile(`[^:]*\(HTTP \d+\)|HTTP \d+[^\n]*`)

// folded turns a review GitHub refused into one comment's text.
func folded(f ReviewFile, why error) string {
	reason := strings.TrimSpace(githubReason.FindString(why.Error()))
	if reason == "" {
		reason = "unknown error"
	}
	reason = strings.TrimPrefix(reason, "gh: ")
	var sb strings.Builder
	fmt.Fprintf(&sb, "**Verdict: %s** (posted as a comment: GitHub refused the review: %s)\n\n%s", f.Event, reason, f.Body)
	for _, c := range f.Comments {
		fmt.Fprintf(&sb, "\n\n**`%s:%d`**\n%s", c.Path, c.Line, c.Body)
	}
	return sb.String()
}
