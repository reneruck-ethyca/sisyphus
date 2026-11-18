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
	case s == nil || !s.Approved():
		return "the PR is not approved"
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
		return nil
	})
	if err != nil {
		return h, err
	}
	m.changed()
	m.Kick()
	return h, nil
}

// mergeStep merges an approved PR when the button was pressed or the merge mode
// says so (design §7.1), and tells a PR in the merge queue apart.
func (m *Manager) mergeStep(ctx context.Context, h *Handoff, s Snapshot, state, reason string) (string, string, error) {
	if s.InQueue {
		return StateQueued, "", nil
	}
	mode := h.Autonomy.Merge
	want := h.MergeRequested || (mode == MergeApproved && s.Approved()) ||
		(mode == MergeAuto && evaluate(*h, s, m.cfg.LastMile, m.now()).WouldMerge)
	if !want {
		return state, reason, nil
	}
	if s.AutoMergeAllowed && !h.MergeRequested {
		if !s.AutoMerge {
			if err := m.GH.EnableAutoMerge(ctx, s.NodeID); err != nil {
				return state, reason, err
			}
			logEvent(h, m.now(), "merge", "GitHub auto-merge enabled")
		}
		return state, reason, nil
	}
	if s.CI != CIGreen || !s.Approved() {
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
