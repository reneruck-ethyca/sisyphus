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
	svc := NewService(cfg, NewTracker(cfg, gh, st))
	slog.Info("waterboy starting", "user", cfg.User, "org", cfg.Org, "interval", cfg.Interval.Duration, "state", cfg.StateFile)

	if *once {
		svc.tick(ctx)
		if st := svc.Status(); st.LastError != "" {
			fatal(fmt.Errorf("%s", st.LastError))
		}
		slog.Info("tick done", "summary", svc.Status().Summary())
		return
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigs; systray.Quit() }()

	systray.Run(func() { onReady(ctx, cfg, svc) }, cancel)
}

func onReady(ctx context.Context, cfg Config, svc *Service) {
	systray.SetTitle("WB")
	systray.SetTooltip("waterboy")
	status := systray.AddMenuItem("Starting…", "")
	status.Disable()
	runNowItem := systray.AddMenuItem("Run now", "")
	dashboard := systray.AddMenuItem("Open dashboard", "")
	dashURL, err := startHTTP(cfg.HTTPAddr, svc)
	if err != nil {
		slog.Error("dashboard disabled", "addr", cfg.HTTPAddr, "err", err)
		dashboard.Disable()
	} else {
		slog.Info("dashboard", "url", dashURL)
	}
	openState := systray.AddMenuItem("Open state file", "")
	systray.AddSeparator()
	quit := systray.AddMenuItem("Quit", "")

	svc.OnChange(func(st Status) {
		switch {
		case st.Running:
			status.SetTitle("Running… · " + st.Summary())
		case st.LastError != "":
			status.SetTitle(fmt.Sprintf("Last run %s failed: %.60s", st.LastRun.Format("15:04"), st.LastError))
		default:
			status.SetTitle(fmt.Sprintf("Last run %s · %s", st.LastRun.Format("15:04"), st.Summary()))
		}
	})
	go func() {
		for {
			select {
			case <-runNowItem.ClickedCh:
				svc.RunNow()
			case <-dashboard.ClickedCh:
				if err := exec.Command("open", dashURL).Run(); err != nil {
					slog.Warn("opening dashboard", "err", err)
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
	go svc.Run(ctx)
}

func fatal(err error) {
	slog.Error("waterboy", "err", err)
	os.Exit(1)
}
