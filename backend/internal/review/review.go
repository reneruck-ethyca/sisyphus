// Package review turns tracker events into review runs: a persistent queue (one
// job per PR, latest wins), a bounded set of running containers, run records on
// disk, and re-attaching to containers that outlived a restart.
package review

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/jsonfile"
	"github.com/reneruck/sisyphus/internal/tracker"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// Job is one review to run, with the PR as the tracker announced it.
type Job struct {
	PR        tracker.Tracked `json:"pr"`
	Agent     config.Agent    `json:"agent"`
	Reason    string          `json:"reason"`              // Reason*
	Attempt   int             `json:"attempt,omitempty"`   // retries so far; 0 = first try
	NotBefore time.Time       `json:"not_before,omitzero"` // a retry waits until then
	Base      string          `json:"base,omitempty"`      // diff against this commit, not GitHub's diff (evals)
}

// Why a job was queued.
const (
	ReasonNew     = "new"     // first review of a PR
	ReasonCommits = "commits" // new commits since the last one
	ReasonReplies = "replies" // new replies since the last one
	ReasonManual  = "manual"  // asked for by the user
)

// A PR's review state, in PRStatus.State; "" = idle.
const (
	StateQueued  = "queued"
	StateRunning = "running"
)

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
	Container string        `json:"container,omitempty"`      // ID
	Name      string        `json:"container_name,omitempty"` // docker name
	Started   time.Time     `json:"started"`
	Finished  time.Time     `json:"finished"`
	Status    string        `json:"status"`
	ExitCode  int           `json:"exit_code"`
	Error     string        `json:"error,omitempty"`
	Dir       string        `json:"dir"`
	Claude    *ClaudeResult `json:"claude,omitempty"`    // Claude's final result line, once it has one
	Usage     []Span        `json:"usage,omitempty"`     // the 5-hour windows it used, by Claude's rate-limit readings
	Posted    *Posted       `json:"posted,omitempty"`    // what sisyphus posted to GitHub
	Posting   bool          `json:"posting,omitempty"`   // posting started: after a crash, don't post again
	Stack     *StackRun     `json:"stack,omitempty"`     // the local stack it ran against
	PostNote  string        `json:"post_note,omitempty"` // what came of posting, in a line (succeeded runs)
	Overdue   bool          `json:"overdue,omitempty"`   // still running past review.timeout (set by Status)
}

// runEvent is an Event of one run, as streamed to subscribers.
type runEvent struct {
	PR  string `json:"pr"`
	Run string `json:"run"`
	Event
}

// PRStatus is what UIs show for one PR.
type PRStatus struct {
	State  string `json:"state"`            // "" | StateQueued | StateRunning
	Runs   []Run  `json:"runs"`             // newest first
	Queued *Job   `json:"queued,omitempty"` // the job waiting to run, if any
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
	errReset      = errors.New("reset")
)

// retryable marks a failure before Claude started (setup, the stack, starting
// the container): likely transient, so the run is retried with backoff. Claude's
// own failures and posting failures aren't: a re-run would likely fail the same way.
type retryable struct{ error }

func (e retryable) Unwrap() error { return e.error }

type store struct {
	Queue  []Job                `json:"queue"`
	Runs   map[string][]Run     `json:"runs"`             // by PR ID, oldest first
	Stacks map[string]*StackRun `json:"stacks,omitempty"` // each PR's latest stack, until removed
}

// Host is what a Reviewer asks of the app around it, from any goroutine.
type Host interface {
	Kick() // ask for a tick soon: a reclaimed worktree is free
	// Refresh fetches tr's head and syncs its worktree before a run starts.
	Refresh(ctx context.Context, tr tracker.Tracked) (tracker.Tracked, error)
	Changed() // something the UIs show changed
}

// nopHost is a Reviewer's host until SetHost: it trusts the job's head.
type nopHost struct{}

func (nopHost) Kick()    {}
func (nopHost) Changed() {}
func (nopHost) Refresh(_ context.Context, tr tracker.Tracked) (tracker.Tracked, error) {
	return tr, nil
}

// SetHost connects r to the app; call it before Run and Reattach.
func (r *Reviewer) SetHost(h Host) { r.host = h }

type Reviewer struct {
	cfg   config.Review
	rt    container.Runtime
	locks *tracker.Locks
	dir   string // reviews.json and runs/ live here

	GitHub GitHub // host-side GitHub access: discussion files, posting; nil = neither
	host   Host   // see SetHost

	wg          sync.WaitGroup // goroutines Run and Reattach start; Wait for them after ctx ends
	mu          sync.Mutex
	st          store
	active      map[string]context.CancelCauseFunc // running, by PR ID
	wake        chan struct{}
	unavailable string // why the runtime can't start containers right now
	overBudget  bool   // the 5-hour budget held reviews back at the last dispatch
	retrying    bool   // a runtime re-check is scheduled
	subs        map[chan runEvent]struct{}
	skipped     map[string]bool // repos without review config and missing env files, logged once
	sh          func(ctx context.Context, dir string, env []string, script string, out io.Writer) error
}

func New(cfg config.Review, rt container.Runtime, locks *tracker.Locks, dir string) (*Reviewer, error) {
	r := &Reviewer{cfg: cfg, rt: rt, locks: locks, dir: dir, st: store{Runs: map[string][]Run{}},
		active: map[string]context.CancelCauseFunc{}, wake: make(chan struct{}, 1),
		subs: map[chan runEvent]struct{}{}, skipped: map[string]bool{}, sh: shell, host: nopHost{}}
	if err := jsonfile.Load(r.storePath(), &r.st); err != nil {
		return nil, err
	}
	if r.st.Runs == nil {
		r.st.Runs = map[string][]Run{}
	}
	if r.st.Stacks == nil {
		r.st.Stacks = map[string]*StackRun{}
	}
	for name, repo := range cfg.Repos { // surface a missing secrets file at startup
		r.envFilesMissing(name, repo)
	}
	return r, nil
}

func (r *Reviewer) storePath() string { return filepath.Join(r.dir, "reviews.json") }

// --- tracker.Events (called on the ticking goroutine; never block) ---

func (r *Reviewer) Tracked(tr tracker.Tracked) {
	if r.reviewable(tr) && r.automatic(tr) {
		r.enqueue(r.newJob(tr, ReasonNew))
	}
}

func (r *Reviewer) Updated(tr tracker.Tracked, newCommits bool, _ int) {
	if !r.reviewable(tr) || !r.automatic(tr) {
		return
	}
	reason := ReasonReplies // replies don't interrupt a running review; they queue behind it
	if newCommits {
		reason = ReasonCommits
		r.cancel(tr.Key(), errSuperseded)
	}
	r.enqueue(r.newJob(tr, reason))
}

func (r *Reviewer) Reclaim(tr tracker.Tracked) { r.cancel(tr.Key(), errReclaimed) }

func (r *Reviewer) Dropped(tr tracker.Tracked, _ string) {
	id := tr.Key()
	r.mu.Lock()
	r.st.Queue = slices.DeleteFunc(r.st.Queue, func(j Job) bool { return j.PR.Key() == id })
	r.saveLocked()
	r.mu.Unlock()
	r.cancel(id, errDropped)
	r.spawn(func() { r.dropStack(id) }) // after the cancelled run lets go of the worktree
	r.changed()
}

// reviewable: only repos with a review.repos entry are reviewed. (Fork PRs never
// get here: the tracker doesn't track them.)
func (r *Reviewer) reviewable(tr tracker.Tracked) bool {
	if _, ok := r.cfg.Repos[tr.Repo]; !ok {
		if r.once(tr.Repo) {
			slog.Info("not reviewing: no review.repos entry", "repo", tr.Repo)
		}
		return false
	}
	return true
}

// automatic: design reviews are reviewed only on request (Trigger).
func (r *Reviewer) automatic(tr tracker.Tracked) bool {
	if tr.DesignReview && r.once("design:"+tr.Key()+"@"+tr.HeadSHA) {
		slog.Info("not reviewing: design review, review it by hand", "pr", tr.Key(), "head", tr.HeadSHA)
	}
	return !tr.DesignReview
}

// newJob snapshots the effective profile so a queued review is reproducible
// even if the app-wide default or the PR override changes before it starts.
func (r *Reviewer) newJob(tr tracker.Tracked, reason string) Job {
	agent := r.cfg.Agent
	if tr.Agent != nil {
		agent = *tr.Agent
	}
	return Job{PR: tr, Agent: agent, Reason: reason}
}

// Blocked says why repo's reviews can't run now ("" = they can): no review
// config, or a missing env file, which docker refuses to start without. Queued
// jobs wait for it; it's re-checked every minute, so creating a missing file
// needs no restart.
func (r *Reviewer) Blocked(repo string) string {
	cfg, ok := r.cfg.Repos[repo]
	if !ok {
		return "no review.repos entry for " + repo
	}
	return r.envFilesMissing(repo, cfg)
}

// envFilesMissing says which of repo's env files isn't there ("" = all are). Each missing file is logged once.
func (r *Reviewer) envFilesMissing(name string, repo config.RepoReview) string {
	for _, f := range []string{repo.EnvFile, r.cfg.EnvFile} {
		if f == "" {
			continue
		}
		if _, err := os.Stat(f); err != nil {
			if r.once("env:" + f) {
				slog.Error("reviews wait: env file missing", "repo", name, "file", f, "err", err)
			}
			return "env file missing: " + f
		}
	}
	return ""
}

// Ensure queues a first review for tracked PRs that never had one (tracked
// while their repo wasn't reviewable, or before a crash). Call after each tick.
func (r *Reviewer) Ensure(prs []tracker.Tracked) {
	for _, tr := range prs {
		if _, ok := r.cfg.Repos[tr.Repo]; !ok || tr.DesignReview || tr.AnnouncedSHA == "" || tr.AnnouncedSHA != tr.HeadSHA {
			continue // not reviewed here, reviewed only by hand, or not synced to its head yet
		}
		id := tr.Key()
		r.mu.Lock()
		none := len(r.st.Runs[id]) == 0 && r.active[id] == nil &&
			!slices.ContainsFunc(r.st.Queue, func(j Job) bool { return j.PR.Key() == id })
		r.mu.Unlock()
		if none {
			r.enqueue(r.newJob(tr, ReasonNew))
		}
	}
}

// once reports whether key is new to r.skipped, marking it seen.
func (r *Reviewer) once(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := !r.skipped[key]
	r.skipped[key] = true
	return first
}

// Forget wipes a PR's review state ("" = every PR's): its queued job, run records
// and run directories, and its review containers; a review this process is
// running is stopped. Meant for `sisyphus -reset`, with the app stopped.
func (r *Reviewer) Forget(ctx context.Context, prID string) error { return r.forget(ctx, prID, true) }

// forget is Forget, leaving stacks alone unless withStacks.
func (r *Reviewer) forget(ctx context.Context, prID string, withStacks bool) error {
	match := func(id string) bool { return prID == "" || id == prID }
	r.mu.Lock()
	r.st.Queue = slices.DeleteFunc(r.st.Queue, func(j Job) bool { return match(j.PR.Key()) })
	for id := range r.st.Runs {
		if match(id) {
			delete(r.st.Runs, id)
		}
	}
	var cancels []context.CancelCauseFunc
	for id, c := range r.active {
		if match(id) {
			cancels = append(cancels, c)
		}
	}
	r.saveLocked()
	r.mu.Unlock()
	for _, c := range cancels {
		c(errReset)
	}
	labels := map[string]string{"sisyphus": "1"}
	if prID != "" {
		labels["sisyphus.pr"] = prID
	}
	cs, err := r.rt.List(ctx, labels)
	for _, c := range cs {
		err = errors.Join(err, r.rt.Remove(ctx, c.ID))
	}
	if withStacks {
		err = errors.Join(err, r.removeStacks(match))
	}
	err = errors.Join(err, os.RemoveAll(filepath.Join(r.dir, "runs", prPath(prID))))
	slog.Info("forgot reviews", "pr", cmp.Or(prID, "all"), "containers_removed", len(cs))
	r.changed()
	return err
}

// Trigger queues a full review of a PR on request, behind a run in progress, and
// replacing a waiting retry.
func (r *Reviewer) Trigger(tr tracker.Tracked) error {
	if _, ok := r.cfg.Repos[tr.Repo]; !ok {
		return fmt.Errorf("%s isn't reviewed: no review.repos entry", tr.Repo)
	}
	if tr.AnnouncedSHA == "" || tr.AnnouncedSHA != tr.HeadSHA {
		return fmt.Errorf("%s isn't synced to its head %.12s yet; try after the next sync", tr.Key(), tr.HeadSHA)
	}
	slog.Info("review requested", "pr", tr.Key())
	r.enqueue(r.newJob(tr, ReasonManual))
	return nil
}

// retry queues a failed run's job again, after a backoff doubling with each
// attempt, unless the retries are used up or a newer job is queued already.
func (r *Reviewer) retry(run Run) {
	j := run.Job
	id := j.PR.Key()
	if j.Attempt >= r.cfg.Retries {
		if r.cfg.Retries > 0 {
			slog.Warn("review failed, no retries left", "pr", id, "run", run.ID, "retries", r.cfg.Retries)
		}
		return
	}
	wait := r.cfg.RetryBackoff.Duration << j.Attempt
	j.Attempt++
	j.NotBefore = time.Now().Add(wait)
	if r.requeue(j) {
		slog.Info("review retry queued", "pr", id, "attempt", j.Attempt, "of", r.cfg.Retries, "at", j.NotBefore.Format(time.TimeOnly))
	}
}

// requeue queues j again unless a job for its PR is queued already: that newer
// job (new head) must win. It says whether j was queued.
func (r *Reviewer) requeue(j Job) bool {
	r.mu.Lock()
	queued := slices.ContainsFunc(r.st.Queue, func(q Job) bool { return q.PR.Key() == j.PR.Key() })
	if !queued { // checked and added under one lock
		r.st.Queue = append(r.st.Queue, j)
		r.saveLocked()
	}
	r.mu.Unlock()
	if !queued {
		r.changed()
	}
	return !queued
}

var rank = map[string]int{ReasonManual: 0, ReasonReplies: 1, ReasonCommits: 2, ReasonNew: 3}

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
	if why := r.Blocked(j.PR.Repo); why != "" {
		slog.Warn("review queued, waiting", "pr", j.PR.Key(), "reason", j.Reason, "head", j.PR.HeadSHA, "waiting_for", why)
	} else {
		slog.Info("review queued", "pr", j.PR.Key(), "reason", j.Reason, "head", j.PR.HeadSHA)
	}
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
	var tasks []struct {
		key string
		run Run
	}
	for key, runs := range r.st.Runs {
		for _, run := range runs {
			switch {
			case run.Status != Running:
			case strings.HasPrefix(key, taskPrefix):
				tasks = append(tasks, struct {
					key string
					run Run
				}{key, run}) // its caller is gone too: the next tick asks again
			default:
				left = append(left, run)
			}
		}
	}
	r.mu.Unlock()
	for _, t := range tasks {
		r.record(t.key, t.run.ID, func(x *Run) {
			x.Status, x.Error, x.Finished = Lost, "lost in a restart", time.Now()
		})
	}
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
		r.spawn(func() { r.reattach(ctx, runCtx, run, unlock) })
	}
	r.spawn(func() { r.sweepOrphans(ctx) })
}

func (r *Reviewer) reattach(appCtx, runCtx context.Context, run Run, unlock func()) {
	id := run.Job.PR.Key()
	giveUp := func(err error) {
		if appCtx.Err() != nil {
			return // shutting down again; try next start
		}
		if run.Stack != nil { // while it still holds the worktree, as finish does
			r.stopStack(run.Stack.Project)
		}
		unlock()
		r.mu.Lock()
		delete(r.active, id)
		r.mu.Unlock()
		r.lose(run, err)
		if errors.Is(err, errReclaimed) {
			r.host.Kick()
		}
		r.poke()
	}
	if err := r.waitRuntime(runCtx); err != nil {
		giveUp(context.Cause(runCtx))
		return
	}
	infos, err := r.rt.List(runCtx, map[string]string{"sisyphus.run": run.ID, "sisyphus.pr": id})
	if err != nil {
		giveUp(fmt.Errorf("couldn't re-attach after restart: %w", err))
		return
	}
	if len(infos) != 1 {
		for _, c := range infos {
			r.removeContainer(context.WithoutCancel(appCtx), run.ID, c.ID)
		}
		giveUp(fmt.Errorf("couldn't re-attach after restart: %d containers for the run", len(infos)))
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
	cs, err := r.rt.List(ctx, map[string]string{"sisyphus": "1"})
	if err != nil {
		return
	}
	r.mu.Lock()
	running := map[string]bool{} // by PR (or task) and run: run IDs are unique only per PR
	for key, runs := range r.st.Runs {
		for _, run := range runs {
			running[key+" "+run.ID] = run.Status == Running
		}
	}
	r.mu.Unlock()
	for _, c := range cs {
		key := cmp.Or(c.Labels["sisyphus.pr"], taskPrefix+c.Labels["sisyphus.task"])
		if !running[key+" "+c.Labels["sisyphus.run"]] {
			slog.Warn("removing orphaned review container", "container", c.ID, "run", c.Labels["sisyphus.run"])
			r.removeContainer(ctx, c.Labels["sisyphus.run"], c.ID)
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

// lose marks a run that can't be followed any more and queues its job again,
// unless a newer one is queued.
func (r *Reviewer) lose(run Run, err error) {
	slog.Warn("review lost, requeueing", "pr", run.Job.PR.Key(), "run", run.ID, "err", err)
	dropRelated(run)
	r.record(run.Job.PR.Key(), run.ID, func(x *Run) { x.Status, x.Error, x.Finished = Lost, err.Error(), time.Now() })
	r.requeue(run.Job)
}

// Run starts queued jobs, at most MaxConcurrent at a time, until ctx ends.
// Containers keep running when ctx ends, so a restart can re-attach to them.
func (r *Reviewer) Run(ctx context.Context) {
	for {
		var recheck <-chan time.Time
		if d := r.dispatch(ctx); d > 0 {
			recheck = time.After(d) // jobs wait on Blocked or a retry's backoff: look again
		}
		select {
		case <-r.wake:
		case <-recheck:
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

// Slots says how many of the review.max_concurrent run slots are taken, by reviews and agent tasks alike.
func (r *Reviewer) Slots() (used, max int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.active), r.cfg.MaxConcurrent
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

// checkBudget says whether the 5-hour budget lets new reviews start, logging
// when that changes.
func (r *Reviewer) checkBudget() bool {
	b := r.Budget()
	r.mu.Lock()
	was := r.overBudget
	r.overBudget = b.Exhausted()
	r.mu.Unlock()
	switch {
	case b.Exhausted() && !was:
		slog.Warn("5-hour budget used, reviews wait for the reset", "used", b.Used, "share", b.Share, "resets", b.ResetsAt.Local().Format(time.TimeOnly))
		r.changed()
	case !b.Exhausted() && was:
		slog.Info("5-hour budget available again")
		r.changed()
	}
	return !b.Exhausted()
}

// budgetWait is how long until the window resets while the budget holds reviews back (0 = it doesn't).
func (r *Reviewer) budgetWait() time.Duration {
	r.mu.Lock()
	over := r.overBudget
	r.mu.Unlock()
	if !over {
		return 0
	}
	return max(time.Until(r.Budget().ResetsAt), time.Second)
}

// dispatch starts what it can and says when to look again for jobs waiting on
// Blocked or a retry's backoff (0 = no need).
func (r *Reviewer) dispatch(ctx context.Context) (recheck time.Duration) {
	r.mu.Lock()
	pending := len(r.st.Queue) > 0 && len(r.active) < r.cfg.MaxConcurrent
	r.mu.Unlock()
	if !pending || !r.checkBudget() || !r.checkRuntime(ctx) {
		return r.budgetWait()
	}
	later := func(d time.Duration) {
		if recheck == 0 || d < recheck {
			recheck = d
		}
	}
	r.mu.Lock()
	repos := map[string]bool{}
	for _, j := range r.st.Queue {
		repos[j.PR.Repo] = true
	}
	r.mu.Unlock()
	blocked := map[string]string{} // checked outside mu: Blocked stats files and takes mu
	for repo := range repos {
		blocked[repo] = r.Blocked(repo)
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
		wait := time.Until(j.NotBefore)
		if blocked[j.PR.Repo] != "" {
			later(retryEvery)
		} else if wait > 0 {
			later(wait)
		}
		if r.active[id] != nil || blocked[j.PR.Repo] != "" || wait > 0 {
			i++ // one run per PR, none while its repo is blocked or before a retry's time; the job waits
			continue
		}
		r.st.Queue = slices.Delete(r.st.Queue, i, i+1)
		now := time.Now()
		runID := newRunID(now)
		run := Run{ID: runID, Job: j, Image: r.cfg.Repos[j.PR.Repo].Image, Started: now, Status: Running,
			Dir: filepath.Join(r.dir, "runs", prPath(id), runID)}
		r.appendRunLocked(run.Job.PR.Key(), run) // saved with the dequeue, so a crash here is seen as "lost"
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
		r.spawn(func() { r.execute(ctx, s.runCtx, s.run) })
	}
	return recheck
}

var lastRunID struct {
	sync.Mutex
	at time.Time
}

// newRunID is a readable, URL- and path-safe UTC timestamp, a millisecond past
// the last one issued if need be, so this process never hands out one twice.
func newRunID(now time.Time) string {
	lastRunID.Lock()
	defer lastRunID.Unlock()
	at := now.UTC().Truncate(time.Millisecond)
	if !at.After(lastRunID.at) {
		at = lastRunID.at.Add(time.Millisecond)
	}
	lastRunID.at = at
	return at.Format("20060102-150405.000")
}

func (r *Reviewer) execute(appCtx, runCtx context.Context, run Run) {
	tr := run.Job.PR
	id := tr.Key()
	fresh, err := r.host.Refresh(runCtx, tr)
	if err != nil {
		r.endEarly(appCtx, runCtx, run, err, nil)
		return
	}
	if fresh.HeadSHA != tr.HeadSHA { // the tracker queues the new head once it's synced
		r.cancel(id, errSuperseded)
		r.endEarly(appCtx, runCtx, run, errSuperseded, nil)
		return
	}
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
	if st := r.cfg.Repos[tr.Repo].Stack; st != nil {
		sr, err := r.startStack(runCtx, run, st)
		run.Stack = sr // finish stops it, whatever happens from here
		r.record(id, run.ID, func(x *Run) { x.Stack = sr })
		if err != nil {
			fail(err)
			return
		}
	}
	spec, err := r.spec(runCtx, run, "")
	if err != nil {
		fail(err)
		return
	}
	cid, err := r.rt.Start(runCtx, spec)
	if err != nil {
		if appCtx.Err() == nil {
			// `docker run` failing doesn't mean the daemon didn't start it.
			r.removeRun(context.WithoutCancel(appCtx), spec.Labels)
		}
		fail(err)
		return
	}
	run.Container = cid
	r.record(id, run.ID, func(x *Run) { x.Container, x.Name = cid, spec.Name })
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
		if errors.Is(why, errReclaimed) {
			r.host.Kick()
		}
		return
	}
	slog.Warn("review not started", "pr", run.Job.PR.Key(), "run", run.ID, "err", err)
	r.finish(run, Errored, 0, retryable{err}, unlock)
}

// removeRun removes the containers carrying a run's labels (its PR's or task's
// too: run IDs alone aren't unique across them).
func (r *Reviewer) removeRun(ctx context.Context, labels map[string]string) {
	runID := labels["sisyphus.run"]
	cs, err := r.rt.List(ctx, labels)
	if err != nil {
		slog.Warn("listing review containers", "run", runID, "err", err)
	}
	for _, c := range cs {
		r.removeContainer(ctx, runID, c.ID)
	}
}

// removeContainer removes a review container, logging a failure: the orphan
// sweep at the next start retries it.
func (r *Reviewer) removeContainer(ctx context.Context, runID, id string) {
	if err := r.rt.Remove(ctx, id); err != nil {
		slog.Warn("removing review container", "run", runID, "container", id, "err", err)
	}
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
// tailing follows a container's output into its run's log, recording the
// agent's result and usage on the run as they come.
type tailing struct {
	lw     *lineWriter   // nil when the log couldn't be created
	result *ClaudeResult // the agent's final result; read it after drain
	done   chan struct{}
	cancel context.CancelFunc
}

// drain waits for the output to end, at most logGrace, then stops following.
func (t *tailing) drain() {
	select {
	case <-t.done:
	case <-time.After(logGrace):
		t.cancel()
		<-t.done
	}
}

// tail starts following container cid of run (recorded under key) until it
// exits or ctx ends: a context of its own, so a container that won't exit
// can't hold the caller past drain. hook, if set, sees each line first and
// keeps it from being parsed by returning true. An error means there's no log;
// the returned tailing still drains.
func (r *Reviewer) tail(ctx context.Context, key string, run Run, cid string, hook func(string) bool) (*tailing, error) {
	ctx, cancel := context.WithCancel(ctx)
	t := &tailing{done: make(chan struct{}), cancel: cancel}
	_ = os.MkdirAll(run.Dir, 0o755)                       // a failure shows as os.Create's
	logf, err := os.Create(filepath.Join(run.Dir, "log")) // docker replays from the start, so truncate
	if err != nil {
		close(t.done)
		return t, err
	}
	t.lw = &lineWriter{file: logf, onLine: func(n int, line string) {
		if hook != nil && hook(line) {
			return
		}
		evs, res, w := parseLine(n, line)
		if res != nil {
			t.result = res
			r.record(key, run.ID, func(x *Run) { x.Claude = res })
		}
		usage := w != nil && w.plausible(time.Now())
		if usage {
			r.record(key, run.ID, func(x *Run) { x.Usage = observe(x.Usage, *w) })
		}
		if res != nil || usage {
			r.logUsage(key, run.ID, line)
		}
		r.publish(key, run.ID, evs)
	}}
	go func() {
		defer close(t.done)
		defer logf.Close()
		if err := r.rt.Logs(ctx, cid, t.lw); err != nil && ctx.Err() == nil {
			slog.Warn("following run output", "run", run.ID, "err", err)
		}
		t.lw.Flush()
	}()
	return t, nil
}

func (r *Reviewer) follow(appCtx, runCtx context.Context, run Run, unlock func(), reattached bool) {
	id := run.Job.PR.Key()
	overdue := time.AfterFunc(time.Until(run.Started.Add(r.cfg.Timeout.Duration)), func() {
		slog.Warn("review running longer than review.timeout", "pr", id, "run", run.ID, "timeout", r.cfg.Timeout.Duration)
		r.changed()
	})
	defer overdue.Stop()

	logs, err := r.tail(appCtx, id, run, run.Container, nil)
	defer logs.cancel()
	if err != nil {
		slog.Warn("creating run log", "run", run.ID, "err", err)
	}
	drainLogs := logs.drain

	code, err := r.rt.Wait(runCtx, run.Container)
	status, why := Succeeded, error(nil)
	switch {
	case err == nil:
	case appCtx.Err() != nil: // shutting down: leave the container running and the record "running"
		drainLogs() // so the log goroutine doesn't write the record after Wait
		return
	case runCtx.Err() != nil: // superseded, reclaimed or dropped
		why = context.Cause(runCtx)
		status = Cancelled
		bg, cancel := context.WithTimeout(context.WithoutCancel(appCtx), time.Minute)
		if err := r.rt.Stop(bg, run.Container); err != nil {
			slog.Warn("stopping review container", "run", run.ID, "err", err) // removed below either way
		}
		code, _ = r.rt.Wait(bg, run.Container)
		cancel()
	default: // couldn't wait for it: don't trust it to ever exit
		r.removeContainer(context.WithoutCancel(appCtx), run.ID, run.Container)
		drainLogs()
		if reattached {
			if run.Stack != nil {
				r.stopStack(run.Stack.Project)
			}
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
	result := logs.result
	if status == Succeeded && runCtx.Err() != nil && appCtx.Err() == nil { // exited just as it was cancelled: still don't post
		status, why = Cancelled, context.Cause(runCtx)
	}
	if status == Succeeded {
		switch {
		case result != nil && result.IsError:
			status, why = Failed, fmt.Errorf("%s: %s", agentFor(r.cfg, run.Job).Vendor, cmp.Or(result.Error, result.Subtype))
		case code != 0 && result == nil:
			status, why = Failed, retryable{fmt.Errorf("exited %d before Claude finished (setup failed?)", code)}
		case code != 0:
			status, why = Failed, fmt.Errorf("exited %d after Claude's result", code)
		case result == nil:
			status, why = Failed, errors.New("claude produced no result")
		}
	}
	bg := context.WithoutCancel(appCtx)
	if status == Succeeded {
		var posted *Posted
		var err error
		design := run.Job.PR.DesignReview // its findings wait for the user, who posts what they approve
		if r.cfg.Post && r.GitHub != nil && !design {
			if run.Posting { // GitHub may have it already; posting twice is worse than not at all
				err = errors.New("posting was interrupted by a restart; check GitHub before posting again")
			} else {
				r.record(id, run.ID, func(x *Run) { x.Posting = true })
				posted, err = r.post(bg, run)
			}
		}
		note := postNote(posted, err, r.cfg.Post && r.GitHub != nil)
		if design {
			note = "not posted: design review, the findings are in out/review.json for you to post"
		}
		r.record(id, run.ID, func(x *Run) { x.Posted, x.PostNote = posted, note })
		r.appendLog(id, run, logs.lw, "── sisyphus: "+note)
		slog.Info("review "+strings.SplitN(note, ":", 2)[0], "pr", id, "run", run.ID, "note", note)
		if err != nil {
			status, why = Failed, fmt.Errorf("posting review: %w", err)
		}
	}
	r.removeContainer(bg, run.ID, run.Container)
	var cleanupErr error
	if r.cfg.Cleanup && run.Job.PR.CreatedWorktree { // reused worktrees may hold the user's own work
		cleanupErr = worktree.Reset(bg, run.Job.PR.Worktree, run.Job.PR.HeadSHA)
	}
	r.finish(run, status, code, errors.Join(why, cleanupErr), unlock)
	if errors.Is(why, errReclaimed) {
		r.host.Kick() // the tracker is waiting for this worktree
	}
}

// postNote says in a line what came of posting a succeeded run's review.
func postNote(p *Posted, err error, posting bool) string {
	switch {
	case !posting:
		return "not posted: review.post is off (dry run); the review is in out/review.json"
	case err != nil:
		return "posting failed: " + err.Error()
	case p == nil:
		return "nothing posted: the agent wrote no out/review.json"
	}
	var parts []string
	if p.Event != "" {
		parts = append(parts, p.Event)
		if p.Comments > 0 {
			parts[0] += fmt.Sprintf(" with %d inline comments", p.Comments)
		}
		if p.Fallback {
			parts[0] += " (as one plain comment: GitHub refused the review)"
		}
	}
	if p.Replies > 0 {
		parts = append(parts, fmt.Sprintf("%d replies", p.Replies))
	}
	note := "posted " + strings.Join(parts, " and ")
	if p.URL != "" {
		note += ": " + p.URL
	}
	return note
}

// appendLog adds a sisyphus line after the container's output, in the run log
// and to live subscribers. lw is the run's log writer, done by now (nil = no log).
func (r *Reviewer) appendLog(prID string, run Run, lw *lineWriter, line string) {
	if lw == nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(run.Dir, "log"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		slog.Warn("appending to run log", "run", run.ID, "err", err)
		return
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err == nil {
		evs, _, _ := parseLine(lw.n, line)
		r.publish(prID, run.ID, evs)
	}
}

// --- live output ---

// subscribe streams every run's events as they're produced, until cancel.
// A subscriber more than 1024 events behind misses events rather than stalling runs.
func (r *Reviewer) subscribe() (<-chan runEvent, func()) {
	ch := make(chan runEvent, 1024)
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
			case ch <- runEvent{PR: prID, Run: runID, Event: e}:
			default:
			}
		}
	}
}

// ErrNoRun means there's no such run for the PR.
var ErrNoRun = errors.New("no such run")

// Events parses a run's log so far and returns the number of the next line to come.
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
		e, _, _ := parseLine(n, strings.TrimRight(line, "\r\n"))
		evs, next = append(evs, e...), n+1
	}
	return evs, next, nil
}

// Follow streams a run's events from line from on: the backlog, then live ones
// with no gap or repeat between them. The channel closes once the run is over
// and its last events are sent, or when ctx ends.
func (r *Reviewer) Follow(ctx context.Context, prID, runID string, from int) (<-chan Event, error) {
	live, cancel := r.subscribe() // before the backlog, so nothing falls in between
	backlog, next, err := r.Events(prID, runID)
	if err != nil {
		cancel()
		return nil, err
	}
	out := make(chan Event)
	go func() {
		defer close(out)
		defer cancel()
		send := func(e Event) bool {
			select {
			case out <- e:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for _, e := range backlog {
			if e.Line >= from && !send(e) {
				return
			}
		}
		from = max(next, from)
		mine := func(e runEvent) bool { return e.PR == prID && e.Run == runID && e.Line >= from }
		// ponytail: polls the run's status; a finish signal on the subscription if a second's lag matters
		check := time.NewTicker(followPoll)
		defer check.Stop()
		for {
			select {
			case e := <-live:
				if mine(e) && !send(e.Event) {
					return
				}
			case <-check.C:
				if r.running(prID, runID) {
					continue
				}
				for { // what the run printed last is already queued
					select {
					case e := <-live:
						if mine(e) && !send(e.Event) {
							return
						}
					default:
						return
					}
				}
			case <-ctx.Done():
				return
			}
		}
	}()
	return out, nil
}

var followPoll = time.Second

func (r *Reviewer) running(prID, runID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, run := range r.st.Runs[prID] {
		if run.ID == runID {
			return run.Status == Running
		}
	}
	return false
}

// finish stops the stack, releases the worktree, then records the outcome and
// frees the run slot: a run is over only once it has let go of everything.
func (r *Reviewer) finish(run Run, status string, code int, err error, unlock func()) {
	id := run.Job.PR.Key()
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	if run.Stack != nil { // still holding the worktree, so the next run's up comes after
		switch status {
		case Succeeded, Cancelled: // kept, stopped: the PR's next review starts on the same DB
			r.stopStack(run.Stack.Project)
		default: // failed or errored: its DB may be half set up, and nothing should be left behind
			_ = r.removeStacks(func(prID string) bool { return prID == id }) // logged; stays recorded for the next try
		}
	}
	dropRelated(run)
	if unlock != nil {
		unlock()
	}
	r.record(id, run.ID, func(x *Run) { x.Status, x.ExitCode, x.Error, x.Finished = status, code, msg, time.Now() })
	if errors.As(err, new(retryable)) {
		r.retry(run) // queued while this run still counts as active, so it can't start early
	}
	r.mu.Lock()
	delete(r.active, id)
	r.mu.Unlock()
	slog.Info("review finished", "pr", id, "run", run.ID, "status", status, "exit_code", code, "err", msg)
	r.changed()
	r.poke()
}

func (r *Reviewer) spawn(fn func()) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		fn()
	}()
}

// Wait blocks until every run goroutine has returned; call it once Run's ctx
// has ended, so nothing writes run records or files after shutdown.
func (r *Reviewer) Wait() { r.wg.Wait() }

func (r *Reviewer) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// --- records ---

// appendRunLocked records run under key (a PR's ID, or a task's), keeping the last keepRuns.
func (r *Reviewer) appendRunLocked(key string, run Run) {
	runs := append(r.st.Runs[key], run)
	r.st.Runs[key] = runs[max(0, len(runs)-keepRuns):]
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
	r.host.Changed() // not changed(): a record alone frees no slot, and dispatch may ask docker
}

func (r *Reviewer) writeRunJSON(run Run) {
	if err := jsonfile.Save(filepath.Join(run.Dir, "run.json"), run); err != nil {
		slog.Warn("writing run.json", "run", run.ID, "err", err)
	}
}

// ponytail: rewrites all of reviews.json on every record, several times per run;
// fine at a review queue's size, append-only per-run files if it grows.
func (r *Reviewer) saveLocked() {
	if err := jsonfile.Save(r.storePath(), r.st); err != nil {
		slog.Error("saving reviews", "err", err)
	}
}

func (r *Reviewer) changed() {
	r.poke()
	r.host.Changed()
}

// Status returns every PR's review state and recent runs, by PR ID.
func (r *Reviewer) Status() map[string]PRStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string]PRStatus{}
	for id, runs := range r.st.Runs {
		if strings.HasPrefix(id, taskPrefix) {
			continue
		}
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
		set(j.PR.Key(), StateQueued)
		s := out[j.PR.Key()]
		s.Queued = &j
		out[j.PR.Key()] = s
	}
	for id := range r.active {
		if !strings.HasPrefix(id, taskPrefix) {
			set(id, StateRunning)
		}
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
