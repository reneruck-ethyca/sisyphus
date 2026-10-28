package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// Shell prepares the container a review of tr would run in (image, mounts, env
// files, stack and its network, prompt and discussion) with an interactive bash
// in place of the review, for `sisyphus -shell`. The review's scripts are at
// /sisyphus/setup.sh and /sisyphus/review.sh; nothing is posted. Make r on a
// scratch dir: it records the stack, which must not touch the app's reviews.json.
// stop stops the stack again (keeping its volumes, as after a review).
func (r *Reviewer) Shell(ctx context.Context, tr tracker.Tracked) (spec container.Spec, stop func(), err error) {
	run, spec, stop, err := r.prepare(ctx, Job{PR: tr, Reason: ReasonNew}, "shell-"+time.Now().UTC().Format("20060102-150405"))
	if err != nil {
		return spec, stop, err
	}
	review := spec.Command[2]
	for name, script := range map[string]string{"setup.sh": strings.TrimSuffix(review, r.claudeScript()), "review.sh": review} {
		path := filepath.Join(run.Dir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			return spec, stop, err
		}
		spec.Mounts = append(spec.Mounts, container.Mount{Source: path, Target: "/sisyphus/" + name, ReadOnly: true})
	}
	spec.Command = []string{"bash", "-c", fmt.Sprintf(`echo %s
echo '  bash /sisyphus/setup.sh    the repo setup, as a review runs it'
echo '  bash /sisyphus/review.sh   setup, then Claude on /sisyphus/prompt.md (nothing is posted)'
exec bash`, shellQuote(fmt.Sprintf("sisyphus shell: %s at %.12s, in %s", tr.Key(), tr.HeadSHA, spec.Image)))}
	// Not "sisyphus": the app removes those containers when it has no run for them.
	spec.Labels = map[string]string{"sisyphus.shell": tr.Key()}
	return spec, stop, nil
}

// prepare starts job's stack and builds the spec of a run with id runID, as a
// review would, without recording anything in the app's state.
func (r *Reviewer) prepare(ctx context.Context, job Job, runID string) (run Run, spec container.Spec, stop func(), err error) {
	stop = func() {}
	tr := job.PR
	repo, ok := r.cfg.Repos[tr.Repo]
	if !ok {
		return run, spec, stop, fmt.Errorf("no review.repos entry for %s", tr.Repo)
	}
	if f := r.envFilesMissing(tr.Repo, repo); f != "" {
		return run, spec, stop, fmt.Errorf("env file missing: %s", f)
	}
	// Its stack would be re-upped and stopped under it.
	cs, err := r.rt.List(ctx, map[string]string{"sisyphus.pr": tr.Key()})
	if err != nil {
		return run, spec, stop, err
	}
	for _, c := range cs {
		if c.Running {
			return run, spec, stop, fmt.Errorf("a review of %s is running (container %.12s); try again when it's done", tr.Key(), c.ID)
		}
	}
	run = Run{ID: runID, Job: job, Dir: filepath.Join(r.dir, "runs", prPath(tr.Key()), runID)}
	if st := repo.Stack; st != nil {
		sr, err := r.startStack(ctx, run, st)
		stop = func() { r.stopStack(sr.Project) }
		if err != nil {
			return run, spec, stop, err
		}
		run.Stack = sr
	}
	spec, err = r.spec(ctx, run)
	return run, spec, stop, err
}

// Eval is a review run for `sisyphus -eval`: job.PR's worktree must be at its
// HeadSHA, and job.Base set. Claude gets no GitHub token (setup still does), so
// it can't read what happened on the PR after the review being replayed.
// Nothing is posted; the output stays in the returned run's Dir.
func (r *Reviewer) Eval(ctx context.Context, job Job, runID string) (run Run, spec container.Spec, stop func(), err error) {
	run, spec, stop, err = r.prepare(ctx, job, runID)
	if err != nil {
		return run, spec, stop, err
	}
	claude := r.claudeScript()
	spec.Command[2] = strings.TrimSuffix(spec.Command[2], claude) + "unset GH_TOKEN GITHUB_TOKEN\n" + claude
	spec.Labels = map[string]string{"sisyphus.eval": job.PR.Key()} // not "sisyphus", as with Shell
	return run, spec, stop, nil
}
