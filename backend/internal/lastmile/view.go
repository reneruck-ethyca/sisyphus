package lastmile

import "time"

// View is a handoff as the dashboard shows it.
type View struct {
	Handoff
	Over      bool     `json:"over"`                 // the handoff is over: merged, closed or cancelled
	Reviewing []string `json:"reviewing"`            // asked to review now; before the PR is read, the planned humans
	Depth     int      `json:"depth"`                // how far down a stack: 0 for a root or a handoff on its own
	WaitingOn string   `json:"waiting_on,omitempty"` // the open parent this stacked handoff sits on
	DirtyHash string   `json:"dirty_hash,omitempty"` // Dirty.Hash, which the dirty panel's actions send back

	// The agent run in flight for this handoff: its trigger (Trigger*) and start; "" = none.
	// A finished run waiting for the next tick to apply it doesn't count.
	AgentRunning string    `json:"agent_running,omitempty"`
	AgentStarted time.Time `json:"agent_started,omitzero"`
}

// Views lists the handoffs for the dashboard, newest root first; never nil. A stack
// is kept together: each parent is followed by its children, oldest first, with Depth set.
func (m *Manager) Views() []View {
	hs := m.List()
	byID := map[string]View{}
	kids := map[string][]string{} // parent id -> children ids, oldest first
	for _, h := range hs {
		v := View{Handoff: h, Over: Terminal(h.State), Reviewing: h.Reviewers}
		if h.Snapshot != nil {
			v.Reviewing = h.Snapshot.Requested
		}
		if h.Dirty != nil {
			v.DirtyHash = h.Dirty.Hash()
		}
		if a, ok := m.running.get(h.ID); ok && !a.done {
			v.AgentRunning, v.AgentStarted = a.trigger, a.started
		}
		byID[h.ID] = v
	}
	for _, h := range hs {
		if _, ok := byID[h.StackParent]; ok {
			kids[h.StackParent] = append(kids[h.StackParent], h.ID)
		}
	}
	out := make([]View, 0, len(hs))
	var add func(id string, depth int)
	add = func(id string, depth int) {
		v := byID[id]
		v.Depth = depth
		if parent, ok := byID[v.StackParent]; ok && !parent.Over {
			v.WaitingOn = parent.ID
		}
		out = append(out, v)
		for _, c := range kids[id] {
			add(c, depth+1)
		}
	}
	for i := len(hs) - 1; i >= 0; i-- {
		if _, ok := byID[hs[i].StackParent]; !ok {
			add(hs[i].ID, 0)
		}
	}
	return out
}
