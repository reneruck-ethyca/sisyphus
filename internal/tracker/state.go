package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/jsonfile"
)

type Tracked struct {
	github.PRRef
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	Author          string    `json:"author"` // GitHub login of whoever opened the PR
	HeadRef         string    `json:"head_ref"`
	HeadSHA         string    `json:"head_sha"`   // last head seen on GitHub
	SyncedSHA       string    `json:"synced_sha"` // worktree HEAD after our last sync; "" = never hard-reset
	Worktree        string    `json:"worktree"`
	CreatedWorktree bool      `json:"created_worktree"`
	CreatedBranch   bool      `json:"created_branch"`
	TrackedAt       time.Time `json:"tracked_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	ConflictHeadSHA string    `json:"conflict_head_sha,omitempty"` // on_conflict already exhausted for this head
	// What Events has been told: the head last announced, and replies seen since.
	AnnouncedSHA       string `json:"announced_sha,omitempty"`
	UnannouncedReplies int    `json:"unannounced_replies,omitempty"`
	LastError          string `json:"last_error,omitempty"`
}

type State struct {
	PRs     map[string]*Tracked `json:"prs"`               // key = PRRef.Key()
	Ignored map[string]Ignored  `json:"ignored,omitempty"` // untracked by hand: never picked up again until re-tracked
	History map[string]Dropped  `json:"history,omitempty"` // every PR no longer tracked, as last seen
}

// Dropped is a PR sisyphus stopped tracking, kept for the history page.
type Dropped struct {
	github.PRRef
	URL       string    `json:"url"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	TrackedAt time.Time `json:"tracked_at"`
	DroppedAt time.Time `json:"dropped_at"`
	Reason    string    `json:"reason"`
}

func (tr Tracked) AsDropped(reason string, at time.Time) Dropped {
	return Dropped{PRRef: tr.PRRef, URL: tr.URL, Title: tr.Title, Author: tr.Author, TrackedAt: tr.TrackedAt, DroppedAt: at, Reason: reason}
}

// Ignored is a PR the user untracked.
type Ignored struct {
	github.PRRef
	Title string    `json:"title"`
	At    time.Time `json:"at"`
}

// LoadState reads the state file. Missing = empty; corrupt = error, never a
// silent reset (that would forget which worktrees we created).
func LoadState(path string) (*State, error) {
	s := &State{PRs: map[string]*Tracked{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, fmt.Errorf("state %s: %w", path, err)
	}
	if s.PRs == nil {
		s.PRs = map[string]*Tracked{}
	}
	return s, nil
}

// Save writes atomically so a crash never leaves half a file.
func (s *State) Save(path string) error { return jsonfile.Save(path, s) }
