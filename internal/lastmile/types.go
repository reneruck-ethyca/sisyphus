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
)

// Terminal says the handoff is over: its branch may be handed off again.
func Terminal(state string) bool { return state == StateMerged || state == StateClosed }

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
	LastBodyWritten string `json:"last_body_written,omitempty"`
	FixRounds       int    `json:"fix_rounds"` // used so far; the fix agents of later phases count here

	// Reviewer bookkeeping (design §6): GitHub is the source of truth after the first request.
	BotRequested     bool     `json:"bot_requested,omitempty"`     // the bot reviewer was requested, once
	HumansRequested  bool     `json:"humans_requested,omitempty"`  // the humans were requested, once
	Requested        []string `json:"requested,omitempty"`         // everyone TLM requested
	Removed          []string `json:"removed,omitempty"`           // requested by TLM, removed on GitHub by a human
	ReviewersPending bool     `json:"reviewers_pending,omitempty"` // a reviewer edit waits to be applied on GitHub

	Snapshot *Snapshot `json:"snapshot,omitempty"` // the PR as of the last tick

	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"` // last change of State
	PROpened time.Time `json:"pr_opened,omitempty"`
}

// Notifies says whether event is one the handoff asked to be told about.
func (h Handoff) Notifies(event string) bool {
	return h.Notify != "" && slices.Contains(h.NotifyOn, event)
}
