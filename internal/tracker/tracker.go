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
	"time"

	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/proc"
	"github.com/reneruck/waterboy/internal/worktree"
)

type Tracker struct {
	cfg     config.Config
	gh      github.Client
	state   *State
	skipped map[string]bool // log-once for forks / missing clones
	teams   []string        // my teams in the org, refreshed every tick
	OnSave  func()          // called after every state save, on the ticking goroutine
	Events  Events          // defaults to a no-op
	Locks   *Locks          // shared with whatever runs in worktrees
}

func New(cfg config.Config, gh github.Client, st *State) *Tracker {
	return &Tracker{cfg: cfg, gh: gh, state: st, skipped: map[string]bool{}, Events: nopEvents{}, Locks: &Locks{}}
}

// Snapshot copies the tracked PRs, sorted by ID. Call it on the ticking goroutine
// (e.g. from OnSave): Tick mutates state without locking.
func (t *Tracker) Snapshot() []Tracked {
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
	if t.OnSave != nil {
		t.OnSave()
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
		if t.state.PRs[ref.Key()] == nil && t.discover(ctx, ref) {
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

func (t *Tracker) discover(ctx context.Context, ref github.PRRef) bool {
	key := ref.Key()
	pr, err := t.gh.PR(ctx, ref)
	if err != nil {
		slog.Warn("fetching PR", "pr", key, "err", err)
		return false
	}
	if dropReason(pr, t.cfg.User, requested(pr, t.cfg.User, t.teams), t.cfg.IncludeDrafts) != "" {
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
		PRRef: pr.PRRef, URL: pr.URL, Title: pr.Title, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA,
		SyncedSHA: synced, Worktree: wt, CreatedWorktree: createdWT, CreatedBranch: createdBranch,
		TrackedAt: time.Now(), LastSeenAt: seen,
	}
	t.state.PRs[key] = tr
	t.save() // before anything else can fail: a created worktree must never be forgotten
	slog.Info("tracking PR", "pr", key, "title", tr.Title, "worktree", tr.Worktree, "created", tr.CreatedWorktree)
	if tr.SyncedSHA != pr.HeadSHA {
		t.sync(ctx, tr)
	}
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
	if reason := dropReason(pr, t.cfg.User, requested(pr, t.cfg.User, t.teams), t.cfg.IncludeDrafts); reason != "" {
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
		delete(t.state.PRs, key)
		slog.Info("dropped PR", "pr", key, "reason", reason, "removed_worktree", tr.CreatedWorktree)
		t.Events.Dropped(*tr, reason)
		return
	}
	tr.LastError = ""
	newCommits := pr.HeadSHA != tr.HeadSHA
	n, latest := newReplies(pr, t.cfg.User, tr.LastSeenAt)
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
	t.announce(tr)
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
	case tr.SyncedSHA != tr.HeadSHA:
		return // not ready; a conflict or busy worktree defers this to a later tick
	case tr.AnnouncedSHA == "":
		t.Events.Tracked(*tr)
	case tr.AnnouncedSHA != tr.HeadSHA || tr.UnannouncedReplies > 0:
		t.Events.Updated(*tr, tr.AnnouncedSHA != tr.HeadSHA, tr.UnannouncedReplies)
	default:
		return
	}
	tr.AnnouncedSHA, tr.UnannouncedReplies = tr.HeadSHA, 0
}

// Forget stops tracking id, removing its worktree if waterboy created it, so the
// next tick discovers it from scratch. It waits (until ctx ends) for whatever
// holds the worktree. Call it on the ticking goroutine.
func (t *Tracker) Forget(ctx context.Context, id string) error {
	tr := t.state.PRs[id]
	if tr == nil {
		return nil
	}
	unlock, err := t.Locks.Lock(ctx, id)
	if err != nil {
		return fmt.Errorf("waiting for %s's worktree: %w", id, err)
	}
	defer unlock()
	if tr.CreatedWorktree {
		if err := worktree.Remove(ctx, t.repoDir(tr.Repo), tr.HeadRef, tr.CreatedBranch); err != nil {
			return err
		}
	}
	delete(t.state.PRs, id)
	t.save()
	slog.Info("forgot PR", "pr", id, "removed_worktree", tr.CreatedWorktree)
	return nil
}

// forgetMissing handles a worktree deleted outside waterboy: drop the entry (the
// next tick re-tracks the PR if it's still eligible) and delete our branch, but
// only if it holds nothing unpushed (`git branch -d` refuses otherwise).
func (t *Tracker) forgetMissing(ctx context.Context, tr *Tracked) {
	repoDir := t.repoDir(tr.Repo)
	if _, err := proc.Run(ctx, repoDir, "git", "worktree", "prune"); err != nil {
		slog.Warn("pruning worktrees", "pr", tr.Key(), "err", err)
	}
	if tr.CreatedBranch {
		if _, err := proc.Run(ctx, repoDir, "git", "branch", "-d", "--", tr.HeadRef); err != nil {
			slog.Warn("keeping branch of deleted worktree", "pr", tr.Key(), "branch", tr.HeadRef, "err", err)
		}
	}
	delete(t.state.PRs, tr.Key())
	slog.Info("worktree deleted outside waterboy, forgot PR", "pr", tr.Key(), "worktree", tr.Worktree)
	t.Events.Dropped(*tr, "worktree deleted outside waterboy")
}

func (t *Tracker) sync(ctx context.Context, tr *Tracked) {
	var onConflict func(string) error
	// Hooks run at most maxHookRuns times per PR head; a persistent conflict
	// waits for a new push instead of re-running the hook every tick.
	if hook := t.cfg.Hooks["on_conflict"]; hook != "" && tr.ConflictHeadSHA != tr.HeadSHA {
		onConflict = func(reason string) error {
			return runHook(ctx, hook, t.cfg.HookTimeout.Duration, tr.Worktree, map[string]string{
				"WATERBOY_HOOK": "on_conflict", "WATERBOY_PR_URL": tr.URL, "WATERBOY_REPO": tr.Repo,
				"WATERBOY_PR_NUMBER": strconv.Itoa(tr.Number), "WATERBOY_BRANCH": tr.HeadRef,
				"WATERBOY_WORKTREE": tr.Worktree, "WATERBOY_CONFLICT_REASON": reason,
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
