package app

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// Status is a point-in-time snapshot of sisyphus for UIs and APIs.
type Status struct {
	User      string    `json:"user"`
	Org       string    `json:"org"`
	Interval  string    `json:"interval"`
	Running   bool      `json:"running"`
	LastRun   time.Time `json:"last_run"`
	NextRun   time.Time `json:"next_run"`             // ponytail: LastRun + interval; off by up to an interval after a "Check now"
	LastError string    `json:"last_error,omitempty"` // of the last tick as a whole
	PRs       []PRView  `json:"prs"`                  // sorted by ID
	Reviews   bool      `json:"reviews"`              // false when review.disabled
	// Why reviews are waiting on the container runtime ("" = they aren't).
	RuntimeError string            `json:"runtime_error,omitempty"`
	Budget       review.Budget     `json:"budget"`
	BudgetNote   string            `json:"budget_note,omitempty"` // the 5-hour budget's use, in a line ("" = no budget)
	BudgetWait   string            `json:"budget_wait,omitempty"` // why the budget holds reviews back ("" = it doesn't)
	Reviewing    []ActiveReview    `json:"reviewing"`             // review runs in progress, oldest first
	Ignored      []tracker.Ignored `json:"ignored"`               // PRs untracked by hand
	Activity     []Activity        `json:"activity"`              // latest review runs across PRs, newest first
	Handoffs     []lastmile.View   `json:"handoffs"`              // the last mile's handoffs, newest first
	SlotsUsed    int               `json:"slots_used"`            // run slots taken by reviews and last-mile agents
	SlotsMax     int               `json:"slots_max"`             // review.max_concurrent; 0 when reviews are disabled
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
	for _, state := range []string{review.StateRunning, review.StateQueued} {
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
	Health     string          `json:"health"` // Health* (tracking, not review)
	Review     review.PRStatus `json:"review"`
	ReviewNote string          `json:"review_note"` // why a review is or isn't happening, in a line
	UpToDate   bool            `json:"up_to_date"`  // the latest review covered the head; nothing pending
	// Where the PR stands for its reader: see verdict.
	Verdict        string       `json:"verdict"`
	Tone           string       `json:"tone"`
	Group          string       `json:"group"`
	VerdictAt      time.Time    `json:"verdict_at"`
	Agent          config.Agent `json:"agent"` // effective review agent
	OverridesAgent bool         `json:"overrides_agent"`
}

// budgetNotes says in a line how much of the 5-hour budget reviews used, and
// why it holds them back once used up ("" = no budget, or not held back).
func budgetNotes(b review.Budget) (note, wait string) {
	if b.Share == 0 {
		return "", ""
	}
	pct := func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) }
	if b.ResetsAt.IsZero() {
		return "Reviews may use " + pct(b.Share) + " of each 5-hour window; none used in this one yet.", ""
	}
	resets := b.ResetsAt.Local().Format("15:04")
	note = fmt.Sprintf("5-hour window: reviews used %s of their %s · account at %s · resets %s", pct(b.Used), pct(b.Share), pct(b.Account), resets)
	if b.Exhausted() {
		wait = fmt.Sprintf("Reviews used their %s share of the 5-hour window. Queued reviews start after it resets at %s.", pct(b.Share), resets)
	}
	return note, wait
}

// reviewNote says in a line where the PR's review stands and why. blocked and
// runtimeErr are what keeps queued reviews from starting ("" = nothing).
func reviewNote(tr tracker.Tracked, rs review.PRStatus, reviews bool, blocked, runtimeErr string) string {
	if !reviews {
		return "reviews are disabled"
	}
	short := func(sha string) string { return sha[:min(len(sha), 8)] }
	switch rs.State {
	case review.StateRunning:
		if len(rs.Runs) == 0 { // a stopped run whose history was just reset, still letting go
			return "stopping a review"
		}
		return "reviewing (" + rs.Runs[0].Job.Reason + ")"
	case review.StateQueued:
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
	if d := tr.Design; d != nil {
		return designNote(*d)
	}
	last := rs.Latest()
	next := "next one on new commits or replies"
	if tr.DesignReview {
		next = "design review: next one when you start it"
	}
	switch {
	case last == nil && blocked != "":
		return "not reviewed: " + blocked
	case tr.DesignReview && last == nil:
		return "design review: not reviewed until you start one"
	case last == nil:
		return "not reviewed yet: waits for the head to be synced"
	case last.Status != review.Succeeded:
		return fmt.Sprintf("last review %s: %s; %s", last.Status, cmp.Or(last.Error, "no details"), next)
	case last.Job.PR.HeadSHA != tr.HeadSHA && tr.DesignReview:
		return fmt.Sprintf("reviewed %s, head is now %s; %s", short(last.Job.PR.HeadSHA), short(tr.HeadSHA), next)
	case last.Job.PR.HeadSHA != tr.HeadSHA:
		return fmt.Sprintf("reviewed %s; head %s not synced yet", short(last.Job.PR.HeadSHA), short(tr.HeadSHA))
	}
	return fmt.Sprintf("up to date: reviewed %s at %s, %s; %s",
		short(last.Job.PR.HeadSHA), last.Finished.Local().Format("Jan 2 15:04"), cmp.Or(last.PostNote, "not posted"), next)
}

// designNote says where the user's review of a design PR stands.
func designNote(d tracker.DesignState) string {
	if d.ReviewedAt.IsZero() {
		return "design review: not reviewed by you yet"
	}
	var news []string
	if d.Replies > 0 {
		news = append(news, plural(d.Replies, "comment"))
	}
	if d.NewCommits {
		news = append(news, "new commits")
	}
	when := d.ReviewedAt.Local().Format("Jan 2")
	if len(news) == 0 || !d.Updated() {
		return "design review: waiting on the author since your review on " + when
	}
	return strings.Join(news, " and ") + " since your review on " + when
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func health(tr tracker.Tracked) string {
	switch {
	case tr.ConflictHeadSHA != "" && tr.ConflictHeadSHA == tr.HeadSHA:
		return HealthConflict
	case tr.LastError != "":
		return HealthError
	}
	return HealthOK
}

// Service is the boundary every UI (tray, web, Wails, a future API) talks to. It
// owns the run loop and hands out copies; the tracker locks, so PR actions and
// a running tick serialise there.
type Service struct {
	lastMile *lastmile.Manager // nil when lastmile.disabled

	cfg     config.Config
	tracker *tracker.Tracker
	rev     *review.Reviewer // nil when reviews are disabled
	runNow  chan struct{}

	mu      sync.Mutex
	tracked []tracker.Tracked // last tracker snapshot
	status  Status
	wakers  map[chan struct{}]struct{} // SubscribeStatus: poked on every change
}

// NewService wires the tracker and the reviewer (nil = reviews disabled).
func NewService(cfg config.Config, tr *tracker.Tracker, rev *review.Reviewer) *Service {
	s := &Service{cfg: cfg, tracker: tr, rev: rev, runNow: make(chan struct{}, 1)}
	s.status = Status{User: cfg.User, Org: cfg.Org, Interval: cfg.Interval.String(), Reviews: rev != nil}
	tr.OnSave = s.refreshPRs
	if rev != nil {
		tr.Events = rev
		rev.Kick = s.RunNow
		rev.Refresh = s.tracker.Refresh
		rev.OnChange = s.publish
	}
	s.refreshPRs(tr.Snapshot(), tr.IgnoredPRs())
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

// ReviewNow queues a full review of a tracked PR, behind a run in progress, at
// its head on GitHub now rather than as of the last tick.
func (s *Service) ReviewNow(id string) error {
	if _, err := s.trackedPR(id); err != nil {
		return err
	}
	return s.do(context.Background(), func(ctx context.Context) error {
		tr, err := s.tracker.Refresh(ctx, id)
		if err != nil {
			return err
		}
		return s.rev.Trigger(tr)
	})
}

// ResetReviews forgets a PR's reviews (history, queued jobs, a running review)
// and queues a fresh one; its stack stays.
func (s *Service) ResetReviews(ctx context.Context, id string) error {
	return s.do(ctx, func(ctx context.Context) error {
		if _, err := s.trackedPR(id); err != nil {
			return err
		}
		err := s.rev.ResetReviews(ctx, id)
		s.rev.Ensure(s.tracker.Snapshot())
		return err
	})
}

// Wipe is ResetReviews plus the PR's stack with its volumes and images; the
// next review starts on a fresh stack. Tracking and the worktree stay.
func (s *Service) Wipe(ctx context.Context, id string) error {
	return s.do(ctx, func(ctx context.Context) error {
		tr, err := s.trackedPR(id)
		if err != nil {
			return err
		}
		err = s.rev.Wipe(ctx, tr)
		s.rev.Ensure(s.tracker.Snapshot())
		return err
	})
}

// Untrack is Wipe plus forgetting the PR (its worktree and branch, if sisyphus
// created them); it isn't picked up again until Retrack.
func (s *Service) Untrack(ctx context.Context, id string) error {
	return s.do(ctx, func(ctx context.Context) error {
		if tr, err := s.trackedPR(id); err == nil {
			if err := s.rev.Wipe(ctx, tr); err != nil {
				return fmt.Errorf("%s stays tracked: %w", id, err)
			}
		}
		return s.tracker.Untrack(ctx, id) // also without reviews; errors for an untracked id
	})
}

// Retrack stops ignoring an untracked PR and syncs, picking it up if it still requests a review.
func (s *Service) Retrack(ctx context.Context, id string) error {
	err := s.do(ctx, func(context.Context) error { return s.tracker.Retrack(id) })
	if err == nil {
		s.RunNow()
	}
	return err
}

// SubscribeStatus returns a channel that gets a (coalesced) signal after every
// change; read Status for the snapshot. cancel stops it.
func (s *Service) SubscribeStatus() (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	s.mu.Lock()
	if s.wakers == nil {
		s.wakers = map[chan struct{}]struct{}{}
	}
	s.wakers[ch] = struct{}{}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		delete(s.wakers, ch)
	}
}

// RunNow queues a tick; a no-op if one is already queued.
func (s *Service) RunNow() {
	slog.Info("run requested")
	select {
	case s.runNow <- struct{}{}:
	default:
	}
}

// Run ticks immediately, then every interval or on RunNow, until ctx is done.
// Ticks never overlap. Reviews run alongside; leftover review containers are
// re-attached before the first tick so it won't sync a worktree still in use.
func (s *Service) Run(ctx context.Context) {
	if s.lastMile != nil {
		go s.lastMile.Run(ctx)
	}
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

// do runs a PR action to completion: the caller's ctx (a request's, say) going
// away mid-way must not leave a half-removed worktree or stack behind.
func (s *Service) do(ctx context.Context, fn func(context.Context) error) error {
	return fn(context.WithoutCancel(ctx))
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
		st.NextRun = st.LastRun.Add(s.cfg.Interval.Duration)
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

// refreshPRs takes a tracker snapshot (the tracker's OnSave). It must not call
// back into the tracker: it runs under its lock.
func (s *Service) refreshPRs(snap []tracker.Tracked, ignored []tracker.Ignored) {
	s.mu.Lock()
	s.tracked = snap
	s.mu.Unlock()
	s.update(func(st *Status) { st.Ignored = ignored })
	s.publish()
}

// publish merges the last tracker snapshot with live review state. Safe from any goroutine.
func (s *Service) publish() {
	var reviews map[string]review.PRStatus
	var budget review.Budget
	runtimeErr := ""
	slotsUsed, slotsMax := 0, 0
	if s.rev != nil {
		reviews, runtimeErr, budget = s.rev.Status(), s.rev.RuntimeError(), s.rev.Budget()
		slotsUsed, slotsMax = s.rev.Slots()
	}
	note, wait := budgetNotes(budget)
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
		why := blocked[tr.Repo]
		if why == "" && rs.State == review.StateQueued && wait != "" {
			why = "the 5-hour budget, until " + budget.ResetsAt.Local().Format("15:04")
		}
		agent := s.cfg.Review.Agent
		if tr.Agent != nil {
			agent = *tr.Agent
		}
		v := PRView{Tracked: tr, ID: tr.Key(), Health: health(tr), Review: rs, UpToDate: upToDate(tr, rs), Agent: agent, OverridesAgent: tr.Agent != nil,
			ReviewNote: reviewNote(tr, rs, s.rev != nil, why, runtimeErr)}
		v.Verdict, v.Tone, v.Group, v.VerdictAt = verdict(tr, v.Health, rs)
		prs = append(prs, v)
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
	handoffs := []lastmile.View{}
	if s.lastMile != nil {
		handoffs = s.lastMile.Views()
	}
	s.update(func(st *Status) {
		st.Handoffs = handoffs
		st.PRs, st.RuntimeError, st.Reviewing, st.Activity = prs, runtimeErr, active, activity(prs)
		st.Budget, st.BudgetNote, st.BudgetWait = budget, note, wait
		st.SlotsUsed, st.SlotsMax = slotsUsed, slotsMax
	})
}

func (s *Service) update(fn func(*Status)) {
	s.mu.Lock()
	fn(&s.status)
	for ch := range s.wakers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	s.mu.Unlock()
}

// UseLastMile sets the manager that takes handoffs and refreshes the dashboard
// when they change. Call it before Run.
func (s *Service) UseLastMile(m *lastmile.Manager) {
	s.lastMile = m
	m.OnChange = s.publish
	s.publish()
}

// LastMile is the manager that takes handoffs; nil when lastmile.disabled.
func (s *Service) LastMile() *lastmile.Manager { return s.lastMile }
