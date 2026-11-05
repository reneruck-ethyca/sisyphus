package lastmile

// Tones colour a state chip; they match the dashboard's chip classes.
const (
	ToneBad     = "bad"     // ci-red, conflict, needs-human
	ToneOK      = "ok"      // ci-green, approved, merged
	ToneNeutral = "neutral" // everything else
)

// Tone is the chip colour of a state.
func Tone(state string) string {
	switch state {
	case StateCIRed, StateConflict, StateNeedsHuman:
		return ToneBad
	case StateCIGreen, StateApproved, StateMerged:
		return ToneOK
	}
	return ToneNeutral
}

// View is a handoff as the dashboard shows it.
type View struct {
	Handoff
	Tone      string   `json:"tone"`
	Reviewing []string `json:"reviewing"` // asked to review now; before the PR is read, the planned humans
}

// Views lists the handoffs for the dashboard, newest first; never nil.
func (m *Manager) Views() []View {
	hs := m.List()
	out := make([]View, 0, len(hs))
	for i := len(hs) - 1; i >= 0; i-- {
		h := hs[i]
		v := View{Handoff: h, Tone: Tone(h.State), Reviewing: h.Reviewers}
		if h.Snapshot != nil {
			v.Reviewing = h.Snapshot.Requested
		}
		out = append(out, v)
	}
	return out
}
