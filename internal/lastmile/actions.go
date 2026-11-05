package lastmile

import (
	"context"
	"log/slog"
	"slices"

	"github.com/reneruck/sisyphus/internal/github"
)

// update applies fn to the stored handoff id under the lock and saves it. It
// bumps Gen, so a tick that started before it won't overwrite the change.
func (m *Manager) update(id string, fn func(h *Handoff) error) (Handoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.handoffs, func(h Handoff) bool { return h.ID == id })
	if i < 0 {
		return Handoff{}, ErrNotFound
	}
	h := m.handoffs[i]
	if err := fn(&h); err != nil {
		return Handoff{}, err
	}
	h.Gen++
	prev := m.handoffs[i]
	m.handoffs[i] = h
	if err := m.saveLocked(); err != nil {
		m.handoffs[i] = prev
		return Handoff{}, err
	}
	return h, nil
}

// Resume takes a handoff back from needs-human: the label goes, the round
// counter of the trigger that escalated starts again, and the next tick reads
// the PR afresh from pr-open.
func (m *Manager) Resume(ctx context.Context, id string) (Handoff, error) {
	h, err := m.update(id, func(h *Handoff) error {
		if h.State != StateNeedsHuman {
			return invalid("handoff %s is %s, not needs-human", id, h.State)
		}
		if h.EscalatedBy == "" { // escalated for no counted reason: start every trigger afresh
			h.FixRounds = nil
		} else {
			delete(h.FixRounds, h.EscalatedBy)
		}
		if h.EscalatedBy == TriggerCI || h.EscalatedBy == "" {
			h.FixedFailures = nil // else the failure that sent it here would escalate again at once
		}
		h.EscalatedBy, h.PrevState, h.Error = "", "", ""
		m.setState(h, StatePROpen, "")
		return nil
	})
	if err != nil {
		return h, err
	}
	if h.PRNumber != 0 {
		err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{RemoveLabels: []string{m.cfg.LastMile.NeedsHumanLabel}})
		if err != nil { // the label may be gone already; the state is what matters
			slog.Warn("lastmile removing needs-human label failed", "id", id, "err", err)
		}
	}
	slog.Info("lastmile resumed", "id", id)
	m.changed()
	m.Kick()
	return h, nil
}

// Cancel ends TLM's care of a handoff, freeing its branch. GitHub is left as it is.
func (m *Manager) Cancel(id string) (Handoff, error) {
	h, err := m.update(id, func(h *Handoff) error {
		if Terminal(h.State) {
			return invalid("handoff %s is already %s", id, h.State)
		}
		m.setState(h, StateCancelled, "")
		return nil
	})
	if err != nil {
		return h, err
	}
	m.running.cancel(id)
	slog.Info("lastmile cancelled", "id", id)
	m.changed()
	return h, nil
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}
