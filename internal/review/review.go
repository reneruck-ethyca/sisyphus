// Package review turns tracker events into review runs: a persistent queue (one
// job per PR, latest wins), a bounded set of running containers, run records on
// disk, and re-attaching to containers that outlived a restart.
package review

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	Failed    = "failed"    // setup or Claude failed (non-zero exit, or Claude reported an error)
	Cancelled = "cancelled" // superseded, worktree reclaimed, or PR dropped
	Errored   = "error"     // couldn't start, or the worktree wasn't at the announced head
	Lost      = "lost"      // its container was gone after a restart; requeued
)

// Run is the record of one review run. Its directory holds run.json, prompt.md,
// log (raw container output: setup, then Claude's stream-json), claude/ (Claude's
// config dir, with the session transcript) and out/.
type Run struct {
	ID        string        `json:"id"`
	Job       Job           `json:"job"`
	Image     string        `json:"image"`
	Container string        `json:"container,omitempty"`
	Started   time.Time     `json:"started"`
	Finished  time.Time     `json:"finished"`
	Status    string        `json:"status"`
	ExitCode  int           `json:"exit_code"`
	Error     string        `json:"error,omitempty"`
	Dir       string        `json:"dir"`
	Claude    *ClaudeResult `json:"claude,omitempty"`  // Claude's final result line, once it has one
	Overdue   bool          `json:"overdue,omitempty"` // still running past review.timeout (set by Status)
}

// RunEvent is an Event of one run, as streamed to subscribers.
type RunEvent struct {
	PR  string `json:"pr"`
	Run string `json:"run"`
	Event
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

// Tunables (vars so tests can shorten them).
var (
	retryEvery = time.Minute     // re-check an unavailable container runtime
	logGrace   = 5 * time.Second // let the log stream drain after the container is done
)

var (
	errSuperseded = errors.New("superseded by new commits")
	errReclaimed  = errors.New("worktree reclaimed by the tracker")
	errDropped    = errors.New("PR dropped")
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
	retrying    bool   // a runtime re-check is scheduled
	subs        map[chan RunEvent]struct{}
	skipped     map[string]bool // repos without review config, logged once
}

func New(cfg config.Review, rt container.Runtime, locks *tracker.Locks, dir string) (*Reviewer, error) {
	r := &Reviewer{cfg: cfg, rt: rt, locks: locks, dir: dir, st: store{Runs: map[string][]Run{}},
		active: map[string]context.CancelCauseFunc{}, wake: make(chan struct{}, 1),
		subs: map[chan RunEvent]struct{}{}, skipped: map[string]bool{}}
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

func (r *Reviewer) Tracked(tr tracker.Tracked) {
	if r.reviewable(tr) {
		r.enqueue(Job{PR: tr, Reason: "new"})
	}
}

func (r *Reviewer) Updated(tr tracker.Tracked, newCommits bool, _ int) {
	if !r.reviewable(tr) {
		return
	}
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

// reviewable: only repos with a review.repos entry are reviewed. (Fork PRs never
// get here: the tracker doesn't track them.)
func (r *Reviewer) reviewable(tr tracker.Tracked) bool {
	if _, ok := r.cfg.Repos[tr.Repo]; ok {
		return true
	}
	r.mu.Lock()
	first := !r.skipped[tr.Repo]
	r.skipped[tr.Repo] = true
	r.mu.Unlock()
	if first {
		slog.Info("not reviewing: no review.repos entry", "repo", tr.Repo)
	}
	return false
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
// first tick: it takes the worktree locks those containers still use, even while
// the runtime isn't up yet (as at login), and re-attaches once it is.
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
	for _, run := range left {
		id := run.Job.PR.Key()
		unlock, ok := r.locks.TryLock(id)
		if !ok {
			r.lose(run, errors.New("worktree busy at start"))
			continue
		}
		runCtx, cancel := context.WithCancelCause(ctx)
		r.mu.Lock()
		r.active[id] = cancel
		r.mu.Unlock()
		go r.reattach(ctx, runCtx, run, unlock)
	}
	go r.sweepOrphans(ctx)
}

func (r *Reviewer) reattach(appCtx, runCtx context.Context, run Run, unlock func()) {
	id := run.Job.PR.Key()
	giveUp := func(err error) {
		if appCtx.Err() != nil {
			return // shutting down again; try next start
		}
		unlock()
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		r.lose(run, err)
		if errors.Is(err, errReclaimed) && r.Kick != nil {
			r.Kick()
		}
		r.poke()
	}
	if err := r.waitRuntime(runCtx); err != nil {
		giveUp(context.Cause(runCtx))
		return
	}
	infos, err := r.rt.List(runCtx, map[string]string{"waterboy.run": run.ID})
	if err != nil || len(infos) != 1 {
		for _, c := range infos {
			r.rt.Remove(context.WithoutCancel(appCtx), c.ID)
		}
		giveUp(fmt.Errorf("couldn't re-attach after restart (containers %d, err %v)", len(infos), err))
		return
	}
	run.Container = infos[0].ID
	slog.Info("re-attached review", "pr", id, "run", run.ID, "container", run.Container, "running", infos[0].Running)
	r.follow(appCtx, runCtx, run, unlock, true)
}

// sweepOrphans removes review containers with no running record (we died
// between starting one and saving its record, or gave up re-attaching).
func (r *Reviewer) sweepOrphans(ctx context.Context) {
	if r.waitRuntime(ctx) != nil {
		return
	}
	cs, err := r.rt.List(ctx, map[string]string{"waterboy": "1"})
	if err != nil {
		return
	}
	r.mu.Lock()
	running := map[string]bool{}
	for _, runs := range r.st.Runs {
		for _, run := range runs {
			running[run.ID] = run.Status == Running
		}
	}
	r.mu.Unlock()
	for _, c := range cs {
		if !running[c.Labels["waterboy.run"]] {
			slog.Warn("removing orphaned review container", "container", c.ID, "run", c.Labels["waterboy.run"])
			r.rt.Remove(ctx, c.ID)
		}
	}
}

// waitRuntime blocks until the container runtime answers or ctx ends.
func (r *Reviewer) waitRuntime(ctx context.Context) error {
	for r.rt.Available(ctx) != nil {
		select {
		case <-time.After(retryEvery):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
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
	r.mu.Lock()
	if err != nil {
		msg = err.Error()
		if !r.retrying { // one pending retry, however often we're poked meanwhile
			r.retrying = true
			time.AfterFunc(retryEvery, func() {
				r.mu.Lock()
				r.retrying = false
				r.mu.Unlock()
				r.poke()
			})
		}
	}
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
		run := Run{ID: runID, Job: j, Image: r.cfg.Repos[j.PR.Repo].Image, Started: now, Status: Running,
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
		r.endEarly(appCtx, runCtx, run, err, nil)
		return
	}
	fail := func(err error) { r.endEarly(appCtx, runCtx, run, err, unlock) }
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
		if appCtx.Err() == nil {
			// `docker run` failing doesn't mean the daemon didn't start it.
			r.removeRun(context.WithoutCancel(appCtx), run.ID)
		}
		fail(err)
		return
	}
	run.Container = cid
	r.record(id, run.ID, func(x *Run) { x.Container = cid })
	slog.Info("review started", "pr", id, "run", run.ID, "reason", run.Job.Reason, "container", cid)
	r.follow(appCtx, runCtx, run, unlock, false)
}

// endEarly settles a run that ended before its container was up.
func (r *Reviewer) endEarly(appCtx, runCtx context.Context, run Run, err error, unlock func()) {
	if appCtx.Err() != nil {
		return // shutting down: the record stays "running" and is requeued as lost on restart
	}
	if runCtx.Err() != nil { // superseded, reclaimed or dropped while starting
		why := context.Cause(runCtx)
		r.finish(run, Cancelled, 0, why, unlock)
		if errors.Is(why, errReclaimed) && r.Kick != nil {
			r.Kick()
		}
		return
	}
	slog.Warn("review not started", "pr", run.Job.PR.Key(), "run", run.ID, "err", err)
	r.finish(run, Errored, 0, err, unlock)
}

func (r *Reviewer) removeRun(ctx context.Context, runID string) {
	cs, _ := r.rt.List(ctx, map[string]string{"waterboy.run": runID})
	for _, c := range cs {
		r.rt.Remove(ctx, c.ID)
	}
}

func (r *Reviewer) spec(ctx context.Context, run Run) (container.Spec, error) {
	tr := run.Job.PR
	repo, ok := r.cfg.Repos[tr.Repo]
	if !ok {
		return container.Spec{}, fmt.Errorf("no review.repos entry for %s", tr.Repo)
	}
	workdir := cmp.Or(repo.Workdir, tr.Worktree)
	for _, d := range []string{"out", "claude"} {
		if err := os.MkdirAll(filepath.Join(run.Dir, d), 0o755); err != nil {
			return container.Spec{}, err
		}
	}
	prompt, err := renderPrompt(cmp.Or(repo.PromptFile, r.cfg.PromptFile),
		PromptData{PR: tr, Reason: run.Job.Reason, PreviousHead: r.previousHead(tr.Key()), Workdir: workdir})
	if err != nil {
		return container.Spec{}, fmt.Errorf("prompt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return container.Spec{}, err
	}
	if r.cfg.ClaudeSettings != "" {
		b, err := os.ReadFile(r.cfg.ClaudeSettings)
		if err != nil {
			return container.Spec{}, fmt.Errorf("claude_settings: %w", err)
		}
		if err := os.WriteFile(filepath.Join(run.Dir, "claude", "settings.json"), b, 0o644); err != nil {
			return container.Spec{}, err
		}
	}

	// Sources are read/write; git metadata is read-only. Host-side git (sync,
	// cleanup) runs in this worktree, so a container that could write hooks or
	// config (core.fsmonitor, …), or re-point the worktree's .git file, could run
	// commands on the host. The worktree's .git points into the clone's .git,
	// which is mounted at its own host path so the pointer resolves.
	dotGit := filepath.Join(tr.Worktree, ".git")
	mounts := []container.Mount{
		{Source: tr.Worktree, Target: workdir},
		{Source: dotGit, Target: filepath.Join(workdir, ".git"), ReadOnly: true},
	}
	gitDir, err := worktree.CommonDir(ctx, tr.Worktree)
	if err != nil {
		return container.Spec{}, err
	}
	if real, _ := filepath.EvalSymlinks(dotGit); gitDir != real {
		mounts = append(mounts, container.Mount{Source: gitDir, Target: gitDir, ReadOnly: true})
	}
	// Dependency caches: per-repo volumes over the worktree, so the host's macOS
	// builds stay out and installs survive between runs. Volumes start root-owned.
	var owned []string
	for _, c := range repo.Cache {
		target := filepath.Join(workdir, c)
		mounts = append(mounts, container.Mount{Source: volumeName(tr.Repo, c), Target: target})
		owned = append(owned, target)
	}
	mounts = append(mounts,
		container.Mount{Source: filepath.Join(run.Dir, "out"), Target: "/out"},
		container.Mount{Source: filepath.Join(run.Dir, "prompt.md"), Target: "/waterboy/prompt.md", ReadOnly: true},
		container.Mount{Source: filepath.Join(run.Dir, "claude"), Target: claudeDir}, // fresh per run
	)
	if r.cfg.PluginsVolume != "" {
		mounts = append(mounts, container.Mount{Source: r.cfg.PluginsVolume, Target: claudeDir + "/plugins"})
		owned = append(owned, claudeDir+"/plugins")
	}

	env := map[string]string{
		"WATERBOY_RUN_ID": run.ID, "WATERBOY_REASON": run.Job.Reason,
		"WATERBOY_PR_URL": tr.URL, "WATERBOY_REPO": tr.Repo, "WATERBOY_PR_NUMBER": strconv.Itoa(tr.Number),
		"WATERBOY_BRANCH": tr.HeadRef, "WATERBOY_HEAD_SHA": tr.HeadSHA, "WATERBOY_WORKTREE": workdir,
		"WATERBOY_OUT":      "/out",
		"CLAUDE_CONFIG_DIR": claudeDir,
		// The container's user doesn't own the mounted files; let git work anyway.
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
	}
	maps.Copy(env, repo.Env)
	for _, k := range r.cfg.Env {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	var envFiles []string
	for _, f := range []string{repo.EnvFile, r.cfg.EnvFile} { // later files win: secrets last
		if f != "" {
			envFiles = append(envFiles, f)
		}
	}
	return container.Spec{
		Name:  "waterboy-" + strings.NewReplacer("/", "-", "#", "-").Replace(tr.Key()) + "-" + run.ID,
		Image: repo.Image, Command: []string{"bash", "-c", r.script(repo, owned)}, Env: env, EnvFiles: envFiles,
		Mounts: mounts, Workdir: workdir,
		Labels: map[string]string{"waterboy": "1", "waterboy.run": run.ID, "waterboy.pr": tr.Key()},
	}, nil
}

// claudeDir is Claude Code's config dir inside review containers.
const claudeDir = "/waterboy/claude"

// script is the container's command: own the volumes, run the repo's setup,
// then Claude headless on the rendered prompt.
func (r *Reviewer) script(repo config.RepoReview, owned []string) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	for _, d := range owned {
		fmt.Fprintf(&sb, "sudo -n chown \"$(id -u):$(id -g)\" %s 2>/dev/null || true\n", shellQuote(d))
	}
	if repo.Setup != "" {
		sb.WriteString("echo '── waterboy: setup ──'\n" + repo.Setup + "\n")
	}
	sb.WriteString("echo '── waterboy: claude ──'\n")
	args := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
		"--model", r.cfg.Model, "--max-turns", strconv.Itoa(r.cfg.MaxTurns)}
	if r.cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(r.cfg.MaxBudgetUSD, 'f', -1, 64))
	}
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	sb.WriteString("exec " + strings.Join(args, " ") + " < /waterboy/prompt.md\n")
	return sb.String()
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// volumeName is the per-repo cache volume for a workdir-relative path.
func volumeName(repo, path string) string {
	clean := func(s string) string {
		return strings.Map(func(c rune) rune {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
				return c
			}
			return '-'
		}, s)
	}
	return "waterboy-" + clean(repo) + "-" + clean(path)
}

// previousHead is the head of the PR's last successful review.
func (r *Reviewer) previousHead(prID string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	runs := r.st.Runs[prID]
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].Status == Succeeded {
			return runs[i].Job.PR.HeadSHA
		}
	}
	return ""
}

// follow streams the container's output into the run log and to subscribers,
// waits for it to exit (stopping it on cancel), then records the result. On
// shutdown it returns without touching the container, which a restart
// re-attaches to. There's no hard timeout: past review.timeout a run is only
// flagged overdue.
func (r *Reviewer) follow(appCtx, runCtx context.Context, run Run, unlock func(), reattached bool) {
	id := run.Job.PR.Key()
	overdue := time.AfterFunc(time.Until(run.Started.Add(r.cfg.Timeout.Duration)), func() {
		slog.Warn("review running longer than review.timeout", "pr", id, "run", run.ID, "timeout", r.cfg.Timeout.Duration)
		r.changed()
	})
	defer overdue.Stop()

	// Logs follows until the container exits; it gets its own context so a
	// container we failed to wait for or stop can't hold this run forever.
	logsCtx, cancelLogs := context.WithCancel(appCtx)
	defer cancelLogs()
	logsDone := make(chan struct{})
	drainLogs := func() {
		select {
		case <-logsDone:
		case <-time.After(logGrace):
			cancelLogs()
			<-logsDone
		}
	}
	var result *ClaudeResult
	os.MkdirAll(run.Dir, 0o755)
	logf, err := os.Create(filepath.Join(run.Dir, "log")) // docker replays from the start, so truncate
	if err != nil {
		slog.Warn("creating run log", "run", run.ID, "err", err)
		close(logsDone)
	} else {
		lw := &lineWriter{file: logf, onLine: func(n int, line string) {
			evs, res := parseLine(n, line)
			if res != nil {
				result = res
				r.record(id, run.ID, func(x *Run) { x.Claude = res })
			}
			r.publish(id, run.ID, evs)
		}}
		go func() {
			defer close(logsDone)
			defer logf.Close()
			r.rt.Logs(logsCtx, run.Container, lw)
			lw.Flush()
		}()
	}

	code, err := r.rt.Wait(runCtx, run.Container)
	status, why := Succeeded, error(nil)
	switch {
	case err == nil:
	case appCtx.Err() != nil:
		return // shutting down: leave the container running and the record "running"
	case runCtx.Err() != nil: // superseded, reclaimed or dropped
		why = context.Cause(runCtx)
		status = Cancelled
		bg, cancel := context.WithTimeout(context.WithoutCancel(appCtx), time.Minute)
		r.rt.Stop(bg, run.Container)
		code, _ = r.rt.Wait(bg, run.Container)
		cancel()
	default: // couldn't wait for it: don't trust it to ever exit
		r.rt.Remove(context.WithoutCancel(appCtx), run.Container)
		drainLogs()
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
	drainLogs() // the result line is in by now
	if status == Succeeded {
		switch {
		case result != nil && result.IsError:
			status, why = Failed, fmt.Errorf("claude: %s", cmp.Or(result.Error, result.Subtype))
		case code != 0:
			status, why = Failed, fmt.Errorf("exited %d before Claude finished (setup failed?)", code)
		case result == nil:
			status, why = Failed, errors.New("Claude produced no result")
		}
	}
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

// --- live output ---

// SubscribeEvents streams every run's events as they're produced, until cancel.
// A subscriber more than 1024 events behind misses events rather than stalling runs.
func (r *Reviewer) SubscribeEvents() (<-chan RunEvent, func()) {
	ch := make(chan RunEvent, 1024)
	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func (r *Reviewer) publish(prID, runID string, evs []Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range evs {
		for ch := range r.subs {
			select {
			case ch <- RunEvent{PR: prID, Run: runID, Event: e}:
			default:
			}
		}
	}
}

// ErrNoRun means there's no such run for the PR.
var ErrNoRun = errors.New("no such run")

// Events parses a run's log so far and returns the number of the next line to
// come. With SubscribeEvents: subscribe first, read this backlog, then skip live
// events with Line < next.
func (r *Reviewer) Events(prID, runID string) (evs []Event, next int, err error) {
	dir, ok := r.RunDir(prID, runID)
	if !ok {
		return nil, 0, ErrNoRun
	}
	b, err := os.ReadFile(filepath.Join(dir, "log"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	for n, line := range strings.SplitAfter(string(b), "\n") {
		if !strings.HasSuffix(line, "\n") {
			break // a partial last line arrives live once complete
		}
		e, _ := parseLine(n, strings.TrimRight(line, "\r\n"))
		evs, next = append(evs, e...), n+1
	}
	return evs, next, nil
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
	for _, s := range out {
		for i := range s.Runs {
			s.Runs[i].Overdue = s.Runs[i].Status == Running && time.Since(s.Runs[i].Started) > r.cfg.Timeout.Duration
		}
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
