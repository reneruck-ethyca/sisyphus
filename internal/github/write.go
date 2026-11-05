package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/reneruck/sisyphus/internal/proc"
)

// NewPR is a PR to open: Head is a branch already pushed to the repo.
type NewPR struct {
	Base, Head, Title, Body string
	Draft                   bool
	Labels                  []string // must exist: see CreateLabel
}

// PREdit is a change to an open PR; zero fields change nothing.
type PREdit struct {
	AddReviewers, RemoveReviewers, AddLabels, RemoveLabels []string
	Base                                                   string
	Body                                                   *string
}

// CreatePR opens a PR and returns its ref.
func (CLI) CreatePR(ctx context.Context, repo string, pr NewPR) (PRRef, error) {
	out, err := proc.Run(ctx, "", "gh", createPRArgs(repo, pr)...)
	if err != nil {
		return PRRef{}, err
	}
	return parsePRURL(out)
}

func createPRArgs(repo string, pr NewPR) []string {
	args := []string{"pr", "create", "--repo", repo, "--base", pr.Base, "--head", pr.Head, "--title", pr.Title, "--body", pr.Body}
	if pr.Draft {
		args = append(args, "--draft")
	}
	for _, l := range pr.Labels {
		args = append(args, "--label", l)
	}
	return args
}

// parsePRURL reads the PR gh printed: its last line is the URL.
func parsePRURL(out string) (PRRef, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	bad := fmt.Errorf("gh pr create: no PR URL in %q", out)
	u, err := url.Parse(last)
	if err != nil {
		return PRRef{}, bad
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/") // owner, name, "pull", n
	if len(p) != 4 || p[2] != "pull" {
		return PRRef{}, bad
	}
	n, err := strconv.Atoi(p[3])
	if err != nil {
		return PRRef{}, bad
	}
	return PRRef{Repo: p[0] + "/" + p[1], Number: n}, nil
}

// MarkReady takes a draft PR out of draft.
func (CLI) MarkReady(ctx context.Context, ref PRRef) error {
	_, err := proc.Run(ctx, "", "gh", "pr", "ready", strconv.Itoa(ref.Number), "--repo", ref.Repo)
	return err
}

// EditPR applies e; with nothing to change it doesn't call gh.
func (CLI) EditPR(ctx context.Context, ref PRRef, e PREdit) error {
	args := editArgs(ref, e)
	if args == nil {
		return nil
	}
	_, err := proc.Run(ctx, "", "gh", args...)
	return err
}

func editArgs(ref PRRef, e PREdit) []string {
	args := []string{"pr", "edit", strconv.Itoa(ref.Number), "--repo", ref.Repo}
	n := len(args)
	if e.Base != "" {
		args = append(args, "--base", e.Base)
	}
	if e.Body != nil {
		args = append(args, "--body", *e.Body)
	}
	for _, f := range []struct {
		flag string
		vals []string
	}{{"--add-reviewer", e.AddReviewers}, {"--remove-reviewer", e.RemoveReviewers}, {"--add-label", e.AddLabels}, {"--remove-label", e.RemoveLabels}} {
		if len(f.vals) > 0 {
			args = append(args, f.flag, strings.Join(f.vals, ","))
		}
	}
	if len(args) == n {
		return nil
	}
	return args
}

// Comment posts a comment on the PR's conversation.
func (CLI) Comment(ctx context.Context, ref PRRef, body string) error {
	_, err := proc.Run(ctx, "", "gh", "pr", "comment", strconv.Itoa(ref.Number), "--repo", ref.Repo, "--body", body)
	return err
}

// CreateLabel makes the label, or updates it if it exists.
func (CLI) CreateLabel(ctx context.Context, repo, name, color, description string) error {
	_, err := proc.Run(ctx, "", "gh", "label", "create", name, "--repo", repo, "--force", "--color", color, "--description", description)
	return err
}

// RerunFailed reruns the failed jobs of a workflow run.
func (CLI) RerunFailed(ctx context.Context, repo string, runID int64) error {
	_, err := proc.Run(ctx, "", "gh", "run", "rerun", strconv.FormatInt(runID, 10), "--failed", "--repo", repo)
	return err
}

// FindPR returns the open PR whose head is branch, if there is one: how a
// handoff adopts a PR it opened before a crash.
func (CLI) FindPR(ctx context.Context, repo, branch string) (PRRef, bool, error) {
	out, err := proc.Run(ctx, "", "gh", "pr", "list", "--repo", repo, "--head", branch, "--state", "open", "--json", "number,isCrossRepository", "--limit", "20")
	if err != nil {
		return PRRef{}, false, err
	}
	return parsePRList(repo, out)
}

func parsePRList(repo, out string) (PRRef, bool, error) {
	var prs []struct {
		Number int  `json:"number"`
		Fork   bool `json:"isCrossRepository"`
	}
	if err := json.Unmarshal([]byte(out), &prs); err != nil {
		return PRRef{}, false, fmt.Errorf("parse pr list: %w", err)
	}
	// --head matches a fork's branch of the same name too; only our own branch's PR is ours to adopt.
	for _, p := range prs {
		if !p.Fork {
			return PRRef{Repo: repo, Number: p.Number}, true, nil
		}
	}
	return PRRef{}, false, nil
}

// RunLog returns the log of a job's failed steps (`gh run view --log-failed`).
func (CLI) RunLog(ctx context.Context, repo string, runID, jobID int64) (string, error) {
	return proc.Run(ctx, "", "gh", "run", "view", strconv.FormatInt(runID, 10), "--job", strconv.FormatInt(jobID, 10), "--repo", repo, "--log-failed")
}
