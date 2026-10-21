// Package review turns tracker events into review runs: a persistent queue (one
// job per PR, latest wins), a bounded set of running containers, run records on
// disk, and re-attaching to containers that outlived a restart.
package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/jsonfile"
	"github.com/reneruck/waterboy/internal/tracker"
	"github.com/reneruck/waterboy/internal/worktree"
)

// Job is one review to run, with the PR as the tracker announced it.
type Job struct {
	PR     tracker.Tracked `json:"pr"`
	Reason string          `json:"reason"` // "new" | "commits" | "replies"
}

// Run statuses.
const (
	Running   = "running"
	Succeeded = "succeeded"
	Failed    = "failed"    // container exited non-zero
	Cancelled = "cancelled" // superseded, worktree reclaimed, or PR dropped
	TimedOut  = "timeout"
	Errored   = "error" // couldn't start, or the worktree wasn't at the announced head
	Lost      = "lost"  // its container was gone after a restart; requeued
)

// Run is the record of one review run. Its directory holds run.json, log and out/.
type Run struct {
	ID        string    `json:"id"`
	Job       Job       `json:"job"`
	Image     string    `json:"image"`
	Container string    `json:"container,omitempty"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished"`
	Status    string    `json:"status"`
	ExitCode  int       `json:"exit_code"`
	Error     string    `json:"error,omitempty"`
	Dir       string    `json:"dir"`
}

// PRStatus is what UIs show for one PR.
type PRStatus struct {
	State string `json:"state"` // "" | "queued" | "running"
	Runs  []Run  `json:"runs"`  // newest first
}

// Latest is the most recent run, if any.
func (p PRStatus) Latest() *Run {
	if len(p.Runs) == 0 {
		return nil
	}
	return &p.Runs[0]
}

const keepRuns = 10 // records per PR in reviews.json; run dirs stay on disk

var (
	errSuperseded = errors.New("superseded by new commits")
	errReclaimed  = errors.New("worktree reclaimed by the tracker")
	errDropped    = errors.New("PR dropped")
	errTimeout    = errors.New("timed out")
)

type store struct {
	Queue []Job            `json:"queue"`
	Runs  map[string][]Run `json:"runs"` // by PR ID, oldest first
}

type Reviewer struct {
	cfg   config.Review
	rt    container.Runtime
	locks *tracker.Locks
	dir   string // reviews.json and runs/ live here

	Kick     func() // asks for a tick soon; called once a reclaimed worktree is free
	OnChange func() // called after every change, on any goroutine

	mu          sync.Mutex
	st          store
	active      map[string]context.CancelCauseFunc // running, by PR ID
	wake        chan struct{}
	unavailable string // why the runtime can't start containers right now
}

func New(cfg config.Review, rt container.Runtime, locks *tracker.Locks, dir string) (*Reviewer, error) {
	r := &Reviewer{cfg: cfg, rt: rt, locks: locks, dir: dir, st: store{Runs: map[string][]Run{}},
		active: map[string]context.CancelCauseFunc{}, wake: make(chan struct{}, 1)}
	if err := jsonfile.Load(r.storePath(), &r.st); err != nil {
		return nil, err
	}
	if r.st.Runs == nil {
		r.st.Runs = map[string][]Run{}
	}
	return r, nil
}

func (r *Reviewer) storePath() string { return filepath.Join(r.dir, "reviews.json") }

// --- tracker.Events (called on the ticking goroutine; never block) ---

func (r *Reviewer) Tracked(tr tracker.Tracked) { r.enqueue(Job{PR: tr, Reason: "new"}) }

func (r *Reviewer) Updated(tr tracker.Tracked, newCommits bool, _ int) {
	reason := "replies" // replies don't interrupt a running review; they queue behind it
	if newCommits {
		reason = "commits"
		r.cancel(tr.Key(), errSuperseded)
	}
	r.enqueue(Job{PR: tr, Reason: reason})
}

func (r *Reviewer) Reclaim(tr tracker.Tracked) { r.cancel(tr.Key(), errReclaimed) }

func (r *Reviewer) Dropped(tr tracker.Tracked, _ string) {
	id := tr.Key()
	r.mu.Lock()
	r.st.Queue = slices.DeleteFunc(r.st.Queue, func(j Job) bool { return j.PR.Key() == id })
	r.saveLocked()
	r.mu.Unlock()
	r.cancel(id, errDropped)
	r.changed()
}

var rank = map[string]int{"replies": 1, "commits": 2, "new": 3}

// enqueue adds j, or replaces the PR's queued job with it (keeping the
// stronger reason, so a never-reviewed PR stays "new").
func (r *Reviewer) enqueue(j Job) {
	r.mu.Lock()
	i := slices.IndexFunc(r.st.Queue, func(q Job) bool { return q.PR.Key() == j.PR.Key() })
	if i < 0 {
		r.st.Queue = append(r.st.Queue, j)
	} else {
		if rank[r.st.Queue[i].Reason] > rank[j.Reason] {
			j.Reason = r.st.Queue[i].Reason
		}
		r.st.Queue[i] = j
	}
	r.saveLocked()
	r.mu.Unlock()
	slog.Info("review queued", "pr", j.PR.Key(), "reason", j.Reason, "head", j.PR.HeadSHA)
	r.changed()
}

func (r *Reviewer) cancel(id string, why error) {
	r.mu.Lock()
	cancel := r.active[id]
	r.mu.Unlock()
	if cancel != nil {
		slog.Info("stopping review", "pr", id, "why", why)
		cancel(why)
	}
}

// --- running ---

// Reattach picks up runs a previous process left running. Call it before the
// first tick: it takes the worktree locks those containers still use.
func (r *Reviewer) Reattach(ctx context.Context) {
	r.mu.Lock()
	var left []Run
	for _, runs := range r.st.Runs {
		for _, run := range runs {
			if run.Status == Running {
				left = append(left, run)
			}
		}
	}
	r.mu.Unlock()
	known := map[string]bool{}
	for _, run := range left {
		infos, err := r.rt.List(ctx, map[string]string{"waterboy.run": run.ID})
		unlock, locked := r.locks.TryLock(run.Job.PR.Key())
		if err != nil || len(infos) != 1 || !locked {
			for _, c := range infos {
				r.rt.Remove(ctx, c.ID)
			}
			if locked {
				unlock()
			}
			r.lose(run, fmt.Errorf("couldn't re-attach after restart (containers %d, err %v)", len(infos), err))
			continue
		}
		known[infos[0].ID] = true
		run.Container = infos[0].ID
		runCtx, cancel := context.WithCancelCause(ctx)
		r.mu.Lock()
		r.active[run.Job.PR.Key()] = cancel
		r.mu.Unlock()
		slog.Info("re-attached review", "pr", run.Job.PR.Key(), "run", run.ID, "container", run.Container)
		go r.follow(ctx, runCtx, run, unlock, true)
	}
	// Containers with no running record (we died between start and saving it).
	if orphans, err := r.rt.List(ctx, map[string]string{"waterboy": "1"}); err == nil {
		for _, c := range orphans {
			if !known[c.ID] {
				slog.Warn("removing orphaned review container", "container", c.ID, "run", c.Labels["waterboy.run"])
				r.rt.Remove(ctx, c.ID)
			}
		}
	}
}

// lose marks a run that can't be followed any more and queues its job again.
func (r *Reviewer) lose(run Run, err error) {
	slog.Warn("review lost, requeueing", "pr", run.Job.PR.Key(), "run", run.ID, "err", err)
	r.record(run.Job.PR.Key(), run.ID, func(x *Run) { x.Status, x.Error, x.Finished = Lost, err.Error(), time.Now() })
	r.enqueue(run.Job)
}

// Run starts queued jobs, at most MaxConcurrent at a time, until ctx ends.
// Containers keep running when ctx ends, so a restart can re-attach to them.
func (r *Reviewer) Run(ctx context.Context) {
	for {
		r.dispatch(ctx)
		select {
		case <-r.wake:
		case <-ctx.Done():
			return
		}
	}
}

// RuntimeError says why reviews are waiting on the container runtime ("" = they aren't).
func (r *Reviewer) RuntimeError() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.unavailable
}

// checkRuntime keeps jobs queued while the runtime is down and retries in a minute.
func (r *Reviewer) checkRuntime(ctx context.Context) bool {
	err := r.rt.Available(ctx)
	msg := ""
	if err != nil {
		msg = err.Error()
		time.AfterFunc(time.Minute, r.poke)
	}
	r.mu.Lock()
	was := r.unavailable
	r.unavailable = msg
	r.mu.Unlock()
	if was != msg {
		if err != nil {
			slog.Error("container runtime unavailable, reviews wait", "err", err)
		} else {
			slog.Info("container runtime available")
		}
		r.changed()
	}
	return err == nil
}

func (r *Reviewer) dispatch(ctx context.Context) {
	r.mu.Lock()
	pending := len(r.st.Queue) > 0 && len(r.active) < r.cfg.MaxConcurrent
	r.mu.Unlock()
	if !pending || !r.checkRuntime(ctx) {
		return
	}
	r.mu.Lock()
	type start struct {
		run    Run
		runCtx context.Context
	}
	var starts []start
	for i := 0; i < len(r.st.Queue) && len(r.active) < r.cfg.MaxConcurrent; {
		j := r.st.Queue[i]
		id := j.PR.Key()
		if r.active[id] != nil {
			i++ // one run per PR; this job waits for the current one
			continue
		}
		r.st.Queue = slices.Delete(r.st.Queue, i, i+1)
		now := time.Now()
		runID := now.UTC().Format("20060102-150405.000")
		run := Run{ID: runID, Job: j, Image: r.cfg.Image, Started: now, Status: Running,
			Dir: filepath.Join(r.dir, "runs", prPath(id), runID)}
		r.appendRunLocked(run) // saved with the dequeue, so a crash here is seen as "lost"
		runCtx, cancel := context.WithCancelCause(ctx)
		r.active[id] = cancel
		starts = append(starts, start{run, runCtx})
	}
	if len(starts) > 0 {
		r.saveLocked()
	}
	r.mu.Unlock()
	for _, s := range starts {
		r.changed()
		go r.execute(ctx, s.runCtx, s.run)
	}
}

func (r *Reviewer) execute(appCtx, runCtx context.Context, run Run) {
	tr := run.Job.PR
	id := tr.Key()
	unlock, err := r.locks.Lock(runCtx, id)
	if err != nil {
		if appCtx.Err() != nil {
			return // shutting down; the record stays "running" and is requeued as lost on restart
		}
		r.finish(run, Cancelled, 0, context.Cause(runCtx), nil)
		return
	}
	fail := func(err error) {
		slog.Warn("review not started", "pr", id, "run", run.ID, "err", err)
		r.finish(run, Errored, 0, err, unlock)
	}
	if head, err := worktree.HeadSHA(runCtx, tr.Worktree); err != nil {
		fail(err)
		return
	} else if head != tr.HeadSHA {
		fail(fmt.Errorf("worktree is at %.12s, not the announced head %.12s", head, tr.HeadSHA))
		return
	}
	spec, err := r.spec(runCtx, run)
	if err != nil {
		fail(err)
		return
	}
	cid, err := r.rt.Start(runCtx, spec)
	if err != nil {
		fail(err)
		return
	}
	run.Container = cid
	r.record(id, run.ID, func(x *Run) { x.Container = cid })
	slog.Info("review started", "pr", id, "run", run.ID, "reason", run.Job.Reason, "container", cid)
	r.follow(appCtx, runCtx, run, unlock, false)
}

func (r *Reviewer) spec(ctx context.Context, run Run) (container.Spec, error) {
	tr := run.Job.PR
	out := filepath.Join(run.Dir, "out")
	if err := os.MkdirAll(out, 0o755); err != nil {
		return container.Spec{}, err
	}
	// The worktree's .git is a pointer into the clone's .git; mount both at the
	// same paths so the pointer resolves inside the container.
	mounts := []container.Mount{{Source: tr.Worktree, Target: tr.Worktree}}
	gitDir, err := worktree.CommonDir(ctx, tr.Worktree)
	if err != nil {
		return container.Spec{}, err
	}
	if real, _ := filepath.EvalSymlinks(tr.Worktree); !strings.HasPrefix(gitDir+"/", real+"/") {
		mounts = append(mounts, container.Mount{Source: gitDir, Target: gitDir})
	}
	mounts = append(mounts, container.Mount{Source: out, Target: "/out"})
	env := map[string]string{
		"WATERBOY_RUN_ID": run.ID, "WATERBOY_REASON": run.Job.Reason,
		"WATERBOY_PR_URL": tr.URL, "WATERBOY_REPO": tr.Repo, "WATERBOY_PR_NUMBER": strconv.Itoa(tr.Number),
		"WATERBOY_BRANCH": tr.HeadRef, "WATERBOY_HEAD_SHA": tr.HeadSHA, "WATERBOY_WORKTREE": tr.Worktree,
		"WATERBOY_OUT": "/out",
		// The container's user doesn't own the mounted files; let git work anyway.
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
	}
	for _, k := range r.cfg.Env {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	return container.Spec{
		Name:  "waterboy-" + strings.NewReplacer("/", "-", "#", "-").Replace(tr.Key()) + "-" + run.ID,
		Image: r.cfg.Image, Command: r.cfg.Command, Env: env, EnvFile: r.cfg.EnvFile,
		Mounts: mounts, Workdir: tr.Worktree,
		Labels: map[string]string{"waterboy": "1", "waterboy.run": run.ID, "waterboy.pr": tr.Key()},
	}, nil
}

// follow streams the container's log into the run dir, waits for it to exit
// (stopping it on cancel or timeout), then records the result. On shutdown it
// returns without touching the container, which a restart re-attaches to.
func (r *Reviewer) follow(appCtx, runCtx context.Context, run Run, unlock func(), reattached bool) {
	id := run.Job.PR.Key()
	runCtx, cancelTimeout := context.WithDeadlineCause(runCtx, run.Started.Add(r.cfg.Timeout.Duration), errTimeout)
	defer cancelTimeout()

	logsDone := make(chan struct{})
	logf, err := os.Create(filepath.Join(run.Dir, "log")) // docker replays from the start, so truncate
	if err != nil {
		slog.Warn("creating run log", "run", run.ID, "err", err)
		close(logsDone)
	} else {
		go func() {
			defer close(logsDone)
			defer logf.Close()
			r.rt.Logs(appCtx, run.Container, logf)
		}()
	}

	code, err := r.rt.Wait(runCtx, run.Container)
	status, why := Succeeded, error(nil)
	switch {
	case err == nil && code != 0:
		status = Failed
	case err != nil && appCtx.Err() != nil:
		return // shutting down: leave the container running and the record "running"
	case err != nil && runCtx.Err() != nil: // cancelled or timed out
		why = context.Cause(runCtx)
		status = Cancelled
		if errors.Is(why, errTimeout) {
			status = TimedOut
		}
		bg, cancel := context.WithTimeout(context.WithoutCancel(appCtx), time.Minute)
		r.rt.Stop(bg, run.Container)
		code, _ = r.rt.Wait(bg, run.Container)
		cancel()
	case err != nil:
		<-logsDone
		r.rt.Remove(context.WithoutCancel(appCtx), run.Container)
		if reattached {
			unlock()
			r.mu.Lock()
			delete(r.active, id)
			r.mu.Unlock()
			r.lose(run, err)
			r.poke()
			return
		}
		r.finish(run, Errored, 0, err, unlock)
		return
	}
	<-logsDone
	bg := context.WithoutCancel(appCtx)
	if err := r.rt.Remove(bg, run.Container); err != nil {
		slog.Warn("removing review container", "run", run.ID, "err", err)
	}
	var cleanupErr error
	if r.cfg.Cleanup && run.Job.PR.CreatedWorktree { // reused worktrees may hold the user's own work
		cleanupErr = worktree.Reset(bg, run.Job.PR.Worktree, run.Job.PR.HeadSHA)
	}
	r.finish(run, status, code, errors.Join(why, cleanupErr), unlock)
	if errors.Is(why, errReclaimed) && r.Kick != nil {
		r.Kick() // the tracker is waiting for this worktree
	}
}

// finish records the outcome, releases the worktree and the run slot.
func (r *Reviewer) finish(run Run, status string, code int, err error, unlock func()) {
	id := run.Job.PR.Key()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	r.record(id, run.ID, func(x *Run) { x.Status, x.ExitCode, x.Error, x.Finished = status, code, msg, time.Now() })
	if unlock != nil {
		unlock()
	}
	r.mu.Lock()
	delete(r.active, id)
	r.mu.Unlock()
	slog.Info("review finished", "pr", id, "run", run.ID, "status", status, "exit_code", code, "err", msg)
	r.changed()
	r.poke()
}

func (r *Reviewer) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// --- records ---

func (r *Reviewer) appendRunLocked(run Run) {
	runs := append(r.st.Runs[run.Job.PR.Key()], run)
	if len(runs) > keepRuns {
		runs = runs[len(runs)-keepRuns:]
	}
	r.st.Runs[run.Job.PR.Key()] = runs
	r.writeRunJSON(run)
}

// record updates one run record, saves reviews.json and its run.json.
func (r *Reviewer) record(prID, runID string, fn func(*Run)) {
	r.mu.Lock()
	runs := r.st.Runs[prID]
	i := slices.IndexFunc(runs, func(x Run) bool { return x.ID == runID })
	if i >= 0 {
		fn(&runs[i])
		r.writeRunJSON(runs[i])
		r.saveLocked()
	}
	r.mu.Unlock()
	r.changed()
}

func (r *Reviewer) writeRunJSON(run Run) {
	if err := jsonfile.Save(filepath.Join(run.Dir, "run.json"), run); err != nil {
		slog.Warn("writing run.json", "run", run.ID, "err", err)
	}
}

func (r *Reviewer) saveLocked() {
	if err := jsonfile.Save(r.storePath(), r.st); err != nil {
		slog.Error("saving reviews", "err", err)
	}
}

func (r *Reviewer) changed() {
	r.poke()
	if r.OnChange != nil {
		r.OnChange()
	}
}

// Status returns every PR's review state and recent runs, by PR ID.
func (r *Reviewer) Status() map[string]PRStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]PRStatus{}
	for id, runs := range r.st.Runs {
		rs := slices.Clone(runs)
		slices.Reverse(rs)
		out[id] = PRStatus{Runs: rs}
	}
	set := func(id, state string) {
		s := out[id]
		s.State = state
		out[id] = s
	}
	for _, j := range r.st.Queue {
		set(j.PR.Key(), "queued")
	}
	for id := range r.active {
		set(id, "running")
	}
	return out
}

// RunDir returns the directory of one run (run.json, log, out/).
func (r *Reviewer) RunDir(prID, runID string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.st.Runs[prID] {
		if run.ID == runID {
			return run.Dir, true
		}
	}
	return "", false
}

// prPath turns "owner/repo#n" into "owner/repo/n".
func prPath(id string) string { return strings.Replace(id, "#", "/", 1) }
