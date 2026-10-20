// waterboy tracks PRs awaiting your review: one worktree per PR, kept in sync,
// with new commits/replies recorded and finished PRs cleaned up.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"

	"fyne.io/systray"
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
	configPath := flag.String("config", defaultConfigPath(), "config file (optional)")
	once := flag.Bool("once", false, "run a single tick and exit (no tray)")
	flag.Parse()

	if err := checkTools("gh", "git", "wtp"); err != nil {
		fatal(err)
	}
	cfg, err := LoadConfig(*configPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	gh := ghCLI{}
	if cfg.User == "" {
		if cfg.User, err = gh.CurrentUser(ctx); err != nil {
			fatal(fmt.Errorf("resolving GitHub user: %w", err))
		}
	}
	st, err := LoadState(cfg.StateFile)
	if err != nil {
		fatal(err)
	}
	tracker := NewTracker(cfg, gh, st)
	slog.Info("waterboy starting", "user", cfg.User, "org", cfg.Org, "interval", cfg.Interval.Duration, "state", cfg.StateFile)

	if *once {
		if err := tracker.Tick(ctx); err != nil {
			fatal(err)
		}
		slog.Info("tick done", "summary", tracker.Summary())
		return
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; systray.Quit() }()

	systray.Run(func() { onReady(ctx, cfg, tracker) }, cancel)
}

func onReady(ctx context.Context, cfg Config, tracker *Tracker) {
	systray.SetTitle("WB")
	systray.SetTooltip("waterboy")
	status := systray.AddMenuItem("Starting…", "")
	status.Disable()
	runNowItem := systray.AddMenuItem("Run now", "")
	openState := systray.AddMenuItem("Open state file", "")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "")

	runNow := make(chan struct{}, 1)
	go func() {
		for {
			select {
			case <-runNowItem.ClickedCh:
				select {
				case runNow <- struct{}{}:
				default: // a run is already queued
				}
			case <-openState.ClickedCh:
				if err := exec.Command("open", cfg.StateFile).Run(); err != nil {
					slog.Warn("opening state file", "err", err)
				}
			case <-quit.ClickedCh:
				systray.Quit()
				return
			case <-ctx.Done():
				return
			}
		}
	}()

	// Single loop goroutine: ticks never overlap.
	go func() {
		ticker := time.NewTicker(cfg.Interval.Duration)
		defer ticker.Stop()
		for {
			status.SetTitle("Running…")
			if err := tracker.Tick(ctx); err != nil {
				slog.Error("tick failed", "err", err)
				status.SetTitle(fmt.Sprintf("Last run %s failed: %.60s", time.Now().Format("15:04"), err))
			} else {
				status.SetTitle(fmt.Sprintf("Last run %s · %s", time.Now().Format("15:04"), tracker.Summary()))
			}
			select {
			case <-ticker.C:
			case <-runNow:
			case <-ctx.Done():
				return
			}
		}
	}()
}

func fatal(err error) {
	slog.Error("waterboy", "err", err)
	os.Exit(1)
}
