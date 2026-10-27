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
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"
	"time"

	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/eval"
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
	shell := flag.String("shell", "", `open a shell in the container a review of PR "owner/repo#n" would run in`)
	evalCases := flag.String("eval", "", "replay the gold review cases in this file or dir (README: Evals), then judge them")
	evalOut := flag.String("eval-out", "", "where -eval puts its runs (default ~/.config/waterboy/eval/runs/<time>); with -judge, the runs to re-judge")
	evalReps := flag.Int("eval-reps", 3, "runs per case")
	judge := flag.Bool("judge", false, "re-judge the runs in -eval-out (after editing the cases) and rewrite its report")
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
	if *judge {
		if *evalOut == "" {
			fatal(errors.New("-judge needs -eval-out"))
		}
		if err := eval.Judge(ctx, *evalOut, true); err != nil {
			fatal(err)
		}
		return
	}
	if cfg.User == "" && *reset == "" && *shell == "" && *evalCases == "" { // before tracker.New: it keeps its own copy of cfg
		if cfg.User, err = gh.CurrentUser(ctx); err != nil {
			fatal(fmt.Errorf("resolving GitHub user: %w", err))
		}
	}
	st, err := tracker.LoadState(cfg.StateFile)
	if err != nil {
		fatal(err)
	}
	tr := tracker.New(cfg, gh, st)
	if *shell != "" {
		if err := openShell(ctx, cfg, tr, gh, *shell); err != nil {
			fatal(err)
		}
		return
	}
	if *evalCases != "" {
		if err := runEval(ctx, cfg, tr, gh, *evalCases, *evalOut, *evalReps); err != nil {
			fatal(err)
		}
		return
	}
	var rev *review.Reviewer // reviews state lives next to the tracker's
	if !cfg.Review.Disabled {
		if rev, err = review.New(cfg.Review, container.Docker{}, tr.Locks, filepath.Dir(cfg.StateFile)); err != nil {
			fatal(err)
		}
		rev.GitHub = gh
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

// openShell runs an interactive shell in the container a review of PR id would
// run in, with its stack up, next to a running app. Its records go to a scratch
// dir, so the app's state is left alone.
// ponytail: the stack starts at the default PORT_OFFSET, not the one the app
// remembered for it; a clash still retries with the next.
func openShell(ctx context.Context, cfg config.Config, tr *tracker.Tracker, gh github.CLI, id string) error {
	i := slices.IndexFunc(tr.Snapshot(), func(t tracker.Tracked) bool { return t.Key() == id })
	if i < 0 {
		return fmt.Errorf("%s isn't tracked: no worktree to open a shell on", id)
	}
	pr := tr.Snapshot()[i]
	dir, err := os.MkdirTemp("", "waterboy-shell-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	rt := container.Docker{}
	rev, err := review.New(cfg.Review, rt, tr.Locks, dir)
	if err != nil {
		return err
	}
	rev.GitHub = gh
	spec, stop, err := rev.Shell(ctx, pr)
	defer stop()
	if err != nil {
		return err
	}
	var exit *exec.ExitError
	if err := rt.Interactive(ctx, spec); err != nil && !(errors.As(err, &exit) && exit.ExitCode() != 125) {
		return fmt.Errorf("docker run: %w", err) // 125: docker's own failure, not the shell's exit status
	}
	return nil
}

// runEval replays cases with this config; the runs and report go to out.
func runEval(ctx context.Context, cfg config.Config, tr *tracker.Tracker, gh github.CLI, cases, out string, reps int) error {
	cs, err := eval.LoadCases(cases)
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Join(filepath.Dir(config.DefaultPath()), "eval", "runs", time.Now().Format("20060102-150405"))
	}
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.Info("eval", "cases", len(cs), "reps", reps, "out", out)
	return eval.Runner{Cfg: cfg, RT: container.Docker{}, GH: gh, Locks: tr.Locks, Out: out, Reps: reps}.Run(ctx, cs)
}

// restart re-execs waterboy with the same arguments, so saved settings apply.
// Review containers keep running and are re-attached; a tick in flight is cut
// short, as with make restart.
var restart = func() {
	exe, err := os.Executable()
	if err == nil {
		err = syscall.Exec(exe, os.Args, os.Environ())
	}
	slog.Error("restart failed", "err", err)
}

// restartSoon restarts after the caller has answered.
// ponytail: a fixed delay, not a handshake; a response takes far less on loopback.
func restartSoon() {
	slog.Info("restarting")
	time.AfterFunc(300*time.Millisecond, restart)
}

func fatal(err error) {
	slog.Error("waterboy", "err", err)
	os.Exit(1)
}
