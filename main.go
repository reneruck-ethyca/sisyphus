// waterboy tracks PRs awaiting your review: one worktree per PR, kept in sync,
// with new commits/replies recorded and finished PRs cleaned up.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/review"
	"github.com/reneruck/waterboy/internal/tracker"
)

func checkTools(names ...string) error {
	for _, n := range names {
		if _, err := exec.LookPath(n); err != nil {
			return fmt.Errorf("%s not found in PATH (%s)", n, os.Getenv("PATH"))
		}
	}
	return nil
}

func main() {
	configPath := flag.String("config", config.DefaultPath(), "config file (optional)")
	once := flag.Bool("once", false, "run a single tick and exit (no tray)")
	reset := flag.String("reset", "", `wipe state for one PR ("owner/repo#n") or "all", then exit; stop the app first (make reset does)`)
	flag.Parse()

	// ponytail: 1000 lines covers several ticks; older lines are in the log file.
	logs := app.NewLogBuffer(1000)
	slog.SetDefault(slog.New(logs.Handler(slog.NewTextHandler(os.Stderr, nil))))

	if err := checkTools("gh", "git", "wtp"); err != nil {
		fatal(err)
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := github.CLI{}
	if cfg.User == "" && *reset == "" { // before tracker.New: it keeps its own copy of cfg
		if cfg.User, err = gh.CurrentUser(ctx); err != nil {
			fatal(fmt.Errorf("resolving GitHub user: %w", err))
		}
	}
	st, err := tracker.LoadState(cfg.StateFile)
	if err != nil {
		fatal(err)
	}
	tr := tracker.New(cfg, gh, st)
	var rev *review.Reviewer // reviews state lives next to the tracker's
	if !cfg.Review.Disabled {
		if rev, err = review.New(cfg.Review, container.Docker{}, tr.Locks, filepath.Dir(cfg.StateFile)); err != nil {
			fatal(err)
		}
	}
	if *reset != "" {
		if err := resetState(ctx, tr, rev, *reset); err != nil {
			fatal(err)
		}
		return
	}
	svc := app.NewService(cfg, tr, rev, logs)
	slog.Info("waterboy starting", "user", cfg.User, "org", cfg.Org, "interval", cfg.Interval.Duration, "state", cfg.StateFile)

	if *once {
		svc.RunOnce(ctx)
		if st := svc.Status(); st.LastError != "" {
			fatal(fmt.Errorf("%s", st.LastError))
		}
		return
	}

	if err := runApp(cfg, svc); err != nil {
		fatal(err)
	}
}

// resetState wipes one PR ("owner/repo#n") or "all": review containers, review
// history and run directories, the worktrees waterboy created, and the tracked
// entries. The next start rediscovers whatever still awaits review, as new. The
// app must not be running (it would write its state back).
func resetState(ctx context.Context, tr *tracker.Tracker, rev *review.Reviewer, which string) error {
	id := which
	if which == "all" {
		id = ""
	}
	var ids []string
	for _, t := range tr.Snapshot() {
		if id == "" || t.Key() == id {
			ids = append(ids, t.Key())
		}
	}
	if id != "" && len(ids) == 0 {
		slog.Warn("PR isn't tracked; clearing its reviews only", "pr", id)
	}
	var errs []error
	if rev != nil {
		errs = append(errs, rev.Forget(ctx, id))
	}
	for _, id := range ids {
		if err := tr.Forget(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("%s stays tracked: %w", id, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("reset %s: %w", which, err)
	}
	slog.Info("reset done", "which", which, "forgot", len(ids))
	return nil
}

func fatal(err error) {
	slog.Error("waterboy", "err", err)
	os.Exit(1)
}
