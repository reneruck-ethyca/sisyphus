// Package testkit builds trackers on temp dirs for tests outside package tracker.
package testkit

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/tracker"
)

var RefA = github.PRRef{Repo: "ethyca/a", Number: 1}

// Tracker returns a config for user "me" on temp dirs and a tracker over the
// given PRs. Empty worktrees are pointed at an existing dir, or Tick would treat
// them as deleted by hand.
func Tracker(t testing.TB, gh github.Client, tracked ...*tracker.Tracked) (config.Config, *tracker.Tracker) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{User: "me", Org: "ethyca", Interval: config.Duration{Duration: time.Hour},
		ReposRoot: filepath.Join(dir, "repos"), StateFile: filepath.Join(dir, "state.json"), Path: filepath.Join(dir, "config.json")}
	os.Mkdir(cfg.ReposRoot, 0o755)
	st := &tracker.State{PRs: map[string]*tracker.Tracked{}}
	for _, tr := range tracked {
		if tr.Worktree == "" {
			tr.Worktree = dir
		}
		st.PRs[tr.Key()] = tr
	}
	return cfg, tracker.New(cfg, gh, st)
}
