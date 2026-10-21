package main

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

func TestUI(t *testing.T) {
	tr := newTestTracker(t, &fakeGitHub{}, &Tracked{PRRef: refA, HeadSHA: "x", PendingUpdates: true})
	ui := &UI{NewService(tr.cfg, tr, NewLogBuffer(10))}

	if st := ui.Status(); len(st.PRs) != 1 || st.PRs[0].ID != refA.Key() {
		t.Fatalf("Status() = %+v", st)
	}
	if pr, err := ui.PR(refA.Key()); err != nil || pr.Health != "pending" {
		t.Fatalf("PR(a) = %+v, %v", pr, err)
	}
	if _, err := ui.PR("ethyca/nope#1"); err == nil {
		t.Fatal("PR(unknown) should fail")
	}
	slog.New(ui.svc.logs.Handler(slog.NewTextHandler(io.Discard, nil))).Info("hello")
	if logs := ui.Logs(); len(logs) != 1 || logs[0].Msg != "hello" {
		t.Fatalf("Logs() = %+v", logs)
	}
	ui.RunNow()
	select {
	case <-ui.svc.runNow:
	case <-time.After(time.Second):
		t.Fatal("RunNow didn't queue a tick")
	}
}

func TestTrayStatus(t *testing.T) {
	at := time.Date(2026, 1, 1, 9, 5, 0, 0, time.Local)
	for want, st := range map[string]Status{
		"Running… · 0 tracked · 0 pending":       {Running: true},
		"Last run 09:05 failed: boom":            {LastRun: at, LastError: "boom"},
		"Last run 09:05 · 0 tracked · 0 pending": {LastRun: at},
	} {
		if got := trayStatus(st); got != want {
			t.Errorf("trayStatus = %q, want %q", got, want)
		}
	}
}
