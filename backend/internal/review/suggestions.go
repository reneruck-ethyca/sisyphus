package review

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"time"

	"github.com/reneruck/sisyphus/internal/jsonfile"
)

// Suggestion statuses.
const (
	SugOpen      = "open"
	SugPosting   = "posting" // accepted, being posted to GitHub
	SugReview    = "review"  // posted into the user's pending review
	SugPosted    = "posted"
	SugDismissed = "dismissed"
)

// A Suggestion is one finding of a design review, waiting for the user to accept or dismiss it.
type Suggestion struct {
	ID         string    `json:"id"`
	Run        string    `json:"run"`
	Head       string    `json:"head"`
	Kind       string    `json:"kind"` // "comment" | "reply"
	Path       string    `json:"path,omitempty"`
	Line       int       `json:"line,omitempty"`
	StartLine  int       `json:"start_line,omitempty"`
	Side       string    `json:"side,omitempty"`
	CommentID  int64     `json:"comment_id,omitempty"` // reply target (database id)
	Body       string    `json:"body"`
	Status     string    `json:"status"`
	DecidedAt  time.Time `json:"decided_at,omitzero"`
	EditedBody string    `json:"edited_body,omitempty"`
	GitHubID   string    `json:"github_id,omitempty"` // node id of the comment it became
}

// SuggestionRun is the review-level part of one design-review run.
type SuggestionRun struct {
	Run   string    `json:"run"`
	Head  string    `json:"head"`
	Event string    `json:"event"`
	Body  string    `json:"body"`
	At    time.Time `json:"at"`
}

type Suggestions struct {
	Runs  []SuggestionRun `json:"runs"`
	Items []Suggestion    `json:"items"`
}

var (
	ErrSuggestionState = errors.New("suggestion's status doesn't allow that")
	ErrNoSuggestion    = errors.New("no such suggestion")
)

// sugMoves are the status changes SetSuggestionStatus allows.
var sugMoves = map[string][]string{
	SugOpen:      {SugDismissed},
	SugDismissed: {SugOpen},
	SugPosting:   {SugOpen, SugPosted},
	SugReview:    {SugOpen, SugPosted},
}

// suggestionsPath is r.dir/suggestions/<owner>/<repo>/<n>.json for prID "owner/repo#n".
func (r *Reviewer) suggestionsPath(prID string) string {
	return filepath.Join(r.dir, "suggestions", prPath(prID)+".json")
}

func (r *Reviewer) loadSuggestions(prID string) (Suggestions, error) {
	var s Suggestions
	if err := jsonfile.Load(r.suggestionsPath(prID), &s); err != nil {
		return s, err
	}
	if s.Runs == nil {
		s.Runs = []SuggestionRun{}
	}
	if s.Items == nil {
		s.Items = []Suggestion{}
	}
	return s, nil
}

// Suggestions returns the PR's suggestions; none yet is empty, not an error.
func (r *Reviewer) Suggestions(prID string) (Suggestions, error) {
	r.sugMu.Lock()
	defer r.sugMu.Unlock()
	return r.loadSuggestions(prID)
}

// updateSuggestion runs fn on the suggestion id and saves, all under sugMu.
func (r *Reviewer) updateSuggestion(prID, id string, fn func(*Suggestion) error) (Suggestion, error) {
	r.sugMu.Lock()
	defer r.sugMu.Unlock()
	s, err := r.loadSuggestions(prID)
	if err != nil {
		return Suggestion{}, err
	}
	i := slices.IndexFunc(s.Items, func(x Suggestion) bool { return x.ID == id })
	if i < 0 {
		return Suggestion{}, ErrNoSuggestion
	}
	if err := fn(&s.Items[i]); err != nil {
		return s.Items[i], err
	}
	return s.Items[i], jsonfile.Save(r.suggestionsPath(prID), s)
}

// addSuggestions stores a run's findings as open suggestions and says how many.
func (r *Reviewer) addSuggestions(prID, runID, head string, at time.Time, f *ReviewFile) (int, error) {
	r.sugMu.Lock()
	defer r.sugMu.Unlock()
	s, err := r.loadSuggestions(prID)
	if err != nil {
		return 0, err
	}
	s.Runs = append(s.Runs, SuggestionRun{Run: runID, Head: head, Event: f.Event, Body: f.Body, At: at})
	n := 0
	add := func(sg Suggestion) {
		sg.Run, sg.Head, sg.Status = runID, head, SugOpen
		s.Items = append(s.Items, sg)
		n++
	}
	for i, c := range f.Comments {
		add(Suggestion{ID: fmt.Sprintf("%s-c%d", runID, i), Kind: "comment", Path: c.Path, Line: c.Line,
			StartLine: c.StartLine, Side: c.Side, Body: c.Body})
	}
	for i, rp := range f.Replies {
		add(Suggestion{ID: fmt.Sprintf("%s-r%d", runID, i), Kind: "reply", CommentID: rp.CommentID, Body: rp.Body})
	}
	return n, jsonfile.Save(r.suggestionsPath(prID), s)
}

// BeginAccept moves an open suggestion made on head to posting, so it's accepted once.
func (r *Reviewer) BeginAccept(prID, id, head string) (Suggestion, error) {
	return r.updateSuggestion(prID, id, func(s *Suggestion) error {
		if s.Status != SugOpen || s.Head != head {
			return ErrSuggestionState
		}
		s.Status = SugPosting
		return nil
	})
}

// FinishAccept settles a posting suggestion as review or posted, noting an edited body and the comment it became.
func (r *Reviewer) FinishAccept(prID, id, status, body, githubID string) error {
	_, err := r.updateSuggestion(prID, id, func(s *Suggestion) error {
		if s.Status != SugPosting || (status != SugReview && status != SugPosted) {
			return ErrSuggestionState
		}
		s.Status, s.GitHubID, s.DecidedAt = status, githubID, time.Now()
		if body != s.Body {
			s.EditedBody = body
		}
		return nil
	})
	return err
}

func (r *Reviewer) SetSuggestionStatus(prID, id, status string) error {
	_, err := r.updateSuggestion(prID, id, func(s *Suggestion) error {
		if !slices.Contains(sugMoves[s.Status], status) {
			return ErrSuggestionState
		}
		s.Status = status
		if status == SugOpen {
			s.DecidedAt = time.Time{}
		} else {
			s.DecidedAt = time.Now()
		}
		return nil
	})
	return err
}

// OpenSuggestions counts the PR's suggestions still waiting for a decision.
func (r *Reviewer) OpenSuggestions(prID string) int {
	s, err := r.Suggestions(prID)
	if err != nil {
		return 0
	}
	return len(slices.DeleteFunc(s.Items, func(x Suggestion) bool { return x.Status != SugOpen }))
}
