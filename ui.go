package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os/exec"

	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/review"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed frontend
var frontend embed.FS

// UI is what the window's JS can call (as "main.UI.<Method>"). It only wraps
// app.Service, so Run/OnChange stay out of the frontend's reach.
type UI struct{ svc *app.Service }

func (u *UI) Status() app.Status { return u.svc.Status() }
func (u *UI) RunNow()            { u.svc.RunNow() }

// Stack is what's on Docker for a PR's local stack (null = its repo has none).
func (u *UI) Stack(id string) (*review.StackStatus, error) {
	return u.svc.Stack(context.Background(), id)
}

// The PR page's … menu (see app.Service for what each removes).
func (u *UI) ResetReviews(id string) error { return u.svc.ResetReviews(context.Background(), id) }
func (u *UI) Wipe(id string) error         { return u.svc.Wipe(context.Background(), id) }
func (u *UI) Untrack(id string) error      { return u.svc.Untrack(context.Background(), id) }
func (u *UI) Retrack(id string) error      { return u.svc.Retrack(context.Background(), id) }

// ReviewNow queues a full review of a tracked PR.
func (u *UI) ReviewNow(id string) error { return u.svc.ReviewNow(id) }
func (u *UI) Logs() []app.LogEntry      { return u.svc.Logs() }

func (u *UI) Settings() (app.Settings, error)          { return u.svc.Settings() }
func (u *UI) SaveSettings(values map[string]any) error { return u.svc.SaveSettings(values) }

// Restart re-execs waterboy; the new one starts with its window hidden.
func (u *UI) Restart() { restartSoon() }

// RunOutput is a run's readable output so far; live "run-event" events with
// line >= Next continue it.
type RunOutput struct {
	Events []review.Event `json:"events"`
	Next   int            `json:"next"`
}

// RunEvents returns a review run's readable output so far.
func (u *UI) RunEvents(prID, runID string) (RunOutput, error) {
	evs, next, err := u.svc.RunEvents(prID, runID)
	return RunOutput{Events: evs, Next: next}, err
}

// OpenRun opens a review run's directory (log, run.json, out/) in Finder.
func (u *UI) OpenRun(prID, runID string) error {
	dir, ok := u.svc.RunDir(prID, runID)
	if !ok {
		return fmt.Errorf("no run %s for %s", runID, prID)
	}
	return exec.Command("open", dir).Run()
}

// ReviewText is the review a run's agent wrote ("" if it wrote none).
func (u *UI) ReviewText(prID, runID string) (string, error) {
	f, err := u.svc.ReviewFile(prID, runID)
	if f == nil {
		return "", err
	}
	return f.Body, nil
}

// wailsAssets serves the window's files plus the stylesheet it shares with the web dashboard.
func wailsAssets(files fs.FS) http.Handler {
	h := application.AssetFileServerFS(files)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/style.css" {
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			w.Write(styleCSS)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func (u *UI) PR(id string) (app.PRView, error) {
	pr, ok := u.svc.PR(id)
	if !ok {
		return app.PRView{}, fmt.Errorf("%s is not tracked", id)
	}
	return pr, nil
}

// runApp owns the macOS main thread: menu-bar item, window, and the tick loop.
func runApp(cfg config.Config, svc *app.Service) error {
	assets, _ := fs.Sub(frontend, "frontend") // can't fail: "frontend" is a valid, embedded path
	wapp := application.New(application.Options{
		Name:     "waterboy",
		Services: []application.Service{application.NewService(&UI{svc})},
		Assets:   application.AssetOptions{Handler: wailsAssets(assets)},
		Mac:      application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})

	win := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "waterboy", Width: 1100, Height: 760, Hidden: true, URL: "/",
	})
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide() // keep it around for "Open window"
		e.Cancel()
	})
	openWindow := func() { win.Show(); win.Focus() }

	menu := wapp.NewMenu()
	status := menu.Add("Starting…").SetEnabled(false)
	menu.Add("Open window").OnClick(func(*application.Context) { openWindow() })
	dashboard := menu.Add("Open dashboard")
	if dashURL, err := startHTTP(cfg.HTTPAddr, svc); err != nil {
		slog.Error("dashboard disabled", "addr", cfg.HTTPAddr, "err", err)
		dashboard.SetEnabled(false)
	} else {
		slog.Info("dashboard", "url", dashURL)
		dashboard.OnClick(func(*application.Context) {
			if err := exec.Command("open", dashURL).Run(); err != nil {
				slog.Warn("opening dashboard", "err", err)
			}
		})
	}
	menu.Add("Check now").OnClick(func(*application.Context) { svc.RunNow() })
	menu.Add("Open state file").OnClick(func(*application.Context) {
		if err := exec.Command("open", cfg.StateFile).Run(); err != nil {
			slog.Warn("opening state file", "err", err)
		}
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) { wapp.Quit() })

	tray := wapp.SystemTray.New()
	tray.SetLabel("WB")
	tray.SetTooltip("waterboy")
	tray.SetMenu(menu)

	svc.OnChange(func(st app.Status) {
		status.SetLabel(trayStatus(st))
		wapp.Event.Emit("status", st)
	})
	wapp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		logs, cancel := svc.SubscribeLogs()
		runs, cancelRuns := svc.SubscribeRunEvents()
		go func() {
			defer cancel()
			defer cancelRuns()
			for {
				select {
				case e := <-logs:
					wapp.Event.Emit("log", e)
				case e := <-runs:
					wapp.Event.Emit("run-event", e)
				case <-wapp.Context().Done():
					return
				}
			}
		}()
		go svc.Run(wapp.Context())
	})
	return wapp.Run()
}

func trayStatus(st app.Status) string {
	switch {
	case len(st.Reviewing) > 0:
		return "Reviewing " + st.Reviewing[0].PR + " · " + st.Summary()
	case st.Running:
		return "Running… · " + st.Summary()
	case st.LastError != "":
		return fmt.Sprintf("Last run %s failed: %.60s", st.LastRun.Format("15:04"), st.LastError)
	}
	return fmt.Sprintf("Last run %s · %s", st.LastRun.Format("15:04"), st.Summary())
}
