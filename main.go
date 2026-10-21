// waterboy tracks PRs awaiting your review: one worktree per PR, kept in sync,
// with new commits/replies recorded and finished PRs cleaned up.
package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/tracker"
	"log/slog"
	"os"
	"os/exec"
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
	if cfg.User == "" {
		if cfg.User, err = gh.CurrentUser(ctx); err != nil {
			fatal(fmt.Errorf("resolving GitHub user: %w", err))
		}
	}
	st, err := tracker.LoadState(cfg.StateFile)
	if err != nil {
		fatal(err)
	}
	svc := app.NewService(cfg, tracker.New(cfg, gh, st), logs)
	slog.Info("waterboy starting", "user", cfg.User, "org", cfg.Org, "interval", cfg.Interval.Duration, "state", cfg.StateFile)

	if *once {
		svc.RunOnce(ctx)
		if st := svc.Status(); st.LastError != "" {
			fatal(fmt.Errorf("%s", st.LastError))
		}
		slog.Info("tick done", "summary", svc.Status().Summary())
		return
	}

	if err := runApp(cfg, svc); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	slog.Error("waterboy", "err", err)
	os.Exit(1)
}
