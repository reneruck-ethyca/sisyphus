package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"
)

type Tracker struct {
	cfg     Config
	gh      GitHub
	state   *State
	skipped map[string]bool // log-once for forks / missing clones
}

func NewTracker(cfg Config, gh GitHub, st *State) *Tracker {
	return &Tracker{cfg: cfg, gh: gh, state: st, skipped: map[string]bool{}}
}

// onTracked is where the next iteration launches a review. Logging only for now.
func onTracked(tr *Tracked) {
	slog.Info("tracking PR", "pr", tr.Key(), "title", tr.Title, "worktree", tr.Worktree, "created", tr.CreatedWorktree)
}

// onUpdated is where the next iteration re-triggers a review. Logging only for now.
func onUpdated(tr *Tracked, newCommits bool, replies int) {
	slog.Info("PR updated", "pr", tr.Key(), "new_commits", newCommits, "new_replies", replies)
}

func (t *Tracker) Summary() string {
	pending := 0
	for _, tr := range t.state.PRs {
		if tr.PendingUpdates {
			pending++
		}
	}
	return fmt.Sprintf("%d tracked · %d pending", len(t.state.PRs), pending)
}

func (t *Tracker) save() {
	if err := t.state.Save(t.cfg.StateFile); err != nil {
		slog.Error("saving state", "err", err)
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
	cand := map[string]bool{}
	added := map[string]bool{}
	for _, ref := range refs {
		cand[ref.Key()] = true
		if t.state.PRs[ref.Key()] == nil && t.discover(ctx, ref) {
			added[ref.Key()] = true
		}
	}
	for key, tr := range t.state.PRs {
		if !added[key] {
			t.update(ctx, tr, cand[key])
		}
	}
	return nil
}

func (t *Tracker) discover(ctx context.Context, ref PRRef) bool {
	key := ref.Key()
	pr, err := t.gh.PR(ctx, ref)
	if err != nil {
		slog.Warn("fetching PR", "pr", key, "err", err)
		return false
	}
	if dropReason(pr, t.cfg.User, true, t.cfg.IncludeDrafts) != "" {
		return false
	}
	if pr.HeadRepo != pr.Repo {
		t.skipOnce(key, "fork")
		return false
	}
	if !validBranch(ctx, pr.HeadRef) {
		t.skipOnce(key, "invalid branch name "+strconv.Quote(pr.HeadRef))
		return false
	}
	repoDir := t.repoDir(pr.Repo)
	if _, err := os.Stat(repoDir); err != nil {
		t.skipOnce(key, "no clone at "+repoDir)
		return false
	}
	wt, err := findWorktree(ctx, repoDir, pr.HeadRef)
	if err != nil {
		slog.Warn("listing worktrees", "pr", key, "err", err)
		return false
	}
	createdWT, createdBranch := false, false
	if wt == "" {
		createdBranch = !branchExists(ctx, repoDir, pr.HeadRef)
		if wt, err = createWorktree(ctx, repoDir, pr.HeadRef); err != nil {
			slog.Warn("creating worktree", "pr", key, "err", err)
			return false
		}
		createdWT = true
	}
	synced := "" // reused worktrees / pre-existing branches are never hard-reset
	if createdBranch {
		synced, _ = headSHA(ctx, wt)
	}
	_, seen := newReplies(pr, t.cfg.User, time.Time{})
	tr := &Tracked{
		PRRef: pr.PRRef, URL: pr.URL, Title: pr.Title, HeadRef: pr.HeadRef, HeadSHA: pr.HeadSHA,
		SyncedSHA: synced, Worktree: wt, CreatedWorktree: createdWT, CreatedBranch: createdBranch,
		TrackedAt: time.Now(), LastSeenAt: seen,
	}
	t.state.PRs[key] = tr
	t.save() // before anything else can fail: a created worktree must never be forgotten
	onTracked(tr)
	if tr.SyncedSHA != pr.HeadSHA {
		t.sync(ctx, tr)
		t.save()
	}
	return true
}

func (t *Tracker) update(ctx context.Context, tr *Tracked, isCandidate bool) {
	key := tr.Key()
	defer t.save()
	if _, err := os.Stat(tr.Worktree); errors.Is(err, os.ErrNotExist) {
		t.forgetMissing(ctx, tr)
		return
	}
	pr, err := t.gh.PR(ctx, tr.PRRef)
	if err != nil {
		tr.LastError = err.Error() // never drop on a fetch error
		slog.Warn("fetching PR", "pr", key, "err", err)
		return
	}
	isCandidate = isCandidate || stillMine(pr, t.cfg.User)
	if reason := dropReason(pr, t.cfg.User, isCandidate, t.cfg.IncludeDrafts); reason != "" {
		if tr.CreatedWorktree {
			if err := removeWorktree(ctx, t.repoDir(tr.Repo), tr.HeadRef, tr.CreatedBranch); err != nil {
				tr.LastError = err.Error()
				slog.Warn("removing worktree, will retry", "pr", key, "err", err)
				return
			}
		}
		delete(t.state.PRs, key)
		slog.Info("dropped PR", "pr", key, "reason", reason, "removed_worktree", tr.CreatedWorktree)
		return
	}
	tr.LastError = ""
	newCommits := pr.HeadSHA != tr.HeadSHA
	n, latest := newReplies(pr, t.cfg.User, tr.LastSeenAt)
	if newCommits || n > 0 {
		tr.HeadSHA, tr.LastSeenAt, tr.PendingUpdates = pr.HeadSHA, latest, true
		onUpdated(tr, newCommits, n)
	}
	if tr.SyncedSHA != pr.HeadSHA {
		t.sync(ctx, tr)
	}
}

// forgetMissing handles a worktree deleted outside waterboy: drop the entry (the
// next tick re-tracks the PR if it's still eligible) and delete our branch, but
// only if it holds nothing unpushed (`git branch -d` refuses otherwise).
func (t *Tracker) forgetMissing(ctx context.Context, tr *Tracked) {
	repoDir := t.repoDir(tr.Repo)
	if _, err := run(ctx, repoDir, "git", "worktree", "prune"); err != nil {
		slog.Warn("pruning worktrees", "pr", tr.Key(), "err", err)
	}
	if tr.CreatedBranch {
		if _, err := run(ctx, repoDir, "git", "branch", "-d", "--", tr.HeadRef); err != nil {
			slog.Warn("keeping branch of deleted worktree", "pr", tr.Key(), "branch", tr.HeadRef, "err", err)
		}
	}
	delete(t.state.PRs, tr.Key())
	slog.Info("worktree deleted outside waterboy, forgot PR", "pr", tr.Key(), "worktree", tr.Worktree)
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
		sha, err := syncWorktree(ctx, tr.Worktree, tr.HeadRef, tr.SyncedSHA)
		if err == nil {
			tr.SyncedSHA = sha
		}
		return err
	}, onConflict, maxHookRuns)
	var ce *ConflictError
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
