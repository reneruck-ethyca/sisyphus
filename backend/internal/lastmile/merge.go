package lastmile

import (
	"context"
	"log/slog"
	"slices"
	"strings"

	"github.com/reneruck/sisyphus/internal/github"
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
	native := s.AutoMergeAllowed && !h.MergeRequested && (mode == MergeAuto || mode == MergeApproved)
	switch {
	case native && !s.Draft && !s.AutoMerge:
		if err := m.GH.EnableAutoMerge(ctx, s.NodeID); err != nil {
			return state, reason, err
		}
		h.AutoMergeSet = true
		logEvent(h, m.now(), "merge", "GitHub auto-merge enabled")
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
		if err := m.GH.Enqueue(ctx, s.NodeID); err != nil {
			return state, reason, err
		}
		logEvent(h, m.now(), "merge", "added to the merge queue")
		return StateQueued, "", nil
	}
	if err := m.GH.MergeSquash(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, s.Head); err != nil {
		return state, reason, err
	}
	logEvent(h, m.now(), "merge", "squash-merged "+s.Head[:min(len(s.Head), 12)])
	return state, reason, nil
}
