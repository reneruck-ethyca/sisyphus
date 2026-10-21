package app

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/reneruck/waterboy/internal/config"
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
}

func (s Status) Summary() string {
	return fmt.Sprintf("%d tracked", len(s.PRs))
}

// PRView is a tracked PR as UIs see it: a copy of its state plus derived fields.
type PRView struct {
	tracker.Tracked
	ID     string `json:"id"`     // "owner/repo#n"
	Health string `json:"health"` // "conflict" | "error" | "ok"
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
	logs    *LogBuffer
	runNow  chan struct{}

	mu     sync.Mutex
	status Status
	subs   []func(Status)
}

func NewService(cfg config.Config, tr *tracker.Tracker, logs *LogBuffer) *Service {
	s := &Service{cfg: cfg, tracker: tr, logs: logs, runNow: make(chan struct{}, 1)}
	s.status = Status{User: cfg.User, Org: cfg.Org, Interval: cfg.Interval.String()}
	tr.OnSave = s.refreshPRs // tracker saves mid-tick, so UIs see progress
	s.refreshPRs()
	return s
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
	select {
	case s.runNow <- struct{}{}:
	default:
	}
}

// RunQueued reports whether a RunNow is waiting for the loop to pick it up.
func (s *Service) RunQueued() bool { return len(s.runNow) > 0 }

// Run ticks immediately, then every interval or on RunNow, until ctx is done.
// Ticks never overlap.
func (s *Service) Run(ctx context.Context) {
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
	err := s.tracker.Tick(ctx)
	if err != nil {
		slog.Error("tick failed", "err", err)
	}
	s.update(func(st *Status) {
		st.Running, st.LastRun, st.LastError = false, time.Now(), ""
		if err != nil {
			st.LastError = err.Error()
		}
	})
}

// refreshPRs copies tracker state. It must run on the goroutine that ticks.
func (s *Service) refreshPRs() {
	var prs []PRView
	for _, tr := range s.tracker.Snapshot() {
		prs = append(prs, PRView{Tracked: tr, ID: tr.Key(), Health: health(tr)})
	}
	s.update(func(st *Status) { st.PRs = prs })
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
