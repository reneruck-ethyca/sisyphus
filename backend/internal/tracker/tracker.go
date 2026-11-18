package tracker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// Tracker keeps the tracked PRs. Its exported methods lock, so any goroutine
// may call them; a Tick holds the lock for its whole pass.
type Tracker struct {
	cfg     config.Config
	gh      github.Client
	skipped map[string]bool // log-once for forks / missing clones
	teams   []string        // my teams in the org, refreshed every tick
	// OnSave gets a copy of the state after every save (mid-tick too, so UIs see
	// progress). It runs under the lock: don't call back into the tracker.
	OnSave func(prs []Tracked, ignored []Ignored)
	Events Events // defaults to a no-op; called under the lock, never blocks
	Locks  *Locks // shared with whatever runs in worktrees

	mu    sync.Mutex
	state *State

	history atomic.Pointer[[]Dropped] // state.History as of the last save: read without waiting out a tick
}

func New(cfg config.Config, gh github.Client, st *State) *Tracker {
	t := &Tracker{cfg: cfg, gh: gh, state: st, skipped: map[string]bool{}, Events: nopEvents{}, Locks: &Locks{}}
	t.cacheHistory()
	return t
}

func (t *Tracker) cacheHistory() {
	h := slices.Collect(maps.Values(t.state.History))
	t.history.Store(&h)
}

// Snapshot copies the tracked PRs, sorted by ID.
func (t *Tracker) Snapshot() []Tracked {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot()
}

func (t *Tracker) snapshot() []Tracked {
	out := make([]Tracked, 0, len(t.state.PRs))
	for _, id := range slices.Sorted(maps.Keys(t.state.PRs)) {
		out = append(out, *t.state.PRs[id])
	}
	return out
}

func (t *Tracker) save() {
	if err := t.state.Save(t.cfg.StateFile); err != nil {
		slog.Error("saving state", "err", err)
	}
	t.cacheHistory()
	if t.OnSave != nil {
		t.OnSave(t.snapshot(), t.ignored())
	}
}

func (t *Tracker) repoDir(repo string) string { return filepath.Join(t.cfg.ReposRoot, path.Base(repo)) }

func (t *Tracker) skipOnce(key, why string) {
	if !t.skipped[key+why] {
		t.skipped[key+why] = true
		slog.Info("skipping PR", "pr", key, "reason", why)
	}
}

// Tick runs one discover → update → cleanup pass. It only returns an error when
// the candidate search fails, in which case nothing is touched.
func (t *Tracker) Tick(ctx context.Context) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cfg.User == "" { // the search would match every open PR in the org
		return errors.New("no GitHub user configured")
	}
	refs, err := t.gh.Candidates(ctx, t.cfg.Org, t.cfg.User)
	if err != nil {
		return fmt.Errorf("searching candidates: %w", err)
	}
	// Without my teams a team-requested PR can't be judged, so touch nothing.
	if t.teams, err = t.gh.Teams(ctx, t.cfg.Org); err != nil {
		return fmt.Errorf("listing your teams: %w", err)
	}
	added := map[string]bool{}
	for _, ref := range refs {
		if _, ignored := t.state.Ignored[ref.Key()]; ignored {
			continue
		}
		if t.state.PRs[ref.Key()] == nil && t.discover(ctx, ref, false) {
			added[ref.Key()] = true
		}
	}
	for key, tr := range t.state.PRs {
		if !added[key] {
			t.update(ctx, tr)
		}
	}
	return nil
}

// discover starts tracking ref. byHand tracks it whether or not it asks for my review.
func (t *Tracker) discover(ctx context.Context, ref github.PRRef, byHand bool) bool {
	key := ref.Key()
	pr, err := t.gh.PR(ctx, ref)
	if err != nil {
		slog.Warn("fetching PR", "pr", key, "err", err)
		return false
	}
	// Picked up only on a request; reviewedByMe keeps PRs already tracked, not new ones.
	if !byHand && !requested(pr, t.cfg.User, t.teams) || dropReason(pr, t.cfg.User, true, t.cfg.IncludeDrafts) != "" {
		return false
	}
	if pr.HeadRepo != pr.Repo {
		t.skipOnce(key, "fork")
		return false
	}
	if !worktree.ValidBranch(ctx, pr.HeadRef) {
		t.skipOnce(key, "invalid branch name "+strconv.Quote(pr.HeadRef))
		return false
	}
	repoDir := t.repoDir(pr.Repo)
	if _, err := os.Stat(repoDir); err != nil {
		t.skipOnce(key, "no clone at "+repoDir)
		return false
	}
	wt, err := worktree.Find(ctx, repoDir, pr.HeadRef)
	if err != nil {
		slog.Warn("listing worktrees", "pr", key, "err", err)
		return false
	}
	createdWT, createdBranch := false, false
	if wt == "" {
		createdBranch = !worktree.BranchExists(ctx, repoDir, pr.HeadRef)
		if wt, err = worktree.Create(ctx, repoDir, pr.HeadRef); err != nil {
			slog.Warn("creating worktree", "pr", key, "err", err)
			return false
		}
		createdWT = true
	}
	synced := "" // reused worktrees / pre-existing branches are never hard-reset
	if createdBranch {
		synced, _ = worktree.HeadSHA(ctx, wt)
	}
	_, seen := newReplies(pr, t.cfg.User, time.Time{})
	tr := &Tracked{
		PRRef: pr.PRRef, URL: pr.URL, Title: pr.Title, Author: pr.Author, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA,
		SyncedSHA: synced, Worktree: wt, CreatedWorktree: createdWT, CreatedBranch: createdBranch,
		TrackedAt: time.Now(), LastSeenAt: seen, ByHand: byHand,
	}
	t.state.PRs[key] = tr
	t.save() // before anything else can fail: a created worktree must never be forgotten
	slog.Info("tracking PR", "pr", key, "title", tr.Title, "worktree", tr.Worktree, "created", tr.CreatedWorktree)
	if tr.SyncedSHA != pr.HeadSHA {
		t.sync(ctx, tr)
	}
	t.classify(ctx, tr)
	t.readDesign(tr, pr)
	t.announce(tr)
	t.save()
	return true
}

func (t *Tracker) update(ctx context.Context, tr *Tracked) {
	key := tr.Key()
	defer t.save()
	if _, err := os.Stat(tr.Worktree); errors.Is(err, os.ErrNotExist) {
		if unlock, ok := t.lock(tr); ok {
			defer unlock()
			t.forgetMissing(ctx, tr)
		}
		return
	}
	pr, err := t.gh.PR(ctx, tr.PRRef)
	if err != nil {
		tr.LastError = err.Error() // never drop on a fetch error
		slog.Warn("fetching PR", "pr", key, "err", err)
		return
	}
	if reason := dropReason(pr, t.cfg.User, tr.ByHand || requested(pr, t.cfg.User, t.teams), t.cfg.IncludeDrafts); reason != "" {
		unlock, ok := t.lock(tr)
		if !ok {
			return // retried next tick, once whatever holds the worktree has let go
		}
		defer unlock()
		if tr.CreatedWorktree {
			if err := worktree.Remove(ctx, t.repoDir(tr.Repo), tr.HeadRef, tr.CreatedBranch); err != nil {
				tr.LastError = err.Error()
				slog.Warn("removing worktree, will retry", "pr", key, "err", err)
				return
			}
		}
		t.archive(*tr, reason)
		delete(t.state.PRs, key)
		slog.Info("dropped PR", "pr", key, "reason", reason, "removed_worktree", tr.CreatedWorktree)
		t.Events.Dropped(*tr, reason)
		return
	}
	tr.LastError = ""
	newCommits := pr.HeadSHA != tr.HeadSHA
	n, latest := newReplies(pr, t.cfg.User, tr.LastSeenAt)
	if latestStates(pr.Reviews)[t.cfg.User] == "APPROVED" {
		// Once I've approved, only new commits wake us; replies are seen but not acted on.
		n, tr.UnannouncedReplies = 0, 0
		tr.LastSeenAt = latest
	}
	if newCommits || n > 0 {
		tr.HeadSHA, tr.LastSeenAt = pr.HeadSHA, latest
		tr.UnannouncedReplies += n
		slog.Info("PR updated", "pr", key, "new_commits", newCommits, "new_replies", n)
	}
	if tr.SyncedSHA != tr.HeadSHA {
		unlock, ok := t.lock(tr)
		if !ok {
			return
		}
		t.sync(ctx, tr)
		unlock()
	}
	t.classify(ctx, tr)
	t.readDesign(tr, pr)
	t.announce(tr)
}

// readDesign records where the user's review of a design PR stands.
func (t *Tracker) readDesign(tr *Tracked, pr github.PR) {
	tr.Design = nil
	if tr.DesignReview {
		d := designState(pr, t.cfg.User)
		tr.Design = &d
	}
}

// classify decides, once per head, whether tr is a design review. Until it
// has, announce holds the PR back, so a design review is never auto-reviewed
// for want of its file list.
func (t *Tracker) classify(ctx context.Context, tr *Tracked) {
	if tr.ClassifiedSHA == tr.HeadSHA {
		return
	}
	files, err := t.gh.Files(ctx, tr.PRRef)
	if err != nil {
		tr.LastError = err.Error()
		slog.Warn("listing PR files", "pr", tr.Key(), "err", err)
		return
	}
	if d := designReview(tr.Title, files); d != tr.DesignReview || tr.ClassifiedSHA == "" {
		slog.Info("classified PR", "pr", tr.Key(), "design_review", d)
		tr.DesignReview = d
	}
	tr.ClassifiedSHA = tr.HeadSHA
}

// Refresh re-reads one PR from GitHub and syncs its worktree to a new head, as a
// tick would, and returns what it's tracked as now.
func (t *Tracker) Refresh(ctx context.Context, id string) (Tracked, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.state.PRs[id]
	if tr == nil {
		return Tracked{}, fmt.Errorf("%s is not tracked", id)
	}
	t.update(ctx, tr)
	if tr = t.state.PRs[id]; tr == nil {
		return Tracked{}, fmt.Errorf("%s is no longer tracked", id)
	}
	return *tr, nil
}

// SetAgent changes a tracked PR's review agent. nil makes it inherit the
// application-wide review.agent profile again.
func (t *Tracker) SetAgent(id string, agent *config.Agent) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.state.PRs[id]
	if tr == nil {
		return fmt.Errorf("%s is not tracked", id)
	}
	if agent != nil {
		if err := agent.Check(); err != nil {
			return err
		}
		copy := *agent
		tr.Agent = &copy
	} else {
		tr.Agent = nil
	}
	t.save()
	return nil
}

// lock takes tr's worktree lock, or asks its holder to let go (we retry next tick).
func (t *Tracker) lock(tr *Tracked) (unlock func(), ok bool) {
	if unlock, ok = t.Locks.TryLock(tr.Key()); !ok {
		slog.Info("worktree busy, asking for it back", "pr", tr.Key())
		t.Events.Reclaim(*tr)
	}
	return unlock, ok
}

// announce tells Events about a PR once its worktree is at the head: first
// time, new commits, or replies that arrived since the last announcement.
func (t *Tracker) announce(tr *Tracked) {
	switch {
	case tr.SyncedSHA != tr.HeadSHA, tr.ClassifiedSHA != tr.HeadSHA:
		return // not ready; a conflict, busy worktree or failed file listing defers this to a later tick
	case tr.AnnouncedSHA == "":
		t.Events.Tracked(*tr)
	case tr.AnnouncedSHA != tr.HeadSHA || tr.UnannouncedReplies > 0:
		t.Events.Updated(*tr, tr.AnnouncedSHA != tr.HeadSHA, tr.UnannouncedReplies)
	default:
		return
	}
	tr.AnnouncedSHA, tr.UnannouncedReplies = tr.HeadSHA, 0
}

// Forget stops tracking id, removing its worktree if sisyphus created it, so the
// next tick discovers it from scratch. It waits (until ctx ends) for whatever
// holds the worktree.
func (t *Tracker) Forget(ctx context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.forget(ctx, id, "reset")
}

// forget drops id, archiving it with reason.
func (t *Tracker) forget(ctx context.Context, id, reason string) error {
	tr := t.state.PRs[id]
	if tr == nil {
		return nil
	}
	// Stop its queued and running reviews first: waiting out a review here would
	// hold t.mu for its whole run, and let it post for a PR being forgotten.
	t.Events.Dropped(*tr, reason)
	unlock, err := t.Locks.Lock(ctx, id)
	if err != nil {
		return fmt.Errorf("waiting for %s's worktree: %w", id, err)
	}
	defer unlock()
	if _, err := os.Stat(tr.Worktree); errors.Is(err, os.ErrNotExist) {
		t.pruneMissing(ctx, tr) // already gone: nothing for wtp to remove
	} else if tr.CreatedWorktree {
		if err := worktree.Remove(ctx, t.repoDir(tr.Repo), tr.HeadRef, tr.CreatedBranch); err != nil {
			return err
		}
	}
	t.archive(*tr, reason)
	delete(t.state.PRs, id)
	t.save()
	slog.Info("forgot PR", "pr", id, "removed_worktree", tr.CreatedWorktree)
	return nil
}

// Untrack forgets a PR (removing the worktree and branch it created) and ignores
// it from then on, so it isn't picked up again while it still requests a review.
func (t *Tracker) Untrack(ctx context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	tr := t.state.PRs[id]
	if tr == nil {
		return fmt.Errorf("%s is not tracked", id)
	}
	ig := Ignored{PRRef: tr.PRRef, Title: tr.Title, At: time.Now()}
	if err := t.forget(ctx, id, "untracked by hand"); err != nil {
		return err
	}
	if t.state.Ignored == nil {
		t.state.Ignored = map[string]Ignored{}
	}
	t.state.Ignored[id] = ig
	t.save()
	slog.Info("untracked PR", "pr", id)
	return nil
}

// Retrack stops ignoring an untracked PR; the next tick picks it up if it still
// requests a review. Any other PR it tracks by hand, now: one that doesn't ask for
// my review (any more), kept until merged or closed.
func (t *Tracker) Retrack(ctx context.Context, id string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.state.Ignored[id]; ok {
		delete(t.state.Ignored, id)
		t.save()
		slog.Info("tracking PR again", "pr", id)
		return nil
	}
	if t.state.PRs[id] != nil {
		return fmt.Errorf("%s is already tracked", id)
	}
	ref, err := github.ParseKey(id)
	if err != nil {
		return err
	}
	if !t.discover(ctx, ref, true) {
		return fmt.Errorf("%s can't be tracked: merged, closed, a fork, or no clone of its repo (see the log)", id)
	}
	return nil
}

// IgnoredPRs copies the untracked PRs, sorted by ID.
func (t *Tracker) IgnoredPRs() []Ignored {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ignored()
}

func (t *Tracker) ignored() []Ignored {
	out := make([]Ignored, 0, len(t.state.Ignored))
	for _, id := range slices.Sorted(maps.Keys(t.state.Ignored)) {
		out = append(out, t.state.Ignored[id])
	}
	return out
}

// archive records tr in the history as dropped now, replacing an earlier drop of it.
func (t *Tracker) archive(tr Tracked, reason string) {
	if t.state.History == nil {
		t.state.History = map[string]Dropped{}
	}
	t.state.History[tr.Key()] = tr.AsDropped(reason, time.Now())
}

// History copies the PRs no longer tracked, in no particular order, as of the
// last save. It doesn't take the lock, so the History page needn't wait for a tick.
func (t *Tracker) History() []Dropped {
	return slices.Clone(*t.history.Load())
}

// ClearHistory forgets every dropped PR.
func (t *Tracker) ClearHistory() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.History = nil
	t.save()
}

// forgetMissing handles a worktree deleted outside sisyphus: drop the entry (the
// next tick re-tracks the PR if it's still eligible) and prune what's left.
func (t *Tracker) forgetMissing(ctx context.Context, tr *Tracked) {
	t.pruneMissing(ctx, tr)
	t.archive(*tr, "worktree deleted outside sisyphus")
	delete(t.state.PRs, tr.Key())
	slog.Info("worktree deleted outside sisyphus, forgot PR", "pr", tr.Key(), "worktree", tr.Worktree)
	t.Events.Dropped(*tr, "worktree deleted outside sisyphus")
}

// pruneMissing clears git's record of tr's deleted worktree and deletes our
// branch, but only if it holds nothing unpushed (`git branch -d` refuses otherwise).
func (t *Tracker) pruneMissing(ctx context.Context, tr *Tracked) {
	repoDir := t.repoDir(tr.Repo)
	if _, err := proc.Run(ctx, repoDir, "git", "worktree", "prune"); err != nil {
		slog.Warn("pruning worktrees", "pr", tr.Key(), "err", err)
	}
	if tr.CreatedBranch && worktree.BranchExists(ctx, repoDir, tr.HeadRef) {
		if _, err := proc.Run(ctx, repoDir, "git", "branch", "-d", "--", tr.HeadRef); err != nil {
			slog.Warn("keeping branch of deleted worktree", "pr", tr.Key(), "branch", tr.HeadRef, "err", err)
		}
	}
}

func (t *Tracker) sync(ctx context.Context, tr *Tracked) {
	var onConflict func(string) error
	// Hooks run at most maxHookRuns times per PR head; a persistent conflict
	// waits for a new push instead of re-running the hook every tick.
	if hook := t.cfg.Hooks["on_conflict"]; hook != "" && tr.ConflictHeadSHA != tr.HeadSHA {
		onConflict = func(reason string) error {
			return RunHook(ctx, hook, t.cfg.HookTimeout.Duration, tr.Worktree, map[string]string{
				"SISYPHUS_HOOK": "on_conflict", "SISYPHUS_PR_URL": tr.URL, "SISYPHUS_REPO": tr.Repo,
				"SISYPHUS_PR_NUMBER": strconv.Itoa(tr.Number), "SISYPHUS_BRANCH": tr.HeadRef,
				"SISYPHUS_WORKTREE": tr.Worktree, "SISYPHUS_CONFLICT_REASON": reason,
			})
		}
	}
	err := syncWithRetry(func() error {
		sha, err := worktree.Sync(ctx, tr.Worktree, tr.HeadRef, tr.SyncedSHA)
		if err == nil {
			tr.SyncedSHA = sha
		}
		return err
	}, onConflict, maxHookRuns)
	var ce *worktree.ConflictError
	if errors.As(err, &ce) {
		tr.ConflictHeadSHA = tr.HeadSHA
	} else if err == nil {
		tr.ConflictHeadSHA = ""
	}
	if err != nil {
		tr.LastError = err.Error()
		slog.Warn("syncing worktree", "pr", tr.Key(), "worktree", tr.Worktree, "err", err)
	}
}
