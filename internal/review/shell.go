package review

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/tracker"
)

// Shell prepares the container a review of tr would run in (image, mounts, env
// files, stack and its network, prompt and discussion) with an interactive bash
// in place of the review, for `waterboy -shell`. The review's scripts are at
// /waterboy/setup.sh and /waterboy/review.sh; nothing is posted. Make r on a
// scratch dir: it records the stack, which must not touch the app's reviews.json.
// stop stops the stack again (keeping its volumes, as after a review).
func (r *Reviewer) Shell(ctx context.Context, tr tracker.Tracked) (spec container.Spec, stop func(), err error) {
	stop = func() {}
	repo, ok := r.cfg.Repos[tr.Repo]
	if !ok {
		return spec, stop, fmt.Errorf("no review.repos entry for %s", tr.Repo)
	}
	if f := r.envFilesMissing(tr.Repo, repo); f != "" {
		return spec, stop, fmt.Errorf("env file missing: %s", f)
	}
	// Its stack would be re-upped and stopped under it.
	cs, err := r.rt.List(ctx, map[string]string{"waterboy.pr": tr.Key()})
	if err != nil {
		return spec, stop, err
	}
	for _, c := range cs {
		if c.Running {
			return spec, stop, fmt.Errorf("a review of %s is running (container %.12s); try again when it's done", tr.Key(), c.ID)
		}
	}
	runID := "shell-" + time.Now().UTC().Format("20060102-150405")
	run := Run{ID: runID, Job: Job{PR: tr, Reason: "new"}, Dir: filepath.Join(r.dir, "runs", prPath(tr.Key()), runID)}
	if st := repo.Stack; st != nil {
		sr, err := r.startStack(ctx, run, st)
		stop = func() { r.stopStack(sr.Project) }
		if err != nil {
			return spec, stop, err
		}
		run.Stack = sr
	}
	if spec, err = r.spec(ctx, run); err != nil {
		return spec, stop, err
	}
	review := spec.Command[2]
	for name, script := range map[string]string{"setup.sh": strings.TrimSuffix(review, r.claudeScript()), "review.sh": review} {
		path := filepath.Join(run.Dir, name)
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			return spec, stop, err
		}
		spec.Mounts = append(spec.Mounts, container.Mount{Source: path, Target: "/waterboy/" + name, ReadOnly: true})
	}
	spec.Command = []string{"bash", "-c", fmt.Sprintf(`echo %s
echo '  bash /waterboy/setup.sh    the repo setup, as a review runs it'
echo '  bash /waterboy/review.sh   setup, then Claude on /waterboy/prompt.md (nothing is posted)'
exec bash`, shellQuote(fmt.Sprintf("waterboy shell: %s at %.12s, in %s", tr.Key(), tr.HeadSHA, spec.Image)))}
	// Not "waterboy": the app removes those containers when it has no run for them.
	spec.Labels = map[string]string{"waterboy.shell": tr.Key()}
	return spec, stop, nil
}
