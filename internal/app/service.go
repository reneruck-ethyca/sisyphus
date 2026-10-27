package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/review"
	"github.com/reneruck/waterboy/internal/tracker"
)

// Status is a point-in-time snapshot of waterboy for UIs and APIs.
type Status struct {
	User      string    `json:"user"`
	Org       string    `json:"org"`
	Interval  string    `json:"interval"`
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run"`
	LastError string    `json:"last_error,omitempty"` // of the last tick as a whole
	PRs       []PRView  `json:"prs"`                  // sorted by ID
	Reviews   bool      `json:"reviews"`              // false when review.disabled
	// Why reviews are waiting on the container runtime ("" = they aren't).
	RuntimeError string         `json:"runtime_error,omitempty"`
	Reviewing    []ActiveReview `json:"reviewing"` // review runs in progress, oldest first
}

// ActiveReview is a review run in progress and its container.
type ActiveReview struct {
	PR        string    `json:"pr"`
	Run       string    `json:"run"`
	Reason    string    `json:"reason"`
	Container string    `json:"container"`      // ID; "" while it's being started
	Name      string    `json:"container_name"` // "" for one re-attached after a restart
	Started   time.Time `json:"started"`
	Overdue   bool      `json:"overdue"`
}

func (s Status) Summary() string {
	out := fmt.Sprintf("%d tracked", len(s.PRs))
	for _, state := range []string{"running", "queued"} {
		n := 0
		for _, pr := range s.PRs {
			if pr.Review.State == state {
				n++
			}
		}
		if n > 0 {
			out += fmt.Sprintf(" · %d review %s", n, state)
		}
	}
	return out
}

// PRView is a tracked PR as UIs see it: a copy of its state plus derived fields.
type PRView struct {
	tracker.Tracked
	ID         string          `json:"id"`     // "owner/repo#n"
	Health     string          `json:"health"` // "conflict" | "error" | "ok" (tracking, not review)
	Review     review.PRStatus `json:"review"`
	ReviewNote string          `json:"review_note"` // why a review is or isn't happening, in a line
}

// reviewNote says in a line where the PR's review stands and why. blocked and
// runtimeErr are what keeps queued reviews from starting ("" = nothing).
func reviewNote(tr tracker.Tracked, rs review.PRStatus, reviews bool, blocked, runtimeErr string) string {
	if !reviews {
		return "reviews are disabled"
	}
	short := func(sha string) string { return sha[:min(len(sha), 8)] }
	switch rs.State {
	case "running":
		return "reviewing (" + rs.Runs[0].Job.Reason + ")"
	case "queued":
		switch {
		case rs.Queued != nil && time.Until(rs.Queued.NotBefore) > 0:
			why := ""
			if last := rs.Latest(); last != nil && last.Error != "" {
				why = " after: " + last.Error
			}
			return fmt.Sprintf("queued: retry %d at %s%s", rs.Queued.Attempt, rs.Queued.NotBefore.Local().Format("15:04"), why)
		case blocked != "":
			return "queued, waiting: " + blocked
		case runtimeErr != "":
			return "queued, waiting for the container runtime"
		}
		return "queued"
	}
	last := rs.Latest()
	switch {
	case last == nil && blocked != "":
		return "not reviewed: " + blocked
	case last == nil:
		return "not reviewed yet: waits for the head to be synced"
	case last.Status != review.Succeeded:
		return fmt.Sprintf("last review %s: %s; next one on new commits or replies", last.Status, cmp.Or(last.Error, "no details"))
	case last.Job.PR.HeadSHA != tr.HeadSHA:
		return fmt.Sprintf("reviewed %s; head %s not synced yet", short(last.Job.PR.HeadSHA), short(tr.HeadSHA))
	}
	return fmt.Sprintf("up to date: reviewed %s at %s, %s; next one on new commits or replies",
		short(last.Job.PR.HeadSHA), last.Finished.Local().Format("Jan 2 15:04"), cmp.Or(last.PostNote, "not posted"))
}

func health(tr tracker.Tracked) string {
	switch {
	case tr.ConflictHeadSHA != "" && tr.ConflictHeadSHA == tr.HeadSHA:
		return "conflict"
	case tr.LastError != "":
		return "error"
	}
	return "ok"
}

// Service is the boundary every UI (tray, web, Wails, a future API) talks to. It
// owns the run loop and hands out copies, so callers never race a running tick.
type Service struct {
	cfg     config.Config
	tracker *tracker.Tracker
	rev     *review.Reviewer // nil when reviews are disabled
	logs    *LogBuffer
	runNow  chan struct{}

	mu      sync.Mutex
	tracked []tracker.Tracked // last tracker snapshot
	status  Status
	subs    []func(Status)
}

// NewService wires the tracker, the reviewer (nil = reviews disabled) and the log buffer.
func NewService(cfg config.Config, tr *tracker.Tracker, rev *review.Reviewer, logs *LogBuffer) *Service {
	s := &Service{cfg: cfg, tracker: tr, rev: rev, logs: logs, runNow: make(chan struct{}, 1)}
	s.status = Status{User: cfg.User, Org: cfg.Org, Interval: cfg.Interval.String(), Reviews: rev != nil}
	tr.OnSave = s.refreshPRs // tracker saves mid-tick, so UIs see progress
	if rev != nil {
		tr.Events = rev
		rev.Kick = s.RunNow
		rev.OnChange = s.publish
	}
	s.refreshPRs()
	return s
}

// RunEvents returns a review run's readable output so far, and the next line number.
func (s *Service) RunEvents(prID, runID string) ([]review.Event, int, error) {
	if s.rev == nil {
		return nil, 0, review.ErrNoRun
	}
	return s.rev.Events(prID, runID)
}

// SubscribeRunEvents streams every run's output events until cancel. Subscribe
// before reading RunEvents' backlog, and skip live events with Line below it.
func (s *Service) SubscribeRunEvents() (<-chan review.RunEvent, func()) {
	if s.rev == nil {
		ch := make(chan review.RunEvent)
		return ch, func() {}
	}
	return s.rev.SubscribeEvents()
}

// RunDir returns the directory holding one review run's run.json, log and out/.
func (s *Service) RunDir(prID, runID string) (string, bool) {
	if s.rev == nil {
		return "", false
	}
	return s.rev.RunDir(prID, runID)
}

// Status returns the latest snapshot. Its PRs slice is shared; don't mutate it.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// PR returns one tracked PR by ID ("owner/repo#n").
func (s *Service) PR(id string) (PRView, bool) {
	st := s.Status()
	i, ok := slices.BinarySearchFunc(st.PRs, id, func(p PRView, id string) int { return cmp.Compare(p.ID, id) })
	if !ok {
		return PRView{}, false
	}
	return st.PRs[i], true
}

// trackedPR is the last tracker snapshot of a PR, for the reviewer's per-PR calls.
func (s *Service) trackedPR(id string) (tracker.Tracked, error) {
	if s.rev == nil {
		return tracker.Tracked{}, errors.New("reviews are disabled")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := slices.IndexFunc(s.tracked, func(tr tracker.Tracked) bool { return tr.Key() == id }); i >= 0 {
		return s.tracked[i], nil
	}
	return tracker.Tracked{}, fmt.Errorf("%s is not tracked", id)
}

// ReviewNow queues a full review of a tracked PR, behind a run in progress.
func (s *Service) ReviewNow(id string) error {
	tr, err := s.trackedPR(id)
	if err != nil {
		return err
	}
	return s.rev.Trigger(tr)
}

// Stack is what's on Docker for a PR's local stack (nil = its repo has none).
func (s *Service) Stack(ctx context.Context, id string) (*review.StackStatus, error) {
	tr, err := s.trackedPR(id)
	if err != nil {
		return nil, err
	}
	return s.rev.StackStatus(ctx, tr)
}

// Wipe removes a PR's review data: a running review, its stack with volumes,
// review containers, history and run dirs. Tracking and the worktree stay.
func (s *Service) Wipe(ctx context.Context, id string) error {
	if _, err := s.trackedPR(id); err != nil {
		return err
	}
	return s.rev.Wipe(ctx, id)
}

// Logs returns the most recent ops log entries, oldest first.
func (s *Service) Logs() []LogEntry { return s.logs.Recent() }

// SubscribeLogs streams new log entries until cancel is called.
func (s *Service) SubscribeLogs() (<-chan LogEntry, func()) { return s.logs.Subscribe() }

// OnChange registers fn to run (on the tick goroutine) after every snapshot change.
func (s *Service) OnChange(fn func(Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subs = append(s.subs, fn)
}

// RunNow queues a tick; a no-op if one is already queued.
func (s *Service) RunNow() {
	slog.Info("run requested")
	select {
	case s.runNow <- struct{}{}:
	default:
	}
}

// RunQueued reports whether a RunNow is waiting for the loop to pick it up.
func (s *Service) RunQueued() bool { return len(s.runNow) > 0 }

// Run ticks immediately, then every interval or on RunNow, until ctx is done.
// Ticks never overlap. Reviews run alongside; leftover review containers are
// re-attached before the first tick so it won't sync a worktree still in use.
func (s *Service) Run(ctx context.Context) {
	if s.rev != nil {
		s.rev.Reattach(ctx)
		go s.rev.Run(ctx)
	}
	ticker := time.NewTicker(s.cfg.Interval.Duration)
	defer ticker.Stop()
	for {
		s.RunOnce(ctx)
		select {
		case <-ticker.C:
		case <-s.runNow:
		case <-ctx.Done():
			return
		}
	}
}

// RunOnce runs a single tick (for -once).
func (s *Service) RunOnce(ctx context.Context) {
	s.update(func(st *Status) { st.Running = true })
	slog.Info("run started")
	start := time.Now()
	err := s.tracker.Tick(ctx)
	if err != nil {
		slog.Error("run failed", "err", err)
	}
	s.update(func(st *Status) {
		st.Running, st.LastRun, st.LastError = false, time.Now(), ""
		if err != nil {
			st.LastError = err.Error()
		}
	})
	if s.rev != nil {
		s.rev.Ensure(s.tracker.Snapshot()) // a first review for PRs that never got one
		s.publish()
	}
	for _, pr := range s.Status().PRs { // what this sync left each PR at, and why
		slog.Info("PR", "pr", pr.ID, "head", pr.HeadSHA[:min(len(pr.HeadSHA), 8)], "sync", cmp.Or(pr.LastError, pr.Health),
			"review", pr.ReviewNote)
	}
	if err == nil {
		slog.Info("run finished", "took", time.Since(start).Round(time.Millisecond), "summary", s.Status().Summary())
	}
}

// refreshPRs copies tracker state. It must run on the goroutine that ticks.
func (s *Service) refreshPRs() {
	snap := s.tracker.Snapshot()
	s.mu.Lock()
	s.tracked = snap
	s.mu.Unlock()
	s.publish()
}

// publish merges the last tracker snapshot with live review state. Safe from any goroutine.
func (s *Service) publish() {
	var reviews map[string]review.PRStatus
	runtimeErr := ""
	if s.rev != nil {
		reviews, runtimeErr = s.rev.Status(), s.rev.RuntimeError()
	}
	s.mu.Lock()
	tracked := s.tracked
	s.mu.Unlock()
	prs := make([]PRView, 0, len(tracked))
	blocked := map[string]string{}
	for _, tr := range tracked {
		if _, ok := blocked[tr.Repo]; !ok && s.rev != nil {
			blocked[tr.Repo] = s.rev.Blocked(tr.Repo)
		}
		rs := reviews[tr.Key()]
		prs = append(prs, PRView{Tracked: tr, ID: tr.Key(), Health: health(tr), Review: rs,
			ReviewNote: reviewNote(tr, rs, s.rev != nil, blocked[tr.Repo], runtimeErr)})
	}
	var active []ActiveReview
	for id, ps := range reviews {
		for _, r := range ps.Runs {
			if r.Status == review.Running {
				active = append(active, ActiveReview{PR: id, Run: r.ID, Reason: r.Job.Reason, Container: r.Container,
					Name: r.Name, Started: r.Started, Overdue: r.Overdue})
			}
		}
	}
	slices.SortFunc(active, func(a, b ActiveReview) int { return a.Started.Compare(b.Started) })
	s.update(func(st *Status) { st.PRs, st.RuntimeError, st.Reviewing = prs, runtimeErr, active })
}

func (s *Service) update(fn func(*Status)) {
	s.mu.Lock()
	fn(&s.status)
	st, subs := s.status, slices.Clone(s.subs)
	s.mu.Unlock()
	for _, sub := range subs {
		sub(st)
	}
}

// Settings is the config file's editable fields, for the settings pages.
type Settings struct {
	Path   string         `json:"path"`
	Fields []config.Field `json:"fields"`
}

// Settings reads the config file now, so it shows saved changes not yet applied.
func (s *Service) Settings() (Settings, error) {
	fs, err := config.Settings(s.cfg.Path)
	return Settings{Path: s.cfg.Path, Fields: fs}, err
}

// SaveSettings writes changed fields (key → value) to the config file; they
// apply on restart.
func (s *Service) SaveSettings(values map[string]any) error {
	if err := config.SaveSettings(s.cfg.Path, values); err != nil {
		return err
	}
	slog.Info("settings saved, restart to apply", "file", s.cfg.Path, "changed", slices.Sorted(maps.Keys(values)))
	return nil
}
