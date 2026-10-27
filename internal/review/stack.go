package review

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// StackRun is the local stack (DB, cache) a run started next to its container.
type StackRun struct {
	Project string `json:"project"`          // compose project; the container joins <project>_default
	Dir     string `json:"dir"`              // where up ran: the worktree, or the safe checkout
	Offset  int    `json:"offset,omitempty"` // PORT_OFFSET sisyphus set; 0 = the stack picked its own
}

// Network is the compose project's default network.
func (s StackRun) Network() string { return s.Project + "_default" }

// Tunables (vars so tests can shorten them).
var (
	stackUpTimeout   = 15 * time.Minute // a first up may pull images
	stackStopTimeout = 2 * time.Minute
	stackAttempts    = 10 // ups per run; only port clashes are retried
)

// shell runs a host-side script in dir; Reviewer.sh, replaced in tests.
func shell(ctx context.Context, dir string, env []string, script string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, "bash", "-c", script)
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = dir, append(os.Environ(), env...), out, out
	return cmd.Run()
}

// stackID is the WORKTREE_ID a PR's stack runs under: the same in both stack
// modes, and free of branch names (which the stack would cut to 20 chars).
func stackID(tr tracker.Tracked) string { return "pr-" + strconv.Itoa(tr.Number) }

// startStack runs the repo's stack up for the PR, from the worktree or the safe
// checkout, with its output in the run's stack.log. On a host port clash it
// retries with the next PORT_OFFSET and remembers the one that worked. The
// returned StackRun is set even on failure: a half-started stack still needs stopping.
func (r *Reviewer) startStack(ctx context.Context, run Run, st *config.Stack) (*StackRun, error) {
	tr := run.Job.PR
	sr := &StackRun{Project: strings.ReplaceAll(st.Project, "{id}", stackID(tr)), Dir: tr.Worktree}
	if cmp.Or(st.From, r.cfg.StackFrom) == config.StackFromSafe {
		sr.Dir = st.SafeDir
	}
	if err := os.MkdirAll(run.Dir, 0o755); err != nil {
		return sr, err
	}
	logf, err := os.Create(filepath.Join(run.Dir, "stack.log"))
	if err != nil {
		return sr, err
	}
	defer logf.Close()
	r.mu.Lock()
	offset := 0
	if prev := r.st.Stacks[tr.Key()]; prev != nil && prev.Project == sr.Project {
		offset = prev.Offset
	}
	r.st.Stacks[tr.Key()] = sr // before up: a failed up can still leave containers to remove
	r.saveLocked()
	r.mu.Unlock()
	for attempt := 1; ; attempt++ {
		env := []string{"WORKTREE_ID=" + stackID(tr)}
		if offset > 0 {
			env = append(env, "PORT_OFFSET="+strconv.Itoa(offset))
		}
		fmt.Fprintf(logf, "── sisyphus: %s in %s (%s) ──\n", st.Up, sr.Dir, strings.Join(env, " "))
		var out bytes.Buffer
		upCtx, cancel := context.WithTimeout(ctx, stackUpTimeout)
		err := r.sh(upCtx, sr.Dir, env, st.Up, io.MultiWriter(logf, &out))
		cancel()
		if err == nil {
			r.mu.Lock()
			sr.Offset = offset
			r.saveLocked()
			r.mu.Unlock()
			slog.Info("stack up", "pr", tr.Key(), "project", sr.Project, "dir", sr.Dir, "port_offset", offset)
			return sr, nil
		}
		if ctx.Err() != nil || !portClash(out.String()) || attempt == stackAttempts {
			return sr, fmt.Errorf("stack up (see stack.log): %w: %s", err, errorLines(out.String()))
		}
		offset = nextOffset(offset, tr.Number)
		slog.Warn("stack port clash, retrying", "pr", tr.Key(), "project", sr.Project, "port_offset", offset)
	}
}

func portClash(out string) bool {
	return strings.Contains(out, "port is already allocated") || strings.Contains(out, "address already in use")
}

// nextOffset walks the stack's 49 port slots (offsets 100–4900), starting from
// one picked by PR number when the stack chose the first offset itself.
func nextOffset(cur, prNumber int) int {
	if cur <= 0 {
		return 100 * (1 + prNumber%49)
	}
	return cur%4900 + 100
}

// errorLines picks the lines of up's output that say what went wrong (docker's
// "Error response…", nox's "… failed"), else its last lines.
func errorLines(out string) string {
	var errs []string
	for _, l := range strings.Split(out, "\n") {
		if ll := strings.ToLower(l); strings.Contains(ll, "error") || strings.Contains(ll, "failed") {
			errs = append(errs, strings.TrimSpace(l))
		}
	}
	if len(errs) == 0 {
		return lastLines(out, 5)
	}
	return strings.Join(errs[max(0, len(errs)-3):], "\n")
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}

// stopStack stops a run's stack after the run, keeping its containers and
// volumes, so the PR's next review starts on the same DB.
func (r *Reviewer) stopStack(project string) {
	r.compose(project, `docker compose -p "$SISYPHUS_STACK" stop`)
}

// downStack removes a PR's stack: containers, network and volumes.
func (r *Reviewer) downStack(project string) error {
	// Without its compose files, `down --volumes` can't see the named volumes; go by label.
	return r.compose(project, `docker compose -p "$SISYPHUS_STACK" down --remove-orphans &&
vols=$(docker volume ls -q --filter "label=com.docker.compose.project=$SISYPHUS_STACK") &&
{ [ -z "$vols" ] || docker volume rm $vols; }`)
}

func (r *Reviewer) compose(project, script string) error {
	ctx, cancel := context.WithTimeout(context.Background(), stackStopTimeout)
	defer cancel()
	var out bytes.Buffer
	if err := r.sh(ctx, "", []string{"SISYPHUS_STACK=" + project}, script, &out); err != nil {
		slog.Warn("stack command failed", "project", project, "err", err, "output", lastLines(out.String(), 5))
		return fmt.Errorf("stack %s: %w", project, err)
	}
	return nil
}

// removeStacks removes the stacks of the PRs match picks; a stack that won't go
// stays recorded, for the next try.
func (r *Reviewer) removeStacks(match func(prID string) bool) error {
	r.mu.Lock()
	stacks := map[string]string{}
	for id, sr := range r.st.Stacks {
		if match(id) {
			stacks[id] = sr.Project
		}
	}
	r.mu.Unlock()
	var errs []error
	for id, project := range stacks {
		if err := r.downStack(project); err != nil {
			errs = append(errs, err)
			continue
		}
		r.mu.Lock()
		delete(r.st.Stacks, id)
		r.saveLocked()
		r.mu.Unlock()
		slog.Info("stack removed", "pr", id, "project", project)
	}
	return errors.Join(errs...)
}

// dropStack removes a PR's stack once it's no longer tracked, after any run
// still finishing (which holds the worktree lock and stops it first).
func (r *Reviewer) dropStack(prID string) {
	r.mu.Lock()
	_, ok := r.st.Stacks[prID]
	r.mu.Unlock()
	if !ok {
		return
	}
	unlock, err := r.locks.Lock(context.Background(), prID)
	if err != nil {
		return
	}
	defer unlock()
	r.removeStacks(func(id string) bool { return id == prID })
}

// StackStatus is what's on Docker for a PR's stack right now.
type StackStatus struct {
	Project    string           `json:"project"`
	State      string           `json:"state"` // "running" | "stopped" | "none"
	Containers []StackContainer `json:"containers"`
	Volumes    []string         `json:"volumes"`
	Networks   []string         `json:"networks"`
}

type StackContainer struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	State   string `json:"state"`  // running | exited | …
	Status  string `json:"status"` // "Up 5 minutes (healthy)", "Exited (0) 3 days ago"
	Image   string `json:"image"`
}

// StackStatus asks Docker about the PR's stack. nil = its repo has no stack.
func (r *Reviewer) StackStatus(ctx context.Context, tr tracker.Tracked) (*StackStatus, error) {
	st := r.cfg.Repos[tr.Repo].Stack
	if st == nil {
		return nil, nil
	}
	project := strings.ReplaceAll(st.Project, "{id}", stackID(tr))
	r.mu.Lock()
	if sr := r.st.Stacks[tr.Key()]; sr != nil {
		project = sr.Project
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out bytes.Buffer
	err := r.sh(ctx, "", []string{"SISYPHUS_STACK=" + project}, `f="label=com.docker.compose.project=$SISYPHUS_STACK"
docker ps -a --filter "$f" --format 'c	{{.Names}}	{{.Label "com.docker.compose.service"}}	{{.State}}	{{.Status}}	{{.Image}}' &&
docker volume ls -q --filter "$f" | sed 's/^/v	/' &&
docker network ls --filter "$f" --format 'n	{{.Name}}'`, &out)
	if err != nil {
		return nil, fmt.Errorf("stack %s: %w: %s", project, err, lastLines(out.String(), 3))
	}
	s := &StackStatus{Project: project, State: "none"}
	for _, line := range strings.Split(out.String(), "\n") {
		f := strings.Split(line, "\t")
		switch {
		case f[0] == "c" && len(f) == 6:
			s.Containers = append(s.Containers, StackContainer{Name: f[1], Service: f[2], State: f[3], Status: f[4], Image: f[5]})
			if f[3] == "running" {
				s.State = "running"
			} else if s.State == "none" {
				s.State = "stopped"
			}
		case f[0] == "v" && len(f) == 2:
			s.Volumes = append(s.Volumes, f[1])
		case f[0] == "n" && len(f) == 2:
			s.Networks = append(s.Networks, f[1])
		}
	}
	if s.State == "none" && len(s.Volumes)+len(s.Networks) > 0 {
		s.State = "stopped"
	}
	return s, nil
}

// ResetReviews forgets a PR's reviews with the app running: it stops a running
// review, waits for it to let go of the worktree, then drops its queued jobs,
// review containers, history and run dirs. The stack stays. With no history
// left, the PR gets a fresh review after the next tick (Ensure).
func (r *Reviewer) ResetReviews(ctx context.Context, prID string) error {
	unlock, err := r.stopAndLock(ctx, prID)
	if err != nil {
		return err
	}
	defer unlock()
	slog.Info("resetting reviews", "pr", prID)
	return r.forget(ctx, prID, false)
}

// Wipe is ResetReviews plus the PR's stack (containers, network, volumes, found
// on Docker whether or not sisyphus recorded it) and the images its containers
// ran, unless other containers still use them. Tracking and the worktree stay;
// the reviewer image and per-repo caches are shared, so they stay too.
func (r *Reviewer) Wipe(ctx context.Context, tr tracker.Tracked) error {
	id := tr.Key()
	unlock, err := r.stopAndLock(ctx, id) // the stack's network can't go while the review container is on it
	if err != nil {
		return err
	}
	defer unlock()
	slog.Info("wiping review data", "pr", id)
	err = r.forget(ctx, id, true)
	stack, serr := r.StackStatus(ctx, tr)
	if serr != nil || stack == nil {
		return errors.Join(err, serr)
	}
	if len(stack.Containers)+len(stack.Volumes)+len(stack.Networks) > 0 {
		err = errors.Join(err, r.downStack(stack.Project))
	}
	images := map[string]bool{}
	for _, c := range stack.Containers {
		images[c.Image] = true
	}
	for img := range images {
		var out bytes.Buffer
		if r.sh(ctx, "", []string{"SISYPHUS_IMAGE=" + img}, `docker image rm "$SISYPHUS_IMAGE"`, &out) != nil {
			slog.Info("keeping image", "image", img, "why", lastLines(out.String(), 1)) // in use elsewhere
		} else {
			slog.Info("removed image", "image", img)
		}
	}
	return err
}

// stopAndLock stops a PR's running review and takes its worktree lock once the
// run has let go of it.
func (r *Reviewer) stopAndLock(ctx context.Context, prID string) (func(), error) {
	r.cancel(prID, errReset)
	return r.locks.Lock(ctx, prID)
}
