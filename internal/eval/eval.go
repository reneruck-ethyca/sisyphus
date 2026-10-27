// Package eval replays reviews of past PRs against gold cases: the review a PR
// got (from people, or a run we trust), written down as the findings it raised.
// Each run reviews the PR at the head the gold review saw, with only the
// discussion from before it; a judge then scores the run against the case.
package eval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// Case is one gold review, a JSON file.
type Case struct {
	PR      string    `json:"pr"`             // owner/repo#n
	SHA     string    `json:"sha"`            // the head the gold review saw
	Cutoff  time.Time `json:"cutoff"`         // when it was written: later discussion is hidden
	Base    string    `json:"base,omitempty"` // diff base; "" = merge-base with the PR's base branch
	Verdict string    `json:"verdict"`        // APPROVE | REQUEST_CHANGES | COMMENT
	// Findings the gold review raised; NonIssues look like problems but aren't
	// (reporting one is a false positive).
	Findings  []Finding `json:"findings"`
	NonIssues []Finding `json:"non_issues,omitempty"`
	Notes     string    `json:"notes,omitempty"` // for the judge: intent, context the PR alone doesn't give

	File string `json:"-"`
}

type Finding struct {
	ID     string `json:"id"`
	Weight string `json:"weight,omitempty"` // must | should | nice (findings only)
	Where  string `json:"where,omitempty"`  // path:lines
	What   string `json:"what"`
	Source string `json:"source,omitempty"` // the gold comment or fix commit
}

func (c Case) ref() (github.PRRef, error) {
	repo, n, ok := strings.Cut(c.PR, "#")
	num, err := strconv.Atoi(n)
	if !ok || err != nil || !strings.Contains(repo, "/") {
		return github.PRRef{}, fmt.Errorf("%s: pr %q isn't owner/repo#n", c.File, c.PR)
	}
	return github.PRRef{Repo: repo, Number: num}, nil
}

// LoadCases reads a case file, or every *.json in a directory.
func LoadCases(p string) ([]Case, error) {
	files := []string{p}
	if st, err := os.Stat(p); err != nil {
		return nil, err
	} else if st.IsDir() {
		if files, err = filepath.Glob(filepath.Join(p, "*.json")); err != nil {
			return nil, err
		}
	}
	var cases []Case
	for _, f := range files {
		c, err := loadCase(f)
		if err != nil {
			return nil, err
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("no cases in %s", p)
	}
	return cases, nil
}

func loadCase(f string) (Case, error) {
	var c Case
	b, err := os.ReadFile(f)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil {
		return c, fmt.Errorf("%s: %w", f, err)
	}
	c.File, _ = filepath.Abs(f)
	if _, err := c.ref(); err != nil {
		return c, err
	}
	if len(c.SHA) < 7 || c.Cutoff.IsZero() {
		return c, fmt.Errorf("%s: needs sha and cutoff", f)
	}
	return c, nil
}

// Runner runs cases with one sisyphus config: the variant being measured
// (image, prompt, model, turn limit all come from it).
type Runner struct {
	Cfg   config.Config
	RT    container.Runtime
	GH    github.CLI
	Locks *tracker.Locks
	Out   string // runs land in Out/runs/<owner>/<repo>/<n>/eval-<sha>-r<k>/
	Reps  int
}

// Run runs every case Reps times, one run at a time, then judges them. A
// case that can't be set up is skipped; the error says which.
func (e Runner) Run(ctx context.Context, cases []Case) error {
	rev, err := review.New(e.Cfg.Review, e.RT, e.Locks, e.Out)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range cases {
		if err := e.runCase(ctx, rev, c); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", c.PR, err))
		}
		if ctx.Err() != nil {
			break
		}
	}
	return errors.Join(append(errs, Judge(ctx, e.Out, false))...)
}

type pull struct {
	Title          string `json:"title"`
	URL            string `json:"html_url"`
	MergeCommitSHA string `json:"merge_commit_sha"`
	User           struct {
		Login string `json:"login"`
	} `json:"user"`
	Head struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (e Runner) runCase(ctx context.Context, rev *review.Reviewer, c Case) error {
	ref, _ := c.ref()
	out, err := proc.Run(ctx, "", "gh", "api", fmt.Sprintf("repos/%s/pulls/%d", ref.Repo, ref.Number))
	if err != nil {
		return err
	}
	var p pull
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		return err
	}
	repoDir := filepath.Join(e.Cfg.ReposRoot, path.Base(ref.Repo))
	sha, base, leaks, err := prepareClone(ctx, repoDir, ref, c, p)
	if err != nil {
		return err
	}
	branch := fmt.Sprintf("sisyphus-eval/%d-%.12s", ref.Number, sha)
	wt, err := proc.Run(ctx, repoDir, "wtp", "add", "-b", branch, sha, "--quiet")
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	wt = strings.TrimSpace(wt[strings.LastIndex(wt, "\n")+1:]) // hooks may print too
	defer func() {
		if err := worktree.Remove(context.WithoutCancel(ctx), repoDir, branch, true); err != nil {
			slog.Warn("eval: removing worktree", "branch", branch, "err", err)
		}
	}()
	tr := tracker.Tracked{PRRef: ref, URL: p.URL, Title: p.Title, Author: p.User.Login,
		HeadRef: p.Head.Ref, HeadSHA: sha, Worktree: wt}
	rev.GitHub = before{GH: e.GH, Cutoff: c.Cutoff, Head: sha, Base: base}

	for k := 1; k <= max(e.Reps, 1) && ctx.Err() == nil; k++ {
		if err := worktree.Reset(ctx, wt, sha); err != nil {
			return err
		}
		id := fmt.Sprintf("eval-%.12s-r%d", sha, k)
		slog.Info("eval: run", "pr", c.PR, "run", id)
		if err := e.runOnce(ctx, rev, review.Job{PR: tr, Reason: "new", Base: base}, id, c, leaks); err != nil {
			slog.Error("eval: run failed", "pr", c.PR, "run", id, "err", err)
		}
	}
	return nil
}

// prepareClone makes sure the case's head is in the clone and works out the
// diff base, plus the commits a run must not see: the PR's later commits and
// its merge. Those stay fetchable from the clone, so the judge checks each
// run's log for them instead.
func prepareClone(ctx context.Context, dir string, ref github.PRRef, c Case, p pull) (sha, base string, leaks []string, err error) {
	tmp := fmt.Sprintf("refs/sisyphus-eval/%d", ref.Number)
	if _, err = proc.Run(ctx, dir, "git", "fetch", "--quiet", "origin",
		fmt.Sprintf("+refs/pull/%d/head:%s", ref.Number, tmp), "+refs/heads/"+p.Base.Ref+":refs/remotes/origin/"+p.Base.Ref); err != nil {
		return "", "", nil, err
	}
	defer proc.Run(context.WithoutCancel(ctx), dir, "git", "update-ref", "-d", tmp)
	if sha, err = proc.Run(ctx, dir, "git", "rev-parse", "--verify", "--quiet", c.SHA+"^{commit}"); err != nil {
		if _, err = proc.Run(ctx, dir, "git", "fetch", "--quiet", "origin", c.SHA); err != nil {
			return "", "", nil, fmt.Errorf("sha %s isn't on the remote: %w", c.SHA, err)
		}
		if sha, err = proc.Run(ctx, dir, "git", "rev-parse", "--verify", c.SHA+"^{commit}"); err != nil {
			return "", "", nil, err
		}
	}
	// Not what came in from the base branch since: that's no hint.
	if later, err := proc.Run(ctx, dir, "git", "rev-list", sha+".."+tmp, "^origin/"+p.Base.Ref); err == nil {
		leaks = strings.Fields(later)
	}
	if p.MergeCommitSHA != "" {
		leaks = append(leaks, p.MergeCommitSHA)
	}
	if base = c.Base; base == "" {
		if base, err = proc.Run(ctx, dir, "git", "merge-base", sha, "origin/"+p.Base.Ref); err != nil {
			return "", "", nil, err
		}
		if base == sha { // merged with a merge commit: the branch is in its base
			return "", "", nil, fmt.Errorf("%.12s is already in %s; set base in the case", sha, p.Base.Ref)
		}
	}
	return sha, base, leaks, nil
}

func (e Runner) runOnce(ctx context.Context, rev *review.Reviewer, job review.Job, id string, c Case, leaks []string) error {
	run, spec, stop, err := rev.Eval(ctx, job, id)
	defer stop()
	if err != nil {
		return err
	}
	meta, _ := json.MarshalIndent(runMeta{Case: c.File, Gold: c, Leaks: leaks, Started: time.Now()}, "", "  ")
	if err := os.WriteFile(filepath.Join(run.Dir, "eval.json"), meta, 0o644); err != nil {
		return err
	}
	log, err := os.Create(filepath.Join(run.Dir, "log"))
	if err != nil {
		return err
	}
	defer log.Close()
	cid, err := e.RT.Start(ctx, spec)
	if err != nil {
		return err
	}
	defer e.RT.Remove(context.WithoutCancel(ctx), cid)
	if err := e.RT.Logs(ctx, cid, log); err != nil && ctx.Err() == nil {
		slog.Warn("eval: logs", "run", id, "err", err)
	}
	if code, err := e.RT.Wait(ctx, cid); err != nil || code != 0 {
		return fmt.Errorf("container exited %d: %v (log: %s)", code, err, log.Name())
	}
	return nil
}

// runMeta is eval.json in a run dir: what the judge needs besides the run.
type runMeta struct {
	Case    string    `json:"case"` // the case file; the judge re-reads it, so edited gold applies
	Gold    Case      `json:"gold"` // the case as run, if the file is gone
	Leaks   []string  `json:"leaks"`
	Started time.Time `json:"started"`
}

// before is the discussion as it stood at Cutoff, for the run's context dir.
type before struct {
	GH         github.CLI
	Cutoff     time.Time
	Head, Base string
}

func (b before) Discussion(ctx context.Context, ref github.PRRef) ([]github.DiscussionFile, error) {
	files, err := b.GH.Discussion(ctx, ref)
	if err != nil {
		return nil, err
	}
	for i, f := range files {
		if files[i].Body, err = filterDiscussion(f.Name, f.Body, b.Cutoff, b.Head, b.Base); err != nil {
			return nil, fmt.Errorf("%s: %w", f.Name, err)
		}
	}
	return files, nil
}

func (before) CreateReview(context.Context, github.PRRef, string) (string, error) {
	return "", errors.New("evals don't post")
}

func (before) ReplyToComment(context.Context, github.PRRef, int64, string) error {
	return errors.New("evals don't post")
}

// filterDiscussion drops what came after cutoff: comments and reviews by
// date, and from pr.json everything but what the author wrote (state, merge
// commit, later heads).
func filterDiscussion(name string, body []byte, cutoff time.Time, head, base string) ([]byte, error) {
	if name == "pr.json" {
		var p map[string]any
		if err := json.Unmarshal(body, &p); err != nil {
			return nil, err
		}
		keep := map[string]any{"head": map[string]any{"sha": head}, "base": map[string]any{"sha": base}}
		for _, k := range []string{"number", "title", "body", "html_url", "user", "created_at", "labels", "draft"} {
			keep[k] = p[k]
		}
		for _, side := range []string{"head", "base"} {
			if m, ok := p[side].(map[string]any); ok {
				keep[side].(map[string]any)["ref"] = m["ref"]
			}
		}
		return json.MarshalIndent(keep, "", "  ")
	}
	var items []map[string]any
	if err := json.Unmarshal(body, &items); err != nil {
		return nil, err
	}
	kept := []map[string]any{}
	for _, it := range items {
		at, _ := it["submitted_at"].(string)
		if at == "" {
			at, _ = it["created_at"].(string)
		}
		if t, err := time.Parse(time.RFC3339, at); err == nil && t.Before(cutoff) {
			kept = append(kept, it)
		}
	}
	return json.MarshalIndent(kept, "", "  ")
}
