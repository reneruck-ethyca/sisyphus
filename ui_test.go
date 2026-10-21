package main

import (
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/testkit"
	"github.com/reneruck/waterboy/internal/tracker"
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestUI(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{}, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x", PendingUpdates: true})
	logs := app.NewLogBuffer(10)
	ui := &UI{app.NewService(cfg, tr, logs)}

	if st := ui.Status(); len(st.PRs) != 1 || st.PRs[0].ID != testkit.RefA.Key() {
		t.Fatalf("app.Status() = %+v", st)
	}
	if pr, err := ui.PR(testkit.RefA.Key()); err != nil || pr.Health != "pending" {
		t.Fatalf("PR(a) = %+v, %v", pr, err)
	}
	if _, err := ui.PR("ethyca/nope#1"); err == nil {
		t.Fatal("PR(unknown) should fail")
	}
	slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil))).Info("hello")
	if logs := ui.Logs(); len(logs) != 1 || logs[0].Msg != "hello" {
		t.Fatalf("Logs() = %+v", logs)
	}
	ui.RunNow()
	if !ui.svc.RunQueued() {
		t.Fatal("RunNow didn't queue a tick")
	}
}

func TestTrayStatus(t *testing.T) {
	at := time.Date(2026, 1, 1, 9, 5, 0, 0, time.Local)
	for want, st := range map[string]app.Status{
		"Running… · 0 tracked · 0 pending":       {Running: true},
		"Last run 09:05 failed: boom":            {LastRun: at, LastError: "boom"},
		"Last run 09:05 · 0 tracked · 0 pending": {LastRun: at},
	} {
		if got := trayStatus(st); got != want {
			t.Errorf("trayStatus = %q, want %q", got, want)
		}
	}
}
