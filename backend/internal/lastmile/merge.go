package lastmile

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// Merge is the dashboard's merge button: the next tick squash-merges the PR, or
// adds it to the merge queue. GitHub auto-merge isn't used for it.
func (m *Manager) Merge(_ context.Context, id string) (Handoff, error) {
	h, err := m.update(id, func(h *Handoff) error {
		if why := mergeRefusal(*h); why != "" {
			return invalid("handoff %s can't merge: %s", id, why)
		}
		h.MergeRequested = true
		return nil
	})
	if err != nil {
		return h, err
	}
	slog.Info("lastmile merge requested", "id", id)
	m.changed()
	m.Kick()
	return h, nil
}

// mergeRefusal says why the merge button can't be pressed for h, or "".
func mergeRefusal(h Handoff) string {
	s := h.Snapshot
	switch {
	case Terminal(h.State) || h.State == StateNeedsHuman:
		return "it is " + h.State
	case s == nil || !humanApproved(*s):
		return "the PR is not approved by a human on its current head"
	case s.CI != CIGreen:
		return "checks are not green (" + s.CI + ")"
	case s.InQueue:
		return "it is in the merge queue already"
	}
	return ""
}

// SetMerge changes a handoff's merge mode (Merge*).
func (m *Manager) SetMerge(id, mode string) (Handoff, error) {
	if !slices.Contains(mergeModes, mode) {
		return Handoff{}, invalid("merge mode %q isn't %s", mode, strings.Join(mergeModes, ", "))
	}
	h, err := m.update(id, func(h *Handoff) error {
		if Terminal(h.State) {
			return invalid("handoff %s is already %s", id, h.State)
		}
		h.Autonomy.Merge = mode
		if mode == MergeOff || mode == MergeShadow {
			h.MergeRequested = false
		}
		return nil
	})
	if err != nil {
		return h, err
	}
	m.changed()
	m.Kick()
	return h, nil
}

// humanApproved says the PR is approved and a human approved its current head:
// what any TLM merge needs, whatever the mode.
func humanApproved(s Snapshot) bool {
	return s.Approved() && slices.ContainsFunc(s.Approvals, func(a Approval) bool { return !a.Stale && !a.Bot })
}

// queueable are the states a PR in the merge queue shows as queued; the others
// say there is work to do.
var queueable = []string{StateApproved, StateCIGreen, StateAwaitingReview, StateCIRunning}

// mergeStep merges the PR when the button was pressed or the merge mode says so
// (design §7.1). Where the repo allows it, merge=auto and merge=approved hand the
// decision to GitHub auto-merge, which TLM turns off again when the mode goes off.
func (m *Manager) mergeStep(ctx context.Context, h *Handoff, s Snapshot, state, reason string) (string, string, error) {
	mode := h.Autonomy.Merge
	// GitHub won't enable auto-merge on a PR that is already mergeable ("clean"): TLM merges that one itself.
	native := s.AutoMergeAllowed && !h.MergeRequested && (mode == MergeAuto || mode == MergeApproved) &&
		(s.AutoMerge || s.MergeState != "CLEAN")
	switch {
	case native && !s.Draft && !s.AutoMerge:
		switch err := m.GH.EnableAutoMerge(ctx, s.NodeID); {
		case err == nil:
			h.AutoMergeSet = true
			logEvent(h, m.now(), "merge", "GitHub auto-merge enabled")
		case strings.Contains(err.Error(), "clean status"):
			native = false
		default:
			return state, reason, err
		}
	case h.AutoMergeSet && !h.MergeRequested && (mode == MergeOff || mode == MergeShadow):
		if s.AutoMerge {
			if err := m.GH.DisableAutoMerge(ctx, s.NodeID); err != nil {
				return state, reason, err
			}
			logEvent(h, m.now(), "merge", "GitHub auto-merge disabled")
		}
		h.AutoMergeSet = false
	}
	if s.InQueue {
		if slices.Contains(queueable, state) {
			return StateQueued, "", nil
		}
		return state, reason, nil
	}
	if native || state != StateApproved || s.CI != CIGreen || !humanApproved(s) {
		return state, reason, nil
	}
	if !h.MergeRequested && (frozen(m.cfg.LastMile, m.now()) || mode == MergeOff || mode == MergeShadow ||
		(mode == MergeAuto && !evaluate(*h, s, m.cfg.LastMile, m.now()).WouldMerge)) {
		return state, reason, nil
	}
	if s.MergeQueue {
		// GitHub may not show the PR in the queue for a tick or two after it was added.
		if m.now().Sub(h.EnqueuedAt) < enqueueGrace && !h.QueueSeen.After(h.EnqueuedAt) {
			return StateQueued, "", nil
		}
		if err := m.GH.Enqueue(ctx, s.NodeID); err != nil {
			return state, reason, err
		}
		h.EnqueuedAt = m.now()
		logEvent(h, m.now(), "merge", "added to the merge queue")
		return StateQueued, "", nil
	}
	if err := m.GH.MergeSquash(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, s.Head); err != nil {
		return state, reason, err
	}
	logEvent(h, m.now(), "merge", "squash-merged "+s.Head[:min(len(s.Head), 12)])
	return state, reason, nil
}

// disarm turns off the GitHub auto-merge TLM turned on for h, as TLM gives the
// PR up; the caller clears AutoMergeSet. A failure is logged: the handoff moves on.
func (m *Manager) disarm(ctx context.Context, h Handoff) {
	if !h.AutoMergeSet || h.Snapshot == nil || h.Snapshot.NodeID == "" {
		return
	}
	if err := m.GH.DisableAutoMerge(ctx, h.Snapshot.NodeID); err != nil {
		slog.Warn("lastmile disabling auto-merge failed", "id", h.ID, "err", err)
	}
}

const (
	enqueueGrace  = 2 * time.Minute
	maxQueueKicks = 10
)

// queueRemoved answers GitHub taking the PR out of the merge queue (design §7.2),
// once per removal; several at once get one answer, for the newest. handled says
// the caller is done for this tick with state and reason. A conflict, flaky checks
// and a person's dequeue leave the rest of the tick to it: drift rebases, and
// mergeStep re-queues while TLM still wants the merge.
func (m *Manager) queueRemoved(ctx context.Context, h *Handoff, s *Snapshot) (state, reason string, handled bool, err error) {
	// Removals from before the handoff (an adopted PR) are someone else's history.
	fresh := slices.DeleteFunc(slices.Clone(s.QueueRemovals), func(r github.QueueRemoval) bool {
		return !r.At.After(h.QueueSeen) || !r.At.After(h.PROpened)
	})
	if len(fresh) == 0 {
		return "", "", false, nil
	}
	human := func(r github.QueueRemoval) bool {
		return r.Actor != "" && !isBot(r.ActorBot, r.Actor, m.cfg.LastMile.BotLogins)
	}
	last := slices.MaxFunc(fresh, func(a, b github.QueueRemoval) int { return a.At.Compare(b.At) })
	if mode := h.Autonomy.Merge; !h.MergeRequested && mode != MergeAuto && mode != MergeApproved {
		h.QueueSeen = last.At // a merge TLM wasn't making: not its to answer
		return "", "", false, nil
	}
	if human(last) {
		h.QueueSeen = last.At
		h.MergeRequested, h.Autonomy.Merge = false, MergeOff
		logEvent(h, m.now(), "merge", last.Actor+" removed the PR from the merge queue: merge mode off")
		return "", "", false, nil
	}
	conflict := s.Mergeable == "CONFLICTING" || strings.Contains(strings.ToLower(last.Reason), "conflict")
	var failed []Failure
	if !conflict {
		checks, err := m.GH.QueueFailures(ctx, h.Repo, cmp.Or(s.Base, h.Base), h.PRNumber)
		if err != nil {
			return "", "", false, err
		}
		failed = m.failures(ctx, *h, checks)
	}
	h.QueueSeen = last.At
	for _, r := range fresh {
		if !human(r) {
			h.QueueKicks++
		}
	}
	what := strings.TrimSuffix("removed from the merge queue ("+last.Reason+")", " ()")
	if h.QueueKicks >= maxQueueKicks {
		h.EscalatedBy = TriggerQueue
		return StateNeedsHuman, fmt.Sprintf("removed from the merge queue %d times", maxQueueKicks), true, nil
	}
	real := slices.DeleteFunc(slices.Clone(failed), func(f Failure) bool { return f.Flake })
	names := make([]string, len(real))
	for i, f := range real {
		names[i] = f.Name
	}
	switch {
	case conflict:
		logEvent(h, m.now(), "merge", what+": conflict with the base")
		return "", "", false, nil
	case len(failed) > 0 && len(real) == 0:
		logEvent(h, m.now(), "merge", what+": flaky checks, re-queueing")
		return "", "", false, nil
	case len(real) > 0:
		why := "merge queue CI failed: " + strings.Join(names, ", ")
		logEvent(h, m.now(), "merge", what+": "+why)
		if !h.Autonomy.FixCI {
			h.EscalatedBy = TriggerCI
			return StateNeedsHuman, why, true, nil
		}
		s.Failures = real // the fix agent works from the merge group's logs
		state, reason, err := m.redState(ctx, h, *s)
		if state == StateNeedsHuman {
			h.EscalatedBy = TriggerCI
		}
		return state, reason, true, err
	}
	logEvent(h, m.now(), "merge", what+": cause unknown")
	if m.Tasks == nil {
		return StateNeedsHuman, "agents need reviews enabled", true, nil
	}
	snap := *s
	m.launch(ctx, h, TriggerQueue, func(ctx context.Context, h Handoff) outcome { return m.queueAgent(ctx, h, snap, last.Reason) })
	return StateQueued, "investigating removal from the merge queue", true, nil
}

// removeWorktree drops a merged handoff's worktree and local branch unless the
// worktree holds uncommitted or unpushed work, or is a repo's main checkout. Trouble becomes a Warning; it is not retried.
func (m *Manager) removeWorktree(ctx context.Context, h Handoff) {
	if h.Worktree == "" {
		return
	}
	if _, err := os.Stat(h.Worktree); err != nil {
		return
	}
	warn := func(w string) {
		_, _ = m.update(h.ID, func(c *Handoff) error { c.Warning = w; return nil })
		m.changed()
	}
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		warn("worktree kept: " + err.Error())
		return
	}
	defer unlock()
	st, err := status(ctx, h)
	if err != nil {
		warn("worktree kept: " + err.Error())
		return
	}
	if st != "" {
		warn("worktree kept: it has uncommitted changes")
		return
	}
	dirs, err := proc.Run(ctx, h.Worktree, "git", "rev-parse", "--path-format=absolute", "--git-dir", "--git-common-dir")
	if err != nil {
		warn("worktree kept: " + err.Error())
		return
	}
	gitDir, common, _ := strings.Cut(dirs, "\n")
	if gitDir == common {
		warn("worktree kept: not a linked worktree")
		return
	}
	if head, err := worktree.HeadSHA(ctx, h.Worktree); err != nil || h.Snapshot == nil || head != h.Snapshot.Head {
		if ahead, err := proc.Run(ctx, h.Worktree, "git", "rev-list", "@{u}..HEAD"); err != nil || ahead != "" {
			warn("worktree kept: it has unpushed commits")
			return
		}
	}
	if err := m.removeWT(ctx, filepath.Dir(common), h.Branch); err != nil {
		warn("removing the worktree failed: " + err.Error())
		return
	}
	_, _ = m.update(h.ID, func(c *Handoff) error {
		logEvent(c, m.now(), "cleanup", "removed worktree and branch "+h.Branch)
		return nil
	})
	m.changed()
}
