package main

import (
	_ "embed"
	"fmt"
	"log/slog"
	"os/exec"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

// windowUserAgent marks the window's requests, so the pages know to open
// external links through /api/open instead of navigating the webview away.
const windowUserAgent = "sisyphus-window"

// trayIcon is 44px so it stays sharp at the 22pt menu-bar height on Retina.
//
//go:embed trayicon.png
var trayIcon []byte

// runApp owns the macOS main thread: menu-bar item, window, and the tick loop.
// The window shows the dashboard; there is no second UI.
func runApp(cfg config.Config, svc *app.Service) error {
	dashURL, err := startHTTP(cfg.HTTPAddr, svc)
	if err != nil {
		return fmt.Errorf("dashboard: %w", err)
	}
	slog.Info("dashboard", "url", dashURL)

	wapp := application.New(application.Options{
		Name: "sisyphus",
		Mac:  application.MacOptions{ActivationPolicy: application.ActivationPolicyAccessory},
	})
	win := wapp.Window.NewWithOptions(application.WebviewWindowOptions{
		Title: "sisyphus", Width: 1100, Height: 760, Hidden: true, URL: dashURL,
		Mac: application.MacWindow{WebviewPreferences: application.MacWebviewPreferences{ApplicationNameForUserAgent: windowUserAgent}},
	})
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		win.Hide() // keep it around for "Open window"
		e.Cancel()
	})

	menu := wapp.NewMenu()
	status := menu.Add("Starting…").SetEnabled(false)
	menu.Add("Open window").OnClick(func(*application.Context) { win.Show(); win.Focus() })
	menu.Add("Open dashboard").OnClick(func(*application.Context) {
		if err := openInBrowser(dashURL); err != nil {
			slog.Warn("opening dashboard", "err", err)
		}
	})
	menu.Add("Check now").OnClick(func(*application.Context) { svc.RunNow() })
	menu.Add("Open state file").OnClick(func(*application.Context) {
		if err := exec.Command("open", cfg.StateFile).Run(); err != nil {
			slog.Warn("opening state file", "err", err)
		}
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) { wapp.Quit() })

	tray := wapp.SystemTray.New()
	tray.SetTemplateIcon(trayIcon)
	tray.SetTooltip("sisyphus")
	tray.SetMenu(menu)

	svc.OnChange(func(st app.Status) { status.SetLabel(trayStatus(st)) })
	wapp.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
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
