package lastmile

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
		h.MergeRequested, h.MergeRequestedHead = true, h.Snapshot.Head
		logEvent(h, m.now(), "merge", "merge requested from the dashboard")
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

// Rebase asks for the branch to be rebased onto its base on the next tick, as when
// GitHub reports it behind: a clean rebase runs on the host, a conflicting one goes
// to the conflict agent (fix-ci) or a human. From needs-human it resumes the handoff
// too: a rebase is often what the human was asked for (a fix that is on the base).
func (m *Manager) Rebase(ctx context.Context, id string) (Handoff, error) {
	resumed := false
	h, err := m.update(id, func(h *Handoff) error {
		switch {
		case Terminal(h.State):
			return invalid("handoff %s is %s", id, h.State)
		case h.Dirty != nil:
			return invalid("%s has uncommitted changes: deal with them first", h.Worktree)
		case h.Snapshot == nil || h.Snapshot.Behind == 0:
			return invalid("%s is not behind its base", h.Branch)
		}
		if h.State == StateNeedsHuman {
			if _, busy := m.running.get(id); busy {
				return invalid("an agent is running on %s: wait for it to finish", id)
			}
			m.unescalate(h)
			resumed = true
		}
		h.RebaseAsked = "dashboard"
		logEvent(h, m.now(), "rebase", "rebase requested from the dashboard")
		return nil
	})
	if err != nil {
		return h, err
	}
	if resumed {
		m.dropNeedsHumanLabel(ctx, h)
	}
	slog.Info("lastmile rebase requested", "id", id)
	m.changed()
	m.Kick()
	return h, nil
}

// mergeRefusal says why the merge button can't be pressed for h, or "". Like
// GitHub's own button it takes GitHub's review decision: an approval of an older
// head counts when the repo keeps stale approvals. The merge modes stay stricter.
func mergeRefusal(h Handoff) string {
	s := h.Snapshot
	switch {
	case Terminal(h.State) || h.State == StateNeedsHuman:
		return "it is " + h.State
	case s == nil || !s.Approved():
		return "the PR is not approved"
	case s.CI != CIGreen:
		return "checks are not green (" + s.CI + ")"
	case s.InQueue:
		return "it is in the merge queue already"
	}
	return ""
}

// ManualMerge says repo is in lastmile.manual_merge: its PRs merge only from the
// dashboard, by the merge button or a merge mode set there.
func (m *Manager) ManualMerge(repo string) bool { return m.cfg.LastMile.Manual(repo) }

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
		logEvent(h, m.now(), "merge", "merge mode set to "+mode)
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
// what a merge mode needs to merge (the button takes GitHub's decision alone). An approval of the head before a
// labelled downrev bump, or a chain of them, counts for the bumped head (design §7.4).
func humanApproved(h Handoff, s Snapshot) bool {
	carried := func(a Approval) bool {
		return h.DownrevHead != "" && h.DownrevHead == s.Head && (a.CommitSHA == h.DownrevRoot || a.CommitSHA == h.DownrevFrom)
	}
	return s.Approved() && slices.ContainsFunc(s.Approvals, func(a Approval) bool { return !a.Bot && (!a.Stale || carried(a)) })
}

// queueable are the states a PR in the merge queue shows as queued; the others
// say there is work to do.
var queueable = []string{StateApproved, StateCIGreen, StateAwaitingReview, StateCIRunning}

// mergeStep merges the PR when the button was pressed or the merge mode says so
// (design §7.1). Where the repo allows it, merge=auto and merge=approved hand the
// decision to GitHub auto-merge, which TLM turns off again when the mode goes off.
func (m *Manager) mergeStep(ctx context.Context, h *Handoff, s Snapshot, state, reason string) (string, string, error) {
	mode := h.Autonomy.Merge
	s.AutoMerge = m.autoMergeOn(*h, s)
	// A press merges the head the person saw: GitHub may keep the approval across a push they didn't see.
	if h.MergeRequested && s.Head != h.MergeRequestedHead {
		h.MergeRequested, h.MergeRequestedHead = false, ""
		logEvent(h, m.now(), "merge", "the head moved after merge was pressed; press it again to merge the new head")
	}
	// GitHub may clear the auto-merge request as it moves the PR into the queue.
	if h.AutoMergeSet && (mode == MergeAuto || mode == MergeApproved) && !s.AutoMerge && !s.InQueue &&
		s.Head == h.AutoMergeHead && h.QueueKicks == h.AutoMergeKicks {
		// GitHub drops auto-merge on a push or a removal from the queue; with neither, a person turned it off.
		h.Autonomy.Merge, mode = MergeOff, MergeOff
		h.AutoMergeSet, h.MergeRequested = false, false
		logEvent(h, m.now(), "merge", "auto-merge turned off on GitHub; merge mode set to off")
	}
	// GitHub won't enable auto-merge on a PR that is already mergeable: TLM merges that one itself.
	// Nor does TLM hand it a PR on a repo that requires no review: GitHub would merge it unapproved.
	native := s.AutoMergeAllowed && !h.MergeRequested && (mode == MergeAuto || mode == MergeApproved) &&
		s.ReviewDecision != "" && (s.AutoMerge || !slices.Contains(mergeableNow, s.MergeState))
	switch {
	case native && !s.Draft && !s.AutoMerge && !s.InQueue:
		switch err := m.GH.EnableAutoMerge(ctx, s.NodeID); {
		case err == nil:
			h.AutoMergeSet, h.AutoMergeHead, h.AutoMergeKicks, h.AutoMergeAt = true, s.Head, h.QueueKicks, m.now()
			if h.Snapshot != nil {
				h.Snapshot.AutoMerge = true
			}
			logEvent(h, m.now(), "merge", "GitHub auto-merge enabled")
		case strings.Contains(err.Error(), "clean status"):
			native = false
		default:
			return m.refused(ctx, h, err, state, reason)
		}
	case !h.MergeRequested && (mode == MergeOff || mode == MergeShadow):
		if err := m.letGo(ctx, h); err != nil {
			return state, reason, err
		}
	}
	if s.InQueue {
		if slices.Contains(queueable, state) {
			return StateQueued, "", nil
		}
		return state, reason, nil
	}
	if native || state != StateApproved || s.CI != CIGreen || !(h.MergeRequested || humanApproved(*h, s)) {
		return state, reason, nil
	}
	if !h.MergeRequested && (frozen(m.cfg.LastMile, m.now()) || mode == MergeOff || mode == MergeShadow ||
		(mode == MergeAuto && !evaluate(*h, s, m.cfg.LastMile, m.now()).WouldMerge)) {
		return state, reason, nil
	}
	if s.MergeQueue {
		// GitHub may not show the PR in the queue for a tick or two after it was added.
		if m.enqueuing(*h) {
			return StateQueued, "", nil
		}
		if err := m.GH.Enqueue(ctx, s.NodeID); err != nil {
			return m.refused(ctx, h, err, state, reason)
		}
		h.EnqueuedAt, h.EnqueuedHead = m.now(), s.Head
		logEvent(h, m.now(), "merge", "added to the merge queue")
		return StateQueued, "", nil
	}
	if err := m.GH.MergeSquash(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, s.Head); err != nil {
		return m.refused(ctx, h, err, state, reason)
	}
	logEvent(h, m.now(), "merge", "squash-merged "+s.Head[:min(len(s.Head), 12)])
	m.Kick() // the next tick reads the PR as merged and finishes the handoff, instead of a full interval later
	return state, reason, nil
}

// mergeableNow are the merge states GitHub won't enable auto-merge in: the PR can merge as it is.
var mergeableNow = []string{"CLEAN", "UNSTABLE", "HAS_HOOKS"}

// refused answers GitHub refusing a merge: a refusal won't go away by itself, so
// it is a person's to look at. A head that moved under the merge is tried again
// on the next tick.
func (m *Manager) refused(ctx context.Context, h *Handoff, err error, state, reason string) (string, string, error) {
	if ctx.Err() != nil {
		return state, reason, err
	}
	if github.IsStatus(err, http.StatusConflict) {
		slog.Info("lastmile merge: the head moved", "id", h.ID, "err", err)
		return state, reason, nil
	}
	why := "GitHub refused the merge: " + err.Error()
	logEvent(h, m.now(), "merge", why)
	return StateNeedsHuman, why, nil
}

// autoMergeOn says GitHub auto-merge is on for the PR, or TLM enabled it a
// moment ago and GitHub may not show it yet.
func (m *Manager) autoMergeOn(h Handoff, s Snapshot) bool {
	return s.AutoMerge || (h.AutoMergeSet && m.now().Sub(h.AutoMergeAt) < enqueueGrace)
}

// enqueuing says TLM added the PR to the merge queue a moment ago, and GitHub
// may not show it there yet.
func (m *Manager) enqueuing(h Handoff) bool {
	return m.now().Sub(h.EnqueuedAt) < enqueueGrace && !h.QueueSeen.After(h.EnqueuedAt)
}

// letGo undoes what TLM armed on GitHub for h as it lets the PR go: its
// auto-merge and its place in the merge queue. What it undid is cleared on h;
// what failed stays set, and the error says why.
func (m *Manager) letGo(ctx context.Context, h *Handoff) error {
	s := h.Snapshot
	if s == nil || s.NodeID == "" {
		return nil
	}
	var errs []error
	if h.AutoMergeSet && m.autoMergeOn(*h, *s) {
		if err := m.GH.DisableAutoMerge(ctx, s.NodeID); err != nil {
			errs = append(errs, err)
		} else {
			h.AutoMergeSet = false
			logEvent(h, m.now(), "merge", "GitHub auto-merge disabled")
		}
	} else {
		h.AutoMergeSet = false
	}
	if !h.EnqueuedAt.IsZero() && (s.InQueue || m.enqueuing(*h)) {
		if err := m.GH.Dequeue(ctx, s.NodeID); err != nil {
			errs = append(errs, err)
		} else {
			h.EnqueuedAt, h.DequeuedAt = time.Time{}, m.now()
			logEvent(h, m.now(), "merge", "taken out of the merge queue")
		}
	} else {
		h.EnqueuedAt = time.Time{}
	}
	return errors.Join(errs...)
}

// letGoLogged is letGo for a handoff TLM gives up on whatever happens: a failure
// is logged, and h forgets what it armed.
func (m *Manager) letGoLogged(ctx context.Context, h *Handoff) {
	if err := m.letGo(ctx, h); err != nil {
		slog.Warn("lastmile letting go of the merge failed", "id", h.ID, "err", err)
	}
	h.AutoMergeSet, h.EnqueuedAt = false, time.Time{}
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
	// TLM's own dequeue shows as a removal by the user it acts as, a moment after it.
	ours := func(r github.QueueRemoval) bool {
		return !h.DequeuedAt.IsZero() && r.At.After(h.DequeuedAt.Add(-time.Minute)) && r.At.Before(h.DequeuedAt.Add(enqueueGrace))
	}
	fresh := slices.DeleteFunc(slices.Clone(s.QueueRemovals), func(r github.QueueRemoval) bool {
		return !r.At.After(h.QueueSeen) || !r.At.After(h.PROpened) || ours(r)
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
	if h.EnqueuedHead != "" && s.Head != h.EnqueuedHead {
		h.QueueSeen = last.At // a push took it out; the new head goes back in once approved
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
	// TLM queues as the user, so their login on a removal says little: a cause makes it a kick-out.
	if !conflict && len(failed) == 0 && human(last) {
		h.QueueSeen = last.At
		h.MergeRequested, h.Autonomy.Merge = false, MergeOff
		logEvent(h, m.now(), "merge", last.Actor+" removed the PR from the merge queue: merge mode off")
		return "", "", false, nil
	}
	h.QueueSeen = last.At
	for _, r := range fresh {
		if !human(r) || r.At.Equal(last.At) {
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
	if branch, err := proc.Run(ctx, h.Worktree, "git", "symbolic-ref", "--short", "HEAD"); err != nil || branch != h.Branch {
		warn("worktree kept: it is not on " + h.Branch)
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
