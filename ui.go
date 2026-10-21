package main

import (
	"embed"
	"fmt"
	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"io/fs"
	"log/slog"
	"os/exec"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed frontend
var frontend embed.FS

// UI is what the window's JS can call (as "main.UI.<Method>"). It only wraps
// app.Service, so Run/OnChange stay out of the frontend's reach.
type UI struct{ svc *app.Service }

func (u *UI) Status() app.Status   { return u.svc.Status() }
func (u *UI) RunNow()              { u.svc.RunNow() }
func (u *UI) Logs() []app.LogEntry { return u.svc.Logs() }

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
		Assets:   application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac:      application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})

	win := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "waterboy", Width: 900, Height: 600, Hidden: true, URL: "/",
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
	menu.Add("Run now").OnClick(func(*application.Context) { svc.RunNow() })
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
		go func() {
			defer cancel()
			for {
				select {
				case e := <-logs:
					wapp.Event.Emit("log", e)
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
	case st.Running:
		return "Running… · " + st.Summary()
	case st.LastError != "":
		return fmt.Sprintf("Last run %s failed: %.60s", st.LastRun.Format("15:04"), st.LastError)
	}
	return fmt.Sprintf("Last run %s · %s", st.LastRun.Format("15:04"), st.Summary())
}
