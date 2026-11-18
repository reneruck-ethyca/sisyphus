package github

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
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

// CreatePR opens a PR and returns its ref. Labels go on after, as gh did:
// if that fails the PR exists anyway and the error says so.
func (a *API) CreatePR(ctx context.Context, repo string, pr NewPR) (PRRef, error) {
	var p struct {
		Number int `json:"number"`
	}
	in := map[string]any{"base": pr.Base, "head": pr.Head, "title": pr.Title, "body": pr.Body, "draft": pr.Draft}
	if err := a.send(ctx, http.MethodPost, "repos/"+repo+"/pulls", in, &p); err != nil {
		return PRRef{}, err
	}
	ref := PRRef{Repo: repo, Number: p.Number}
	if len(pr.Labels) > 0 {
		if err := a.send(ctx, http.MethodPost, issuePath(ref)+"/labels", map[string]any{"labels": pr.Labels}, nil); err != nil {
			return ref, fmt.Errorf("opened %s but labelling it failed: %w", ref.Key(), err)
		}
	}
	return ref, nil
}

func pullPath(ref PRRef) string  { return fmt.Sprintf("repos/%s/pulls/%d", ref.Repo, ref.Number) }
func issuePath(ref PRRef) string { return fmt.Sprintf("repos/%s/issues/%d", ref.Repo, ref.Number) }

// MarkReady takes a draft PR out of draft; a PR that isn't one is left alone.
func (a *API) MarkReady(ctx context.Context, ref PRRef) error {
	var p struct {
		NodeID string `json:"node_id"`
		Draft  bool   `json:"draft"`
	}
	if err := a.get(ctx, pullPath(ref), &p); err != nil || !p.Draft {
		return err
	}
	return a.graphql(ctx, `mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){clientMutationId}}`,
		map[string]any{"id": p.NodeID}, nil)
}

// EditPR applies e; with nothing to change it doesn't call GitHub. A reviewer
// written org/team is requested as a team.
func (a *API) EditPR(ctx context.Context, ref PRRef, e PREdit) error {
	patch := map[string]any{}
	if e.Base != "" {
		patch["base"] = e.Base
	}
	if e.Body != nil {
		patch["body"] = *e.Body
	}
	if len(patch) > 0 {
		if err := a.send(ctx, http.MethodPatch, pullPath(ref), patch, nil); err != nil {
			return err
		}
	}
	for _, r := range []struct {
		method string
		who    []string
	}{{http.MethodPost, e.AddReviewers}, {http.MethodDelete, e.RemoveReviewers}} {
		if len(r.who) == 0 {
			continue
		}
		if err := a.send(ctx, r.method, pullPath(ref)+"/requested_reviewers", reviewers(r.who), nil); err != nil {
			return err
		}
	}
	if len(e.AddLabels) > 0 {
		if err := a.send(ctx, http.MethodPost, issuePath(ref)+"/labels", map[string]any{"labels": e.AddLabels}, nil); err != nil {
			return err
		}
	}
	for _, l := range e.RemoveLabels {
		// A label that's already off is what we wanted.
		if err := a.send(ctx, http.MethodDelete, issuePath(ref)+"/labels/"+url.PathEscape(l), nil, nil); err != nil && !isStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return nil
}

func reviewers(who []string) map[string][]string {
	m := map[string][]string{"reviewers": {}, "team_reviewers": {}}
	for _, w := range who {
		if _, team, ok := strings.Cut(w, "/"); ok {
			m["team_reviewers"] = append(m["team_reviewers"], team)
		} else {
			m["reviewers"] = append(m["reviewers"], w)
		}
	}
	return m
}

// Comment posts a comment on the PR's conversation and returns its id.
func (a *API) Comment(ctx context.Context, ref PRRef, body string) (int64, error) {
	var c struct {
		ID int64 `json:"id"`
	}
	return c.ID, a.send(ctx, http.MethodPost, issuePath(ref)+"/comments", map[string]string{"body": body}, &c)
}

// CreateLabel makes the label, or updates it if it exists.
func (a *API) CreateLabel(ctx context.Context, repo, name, color, description string) error {
	in := map[string]string{"name": name, "color": color, "description": description}
	err := a.send(ctx, http.MethodPost, "repos/"+repo+"/labels", in, nil)
	if isStatus(err, http.StatusUnprocessableEntity) { // already_exists
		delete(in, "name")
		err = a.send(ctx, http.MethodPatch, "repos/"+repo+"/labels/"+url.PathEscape(name), in, nil)
	}
	return err
}

// RerunFailed reruns the failed jobs of a workflow run.
func (a *API) RerunFailed(ctx context.Context, repo string, runID int64) error {
	return a.send(ctx, http.MethodPost, fmt.Sprintf("repos/%s/actions/runs/%d/rerun-failed-jobs", repo, runID), nil, nil)
}

// FindPR returns the open PR whose head is branch, if there is one: how a
// handoff adopts a PR it opened before a crash.
func (a *API) FindPR(ctx context.Context, repo, branch string) (PRRef, bool, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var prs []struct {
		Number int `json:"number"`
		Head   struct {
			Repo *ghRepo `json:"repo"`
		} `json:"head"`
	}
	q := url.Values{"state": {"open"}, "head": {owner + ":" + branch}}
	if err := a.get(ctx, "repos/"+repo+"/pulls?"+q.Encode(), &prs); err != nil {
		return PRRef{}, false, err
	}
	// Only our own branch's PR is ours to adopt, never a fork's.
	for _, p := range prs {
		if p.Head.Repo != nil && p.Head.Repo.FullName == repo {
			return PRRef{Repo: repo, Number: p.Number}, true, nil
		}
	}
	return PRRef{}, false, nil
}

// jobStep is a step of an Actions job, as the jobs API reports it.
type jobStep struct {
	Name        string    `json:"name"`
	Conclusion  string    `json:"conclusion"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

// RunLog returns the log of a job's failed steps, as `gh run view --log-failed` did.
func (a *API) RunLog(ctx context.Context, repo string, _, jobID int64) (string, error) {
	var job struct {
		Steps []jobStep `json:"steps"`
	}
	path := fmt.Sprintf("repos/%s/actions/jobs/%d", repo, jobID)
	if err := a.get(ctx, path, &job); err != nil {
		return "", err
	}
	log, _, err := a.do(ctx, http.MethodGet, path+"/logs", nil)
	if err != nil {
		return "", err
	}
	return failedSteps(string(log), job.Steps), nil
}

// failedSteps keeps the log lines written while a failed step ran, told apart by
// each line's leading timestamp; a line without one goes with the line before.
// With no failed step, or none of its lines found, it returns the whole log.
// ponytail: step times are to the second, so a line from the second a failed
// step ended may belong to the next step; harmless for a failure tail.
func failedSteps(log string, steps []jobStep) string {
	var failed []jobStep
	for _, s := range steps {
		if s.Conclusion == "failure" && !s.StartedAt.IsZero() {
			failed = append(failed, s)
		}
	}
	if len(failed) == 0 {
		return log
	}
	var b strings.Builder
	keep := false
	sc := bufio.NewScanner(strings.NewReader(log))
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if stamp, _, ok := strings.Cut(strings.TrimPrefix(line, "\ufeff"), " "); ok {
			if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
				keep = false
				for _, s := range failed {
					if !t.Before(s.StartedAt) && t.Before(s.CompletedAt.Add(time.Second)) {
						keep = true
					}
				}
			}
		}
		if keep {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	if b.Len() == 0 {
		return log
	}
	return b.String()
}

// MergeSquash squash-merges the PR if its head is still head; a head that
// moved is GitHub's 409.
func (a *API) MergeSquash(ctx context.Context, ref PRRef, head string) error {
	return a.send(ctx, http.MethodPut, pullPath(ref)+"/merge", map[string]string{"merge_method": "squash", "sha": head}, nil)
}

// Enqueue puts the PR (by GraphQL node id) in its base's merge queue.
func (a *API) Enqueue(ctx context.Context, nodeID string) error {
	return a.graphql(ctx, `mutation($id:ID!){enqueuePullRequest(input:{pullRequestId:$id}){clientMutationId}}`,
		map[string]any{"id": nodeID}, nil)
}

// EnableAutoMerge has GitHub squash-merge the PR once its requirements are met.
func (a *API) EnableAutoMerge(ctx context.Context, nodeID string) error {
	return a.graphql(ctx, `mutation($id:ID!){enablePullRequestAutoMerge(input:{pullRequestId:$id,mergeMethod:SQUASH}){clientMutationId}}`,
		map[string]any{"id": nodeID}, nil)
}

// DisableAutoMerge turns GitHub's auto-merge off for the PR.
func (a *API) DisableAutoMerge(ctx context.Context, nodeID string) error {
	return a.graphql(ctx, `mutation($id:ID!){disablePullRequestAutoMerge(input:{pullRequestId:$id}){clientMutationId}}`,
		map[string]any{"id": nodeID}, nil)
}

// QueueFailures returns the failed jobs of the PR's latest merge-queue attempt:
// the newest completed merge_group runs on its gh-readonly-queue branch.
// ponytail: looks at the 100 newest merge_group runs of the repo; a busy queue may push the PR's off the page.
func (a *API) QueueFailures(ctx context.Context, repo, base string, number int) ([]Check, error) {
	var runs struct {
		Runs []struct {
			ID         int64  `json:"id"`
			Name       string `json:"name"`
			HeadBranch string `json:"head_branch"`
			HeadSHA    string `json:"head_sha"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}
	q := url.Values{"event": {"merge_group"}, "status": {"completed"}, "per_page": {"100"}}
	if err := a.get(ctx, "repos/"+repo+"/actions/runs?"+q.Encode(), &runs); err != nil {
		return nil, err
	}
	prefix := fmt.Sprintf("gh-readonly-queue/%s/pr-%d-", base, number)
	var checks []Check
	latest := "" // the runs come newest first; only the newest attempt's head counts
	for _, r := range runs.Runs {
		if !strings.HasPrefix(r.HeadBranch, prefix) || (latest != "" && r.HeadSHA != latest) {
			continue
		}
		latest = r.HeadSHA
		if r.Conclusion == "success" || r.Conclusion == "skipped" {
			continue
		}
		var jobs struct {
			Jobs []struct {
				Name       string `json:"name"`
				Conclusion string `json:"conclusion"`
				HTMLURL    string `json:"html_url"`
			} `json:"jobs"`
		}
		if err := a.get(ctx, fmt.Sprintf("repos/%s/actions/runs/%d/jobs?filter=latest&per_page=100", repo, r.ID), &jobs); err != nil {
			return nil, err
		}
		for _, j := range jobs.Jobs {
			if j.Conclusion == "failure" || j.Conclusion == "timed_out" {
				checks = append(checks, Check{Name: j.Name, Workflow: r.Name, Status: "COMPLETED", Conclusion: strings.ToUpper(j.Conclusion), URL: j.HTMLURL})
			}
		}
	}
	return checks, nil
}
