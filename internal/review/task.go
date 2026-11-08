package review

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// taskPrefix keys a task's run records in reviews.json, apart from the PRs'.
const taskPrefix = "task:"

// taskPoll is how often a task waiting for a slot, the budget or the runtime looks again.
var taskPoll = 5 * time.Second

// Task is a one-shot agent run on a worktree, for callers other than reviews
// (the last mile's fix agents). The agent edits files and writes /out/result.json;
// nothing is posted, and the container gets no GitHub token.
type Task struct {
	ID       string            // names the run dir (tasks/<ID>/<run>) and the records; one live task per ID
	Repo     string            // "owner/name", a key of review.repos
	Worktree string            // mounted read/write, .git read-only; the caller holds its lock
	Agent    config.Agent      // zero = review.agent
	Prompt   string            // written to /sisyphus/prompt.md
	Context  map[string]string // file name -> content, under /sisyphus/context (read-only)
	// AfterSetup, when set, is called once the repo's setup is done and before the
	// agent starts; the container waits for it. An error fails the run.
	AfterSetup func() error
}

// TaskResult is what a finished task leaves.
type TaskResult struct {
	Result []byte // /out/result.json
	Dir    string // the run dir: prompt.md, log, out/
	RunID  string
}

// RunTask waits for a run slot (sharing review.max_concurrent with reviews),
// the 5-hour budget and the container runtime, runs the task, and returns its
// /out/result.json. A missing result file or a failed container is an error.
// Usage is recorded like a review's, under "task:<ID>", so the budget counts it.
// ponytail: no local stack for tasks, and no live event stream; add both when an
// agent kind needs them. Not re-attached after a restart: the run is marked lost.
func (r *Reviewer) RunTask(ctx context.Context, t Task) (TaskResult, error) {
	repo, ok := r.cfg.Repos[t.Repo]
	if !ok {
		return TaskResult{}, fmt.Errorf("no review.repos entry for %s", t.Repo)
	}
	if why := r.Blocked(t.Repo); why != "" {
		return TaskResult{}, errors.New(why)
	}
	if t.Agent.Vendor == "" {
		t.Agent = r.cfg.Agent
	}
	key := taskPrefix + t.ID
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	if err := r.acquireSlot(ctx, key, cancel); err != nil {
		return TaskResult{}, err
	}
	defer func() {
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()
		r.poke() // a review may be waiting for the slot
	}()

	now := time.Now()
	runID := now.UTC().Format("20060102-150405.000")
	run := Run{ID: runID, Image: repo.Image, Started: now, Status: Running,
		Job: Job{PR: tracker.Tracked{PRRef: github.PRRef{Repo: t.Repo}, Worktree: t.Worktree}, Agent: t.Agent, Reason: "task"},
		Dir: filepath.Join(r.dir, "tasks", t.ID, runID)}
	r.mu.Lock()
	runs := append(r.st.Runs[key], run)
	r.st.Runs[key] = runs[max(0, len(runs)-keepRuns):]
	r.writeRunJSON(run)
	r.saveLocked()
	r.mu.Unlock()
	r.changed()

	res, code, err := r.runTask(ctx, key, run, t)
	status, msg := Succeeded, ""
	if err != nil {
		status, msg = Failed, err.Error()
		if ctx.Err() != nil {
			status = Cancelled
		}
	}
	r.record(key, runID, func(x *Run) { x.Status, x.ExitCode, x.Error, x.Finished = status, code, msg, time.Now() })
	slog.Info("task finished", "task", t.ID, "run", runID, "status", status, "err", msg)
	return TaskResult{Result: res, Dir: run.Dir, RunID: runID}, err
}

// acquireSlot waits until the budget and runtime allow a run and a slot is free,
// then takes it for key. The slot check and the claim are one step under mu, as in dispatch.
func (r *Reviewer) acquireSlot(ctx context.Context, key string, cancel context.CancelCauseFunc) error {
	for {
		if r.checkBudget() && r.checkRuntime(ctx) {
			r.mu.Lock()
			if r.active[key] != nil {
				r.mu.Unlock()
				return fmt.Errorf("task %s is already running", key)
			}
			if len(r.active) < r.cfg.MaxConcurrent {
				r.active[key] = cancel
				r.mu.Unlock()
				return nil
			}
			r.mu.Unlock()
		}
		select {
		case <-time.After(taskPoll):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

func (r *Reviewer) runTask(ctx context.Context, key string, run Run, t Task) (result []byte, code int, err error) {
	for _, d := range []string{"out", "claude", "context"} {
		if err := os.MkdirAll(filepath.Join(run.Dir, d), 0o755); err != nil {
			return nil, 0, err
		}
	}
	if err := os.WriteFile(filepath.Join(run.Dir, "prompt.md"), []byte(t.Prompt), 0o644); err != nil {
		return nil, 0, err
	}
	for name, content := range t.Context {
		if err := os.WriteFile(filepath.Join(run.Dir, "context", filepath.Base(name)), []byte(content), 0o644); err != nil {
			return nil, 0, err
		}
	}
	spec, err := r.buildSpec(ctx, specReq{
		Repo: t.Repo, Worktree: t.Worktree, Dir: run.Dir, RunID: run.ID, Agent: t.Agent,
		Name:        "sisyphus-task-" + strings.NewReplacer("/", "-", "#", "-").Replace(t.ID) + "-" + run.ID,
		Env:         map[string]string{"SISYPHUS_REASON": "task", "SISYPHUS_TASK": t.ID},
		Override:    map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": ""},
		Labels:      map[string]string{"sisyphus": "1", "sisyphus.run": run.ID, "sisyphus.task": t.ID},
		WithContext: true,
		Gate:        t.AfterSetup != nil,
	})
	if err != nil {
		return nil, 0, err
	}
	ctx, fail := context.WithCancelCause(ctx)
	defer fail(nil)
	if r.cfg.Timeout.Duration > 0 { // unlike a review, a task is cut off: its caller holds a worktree lock
		var stop context.CancelFunc
		ctx, stop = context.WithTimeoutCause(ctx, r.cfg.Timeout.Duration, errors.New("task timed out (review.timeout)"))
		defer stop()
	}
	bg := context.WithoutCancel(ctx)
	cid, err := r.rt.Start(ctx, spec)
	if err != nil {
		r.removeRun(bg, run.ID) // `docker run` failing doesn't mean nothing started
		return nil, 0, err
	}
	defer r.removeContainer(bg, run.ID, cid)
	r.record(key, run.ID, func(x *Run) { x.Container, x.Name = cid, spec.Name })

	logf, err := os.Create(filepath.Join(run.Dir, "log"))
	if err != nil {
		return nil, 0, err
	}
	defer logf.Close()
	var claude *ClaudeResult
	gated := false
	lw := &lineWriter{file: logf, onLine: func(n int, line string) {
		if t.AfterSetup != nil && !gated && line == setupDone {
			gated = true
			err := t.AfterSetup()
			if err == nil {
				err = os.WriteFile(filepath.Join(run.Dir, "out", gateFile), nil, 0o644)
			}
			if err != nil {
				fail(fmt.Errorf("after setup: %w", err))
			}
			return
		}
		_, res, w := parseLine(n, line)
		if res != nil {
			claude = res
			r.record(key, run.ID, func(x *Run) { x.Claude = res })
		}
		if w != nil && w.plausible(time.Now()) {
			r.record(key, run.ID, func(x *Run) { x.Usage = observe(x.Usage, *w) })
		}
		if res != nil || w != nil && w.plausible(time.Now()) {
			r.logUsage(key, run.ID, line)
		}
	}}
	logsCtx, cancelLogs := context.WithCancel(bg)
	defer cancelLogs()
	logsDone := make(chan struct{})
	go func() {
		defer close(logsDone)
		if err := r.rt.Logs(logsCtx, cid, lw); err != nil && logsCtx.Err() == nil {
			slog.Warn("following task output", "run", run.ID, "err", err)
		}
		lw.Flush()
	}()
	drain := func() {
		select {
		case <-logsDone:
		case <-time.After(logGrace):
			cancelLogs()
			<-logsDone
		}
	}

	code, err = r.rt.Wait(ctx, cid)
	if err != nil {
		stopCtx, stop := context.WithTimeout(bg, time.Minute)
		defer stop()
		if serr := r.rt.Stop(stopCtx, cid); serr != nil {
			slog.Warn("stopping task container", "run", run.ID, "err", serr)
		}
		drain()
		return nil, 0, cmp.Or(context.Cause(ctx), err)
	}
	drain()
	switch {
	case claude != nil && claude.IsError:
		return nil, code, fmt.Errorf("%s: %s", t.Agent.Vendor, cmp.Or(claude.Error, claude.Subtype))
	case code != 0:
		return nil, code, fmt.Errorf("agent container exited %d", code)
	}
	result, err = os.ReadFile(filepath.Join(run.Dir, "out", "result.json"))
	if err != nil {
		return nil, code, fmt.Errorf("agent wrote no /out/result.json: %w", err)
	}
	return result, code, nil
}
