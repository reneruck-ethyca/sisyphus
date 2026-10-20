package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSyncWithRetry(t *testing.T) {
	conflict := &ConflictError{Reason: "dirty"}
	boom := errors.New("boom")
	cases := []struct {
		name         string
		results      []error // per sync call; last repeats
		nilHook      bool
		wantErr      error
		syncs, hooks int
	}{
		{"ok first", []error{nil}, false, nil, 1, 0},
		{"conflict twice then ok", []error{conflict, conflict, nil}, false, nil, 3, 2},
		{"always conflict", []error{conflict}, false, conflict, 4, 3},
		{"non-conflict error", []error{boom}, false, boom, 1, 0},
		{"no hook", []error{conflict}, true, conflict, 1, 0},
	}
	for _, c := range cases {
		syncs, hooks := 0, 0
		sync := func() error {
			r := c.results[min(syncs, len(c.results)-1)]
			syncs++
			return r
		}
		hook := func(string) error { hooks++; return nil }
		if c.nilHook {
			hook = nil
		}
		err := syncWithRetry(sync, hook, maxHookRuns)
		if !errors.Is(err, c.wantErr) || syncs != c.syncs || hooks != c.hooks {
			t.Errorf("%s: err=%v syncs=%d hooks=%d", c.name, err, syncs, hooks)
		}
	}
}

func TestRunHookEnvAndDir(t *testing.T) {
	dir := t.TempDir()
	err := runHook(context.Background(), `printf %s "$WATERBOY_PR_URL" > out`, time.Minute, dir,
		map[string]string{"WATERBOY_PR_URL": "https://x/pull/1"})
	b, _ := os.ReadFile(filepath.Join(dir, "out"))
	if err != nil || string(b) != "https://x/pull/1" {
		t.Fatalf("err=%v out=%q", err, b)
	}
}

func TestRunHookTimeout(t *testing.T) {
	start := time.Now()
	if err := runHook(context.Background(), "sleep 5", 100*time.Millisecond, t.TempDir(), nil); err == nil {
		t.Fatal("want timeout error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("timeout not enforced")
	}
}
