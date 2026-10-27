package main

import (
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/review"
)

func TestTrayStatus(t *testing.T) {
	at := time.Date(2026, 1, 1, 9, 5, 0, 0, time.Local)
	for want, st := range map[string]app.Status{
		"Running… · 0 tracked":        {Running: true},
		"Last run 09:05 failed: boom": {LastRun: at, LastError: "boom"},
		"Last run 09:05 · 0 tracked":  {LastRun: at},
		"Reviewing ethyca/a#1 · 1 tracked · 1 review running": {LastRun: at, Reviewing: []app.ActiveReview{{PR: "ethyca/a#1"}},
			PRs: []app.PRView{{Review: review.PRStatus{State: "running"}}}},
	} {
		if got := trayStatus(st); got != want {
			t.Errorf("trayStatus = %q, want %q", got, want)
		}
	}
}
