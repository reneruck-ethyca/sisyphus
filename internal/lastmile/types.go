// Package lastmile is the-last-mile (TLM): an agent hands over a finished
// branch, and TLM takes the PR from there (design: docs/the-last-mile.md).
package lastmile

import (
	"slices"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
)

// Handoff states (design §4).
const (
	StateHandedOff      = "handed-off"
	StatePROpen         = "pr-open"
	StateCIRunning      = "ci-running"
	StateCIRed          = "ci-red"
	StateCIGreen        = "ci-green"
	StateAwaitingReview = "awaiting-review"
	StateFeedback       = "feedback"
	StateApproved       = "approved"
	StateMergeable      = "mergeable"
	StateConflict       = "conflict"
	StateMerged         = "merged"
	StateNeedsHuman     = "needs-human"
	StateClosed         = "closed"
	StateCancelled      = "cancelled" // a person stopped TLM managing it; GitHub is as it was
)

// Terminal says the handoff is over: its branch may be handed off again.
func Terminal(state string) bool {
	return state == StateMerged || state == StateClosed || state == StateCancelled
}

// Triggers an agent round can answer; FixRounds is counted per trigger.
const (
	TriggerCI     = "ci"
	TriggerReview = "review"
)

// Notify events.
const (
	EventPROpened          = "pr-opened"
	EventMerged            = "merged"
	EventNeedsHuman        = "needs-human"
	EventClosed            = "closed"
	EventStackParentMerged = "stack-parent-merged"
)

var events = []string{EventPROpened, EventMerged, EventNeedsHuman, EventClosed, EventStackParentMerged}

// defaultNotifyOn is what --notify fires on when --notify-on says nothing.
var defaultNotifyOn = []string{EventMerged, EventNeedsHuman, EventClosed}

// Merge modes of an Autonomy.
const (
	MergeOff    = "off"
	MergeShadow = "shadow"
	MergeAuto   = "auto" // not supported yet
)

// Autonomy is what TLM may do on its own for a handoff.
type Autonomy struct {
	FixCI          bool   `json:"fix_ci"`
	AddressReviews bool   `json:"address_reviews"`
	Merge          string `json:"merge"` // Merge*
}

// Request is a handoff as the CLI and API send it. Strings that the CLI takes
// as comma lists (Autonomy, Agent) arrive as written.
type Request struct {
	Worktree     string   `json:"worktree"` // absolute path of the worktree to hand over
	Base         string   `json:"base"`
	StackParent  string   `json:"stack_parent,omitempty"` // handoff id or branch; not acted on yet
	Title        string   `json:"title,omitempty"`        // "" = the last commit's subject
	Body         string   `json:"body,omitempty"`         // "" = the repo's PR template
	Jira         string   `json:"jira,omitempty"`
	Labels       []string `json:"labels,omitempty"`
	Reviewers    []string `json:"reviewers,omitempty"` // replace the roster
	Draft        bool     `json:"draft"`
	Autonomy     string   `json:"autonomy,omitempty"` // "fix-ci,address-reviews,merge=off|shadow"
	Notify       string   `json:"notify,omitempty"`   // Go text/template, run through sh -c
	NotifyOn     []string `json:"notify_on,omitempty"`
	Agent        string   `json:"agent,omitempty"` // "vendor:model:effort"; "" = review.agent
	MaxFixRounds int      `json:"max_fix_rounds,omitempty"`
	Requester    string   `json:"requester,omitempty"` // free text, for {{.Requester}}
}

// Handoff is one branch under TLM's care. Everything about it is keyed by ID.
type Handoff struct {
	ID           string       `json:"id"`
	Repo         string       `json:"repo"` // "owner/name"
	Branch       string       `json:"branch"`
	Worktree     string       `json:"worktree"`
	Base         string       `json:"base"`
	StackParent  string       `json:"stack_parent,omitempty"`
	Title        string       `json:"title"`
	Body         string       `json:"body,omitempty"`
	Jira         string       `json:"jira,omitempty"`
	Labels       []string     `json:"labels,omitempty"`
	Reviewers    []string     `json:"reviewers,omitempty"`
	Draft        bool         `json:"draft"`
	Autonomy     Autonomy     `json:"autonomy"`
	Notify       string       `json:"notify,omitempty"`
	NotifyOn     []string     `json:"notify_on,omitempty"`
	Agent        config.Agent `json:"agent"`
	MaxFixRounds int          `json:"max_fix_rounds"`
	Requester    string       `json:"requester,omitempty"`

	// ReviewersSet says Reviewers is the user's choice even when empty (a dashboard edit
	// to "nobody"); otherwise an empty Reviewers means "use the roster".
	ReviewersSet bool `json:"reviewers_set,omitempty"`

	State     string `json:"state"`
	PrevState string `json:"prev_state,omitempty"` // State before needs-human, so the UI can say "was ci-red"
	Reason    string `json:"reason,omitempty"`     // why it's in State, when that needs saying (ci-red, needs-human)
	Error     string `json:"error,omitempty"`      // the last tick's failure; "" after a good one
	Warning   string `json:"warning,omitempty"`    // something to look at that isn't a failure (no reviewers)
	PRNumber  int    `json:"pr_number,omitempty"`
	PRURL     string `json:"pr_url,omitempty"`
	// LastBodyWritten is the PR body TLM last wrote, to tell a human's edit from its own (M2).
	LastBodyWritten string         `json:"last_body_written,omitempty"`
	FixRounds       map[string]int `json:"fix_rounds,omitempty"`     // agent rounds used so far, by trigger (Trigger*)
	EscalatedBy     string         `json:"escalated_by,omitempty"`   // the trigger of the last needs-human, for Resume
	FixedFailures   []string       `json:"fixed_failures,omitempty"` // fingerprints a fix round was spent on
	FlakeHead       string         `json:"flake_head,omitempty"`     // the head SHA FlakeReruns counts for
	FlakeReruns     int            `json:"flake_reruns,omitempty"`   // flake reruns on FlakeHead
	Gen             int            `json:"gen,omitempty"`            // bumped by edits from outside the tick, which discard a tick in progress

	// Reviewer bookkeeping (design §6): GitHub is the source of truth after the first request.
	BotRequested     bool     `json:"bot_requested,omitempty"`     // the bot reviewer was requested, once
	HumansRequested  bool     `json:"humans_requested,omitempty"`  // the humans were requested, once
	Requested        []string `json:"requested,omitempty"`         // everyone TLM requested
	Removed          []string `json:"removed,omitempty"`           // requested by TLM, removed on GitHub by a human
	ReviewersPending bool     `json:"reviewers_pending,omitempty"` // a reviewer edit waits to be applied on GitHub

	// Review inbox bookkeeping (design §4, §5).
	Cursor       Cursor   `json:"cursor"`                  // what the review agent has been handed
	Posted       []int64  `json:"posted,omitempty"`        // ids of the comments TLM posted (last maxPosted): TLM posts as the user
	FixedThreads []string `json:"fixed_threads,omitempty"` // threads TLM answered "Fixed in …"; a human comment after it is a re-raise

	Events []Event `json:"events,omitempty"` // the last maxEvents things that happened, oldest first

	Snapshot *Snapshot `json:"snapshot,omitempty"` // the PR as of the last tick

	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"` // last change of State
	PROpened time.Time `json:"pr_opened,omitempty"`
}

// Event is one line of a handoff's log.
type Event struct {
	Time time.Time `json:"time"`
	Kind string    `json:"kind"` // pr-opened | state | agent | commit | reply | skip | bot-cap | body | reviewer | escalated
	Text string    `json:"text"`
}

// maxEvents and maxPosted bound what a handoff remembers.
const (
	maxEvents = 100
	maxPosted = 500
)

// logEvent appends to h's log, dropping the oldest beyond maxEvents. It builds a
// new slice: handoffs are shared by copy.
func logEvent(h *Handoff, t time.Time, kind, text string) {
	ev := append(slices.Clone(h.Events), Event{Time: t, Kind: kind, Text: text})
	if len(ev) > maxEvents {
		ev = ev[len(ev)-maxEvents:]
	}
	h.Events = ev
}

// Notifies says whether event is one the handoff asked to be told about.
func (h Handoff) Notifies(event string) bool {
	return h.Notify != "" && slices.Contains(h.NotifyOn, event)
}
