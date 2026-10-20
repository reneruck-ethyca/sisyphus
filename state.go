package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Tracked struct {
	PRRef
	URL             string    `json:"url"`
	Title           string    `json:"title"`
	HeadRef         string    `json:"head_ref"`
	HeadSHA         string    `json:"head_sha"`   // last head seen on GitHub
	SyncedSHA       string    `json:"synced_sha"` // worktree HEAD after our last sync; "" = never hard-reset
	Worktree        string    `json:"worktree"`
	CreatedWorktree bool      `json:"created_worktree"`
	CreatedBranch   bool      `json:"created_branch"`
	TrackedAt       time.Time `json:"tracked_at"`
	LastSeenAt      time.Time `json:"last_seen_at"`
	ConflictHeadSHA string    `json:"conflict_head_sha,omitempty"` // on_conflict already exhausted for this head
	PendingUpdates  bool      `json:"pending_updates"`             // consumed by the (future) review launcher
	LastError       string    `json:"last_error,omitempty"`
}

type State struct {
	PRs map[string]*Tracked `json:"prs"` // key = PRRef.Key()
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

// Save writes atomically (temp file + rename) so a crash never leaves half a file.
func (s *State) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
