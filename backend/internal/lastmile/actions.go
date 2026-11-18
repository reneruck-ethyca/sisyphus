package lastmile

import (
	"context"
	"log/slog"
	"slices"
	"time"

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
		if _, busy := m.running.get(id); busy { // its outcome would land on a resumed handoff
			return invalid("an agent is running on %s: wait for it to finish", id)
		}
		m.unescalate(h)
		return nil
	})
	if err != nil {
		return h, err
	}
	m.dropNeedsHumanLabel(ctx, h)
	slog.Info("lastmile resumed", "id", id)
	m.changed()
	m.Kick()
	return h, nil
}

// unescalate takes h out of needs-human: the round counter of the trigger that
// escalated starts again, and the next tick reads the PR afresh from pr-open.
func (m *Manager) unescalate(h *Handoff) {
	if h.EscalatedBy == "" { // escalated for no counted reason: start every trigger afresh
		h.FixRounds = nil
	} else {
		delete(h.FixRounds, h.EscalatedBy)
	}
	// The review counter went with delete(FixRounds, EscalatedBy) above; the cursor stays, so
	// what the human dealt with isn't handed to an agent again.
	if h.EscalatedBy == TriggerCI || h.EscalatedBy == "" {
		h.FixedFailures = nil // else the failure that sent it here would escalate again at once
	}
	if h.EscalatedBy == TriggerQueue {
		h.QueueKicks = 0
	}
	// Removals while it waited are the human's to have dealt with.
	if s := h.Snapshot; s != nil {
		for _, r := range s.QueueRemovals {
			if r.At.After(h.QueueSeen) {
				h.QueueSeen = r.At
			}
		}
	}
	h.EscalatedBy, h.PrevState, h.Error = "", "", ""
	h.Dirty = nil
	m.setState(h, StatePROpen, "")
}

func (m *Manager) dropNeedsHumanLabel(ctx context.Context, h Handoff) {
	if h.PRNumber == 0 {
		return
	}
	err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{RemoveLabels: []string{m.cfg.LastMile.NeedsHumanLabel}})
	if err != nil { // the label may be gone already; the state is what matters
		slog.Warn("lastmile removing needs-human label failed", "id", h.ID, "err", err)
	}
}

// Cancel ends TLM's care of a handoff, freeing its branch. GitHub is left as it
// is, except what TLM armed there: its auto-merge and its place in the merge queue.
func (m *Manager) Cancel(ctx context.Context, id string) (Handoff, error) {
	var armed Handoff
	h, err := m.update(id, func(h *Handoff) error {
		if Terminal(h.State) {
			return invalid("handoff %s is already %s", id, h.State)
		}
		armed, h.AutoMergeSet, h.EnqueuedAt = *h, false, time.Time{}
		h.DownrevHead, h.DownrevFrom, h.DownrevRoot = "", "", ""
		m.setState(h, StateCancelled, "")
		return nil
	})
	if err != nil {
		return h, err
	}
	m.running.cancel(id)
	m.letGoLogged(ctx, &armed)
	label := m.cfg.LastMile.DownrevLabels[armed.Repo]
	if label != "" && (armed.DownrevHead != "" || (armed.Run != nil && armed.Run.Trigger == TriggerConflict)) { // a restack may have labelled
		if err := m.setDownrevLabel(ctx, armed, label, false); err != nil {
			slog.Warn("lastmile removing the downrev label failed", "id", id, "err", err)
		} else {
			if u, err := m.update(id, func(h *Handoff) error { logEvent(h, m.now(), "label", label+" removed: handoff cancelled"); return nil }); err == nil {
				h = u
			}
		}
	}
	slog.Info("lastmile cancelled", "id", id)
	m.changed()
	return h, nil
}

func (m *Manager) changed() {
	if m.OnChange != nil {
		m.OnChange()
	}
}
