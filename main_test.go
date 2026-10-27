package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/testkit"
	"github.com/reneruck/sisyphus/internal/tracker"
)

func TestCheckTools(t *testing.T) {
	if err := checkTools("sh"); err != nil {
		t.Fatalf("sh should resolve: %v", err)
	}
	t.Setenv("PATH", t.TempDir())
	if err := checkTools("gh"); err == nil || !strings.Contains(err.Error(), "gh") {
		t.Fatalf("got %v", err)
	}
}

func TestResetState(t *testing.T) {
	b := github.PRRef{Repo: "ethyca/b", Number: 2}
	cfg, tr := testkit.Tracker(t, &github.Fake{}, &tracker.Tracked{PRRef: testkit.RefA}, &tracker.Tracked{PRRef: b})
	rt := &container.Fake{}
	rev, err := review.New(config.Review{MaxConcurrent: 1}, rt, tr.Locks, filepath.Dir(cfg.StateFile))
	if err != nil {
		t.Fatal(err)
	}
	rt.Seed(map[string]string{"sisyphus": "1", "sisyphus.pr": testkit.RefA.Key()}, true, 0, "")
	keep := rt.Seed(map[string]string{"sisyphus": "1", "sisyphus.pr": b.Key()}, true, 0, "")
	ctx := context.Background()

	if err := resetState(ctx, tr, rev, testkit.RefA.Key()); err != nil {
		t.Fatal(err)
	}
	if snap := tr.Snapshot(); len(snap) != 1 || snap[0].Key() != b.Key() {
		t.Fatalf("tracked after reset of a: %+v", snap)
	}
	if cs, _ := rt.List(ctx, map[string]string{"sisyphus": "1"}); len(cs) != 1 || cs[0].ID != keep {
		t.Fatalf("containers after reset of a: %+v", cs)
	}
	if err := resetState(ctx, tr, rev, "all"); err != nil {
		t.Fatal(err)
	}
	if cs, _ := rt.List(ctx, nil); len(tr.Snapshot()) != 0 || len(cs) != 0 {
		t.Fatalf("after reset all: %+v, containers %+v", tr.Snapshot(), cs)
	}
	reloaded, _ := tracker.LoadState(cfg.StateFile)
	if len(reloaded.PRs) != 0 {
		t.Fatalf("state file still has %+v", reloaded.PRs)
	}
}
