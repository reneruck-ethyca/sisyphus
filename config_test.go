package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := LoadConfig(filepath.Join(home, "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Org != "ethyca" || cfg.Interval.Duration != 10*time.Minute || cfg.HookTimeout.Duration != 30*time.Minute {
		t.Errorf("bad defaults: %+v", cfg)
	}
	if cfg.ReposRoot != home+"/Dev" || cfg.StateFile != home+"/.local/state/waterboy/state.json" {
		t.Errorf("bad paths: %+v", cfg)
	}
	if cfg.IncludeDrafts || cfg.User != "" {
		t.Errorf("bad defaults: %+v", cfg)
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"interval":"2m","repos_root":"~/src","include_drafts":true,"hooks":{"on_conflict":"echo hi"}}`), 0o644)
	cfg, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Duration != 2*time.Minute || cfg.ReposRoot != home+"/src" || !cfg.IncludeDrafts ||
		cfg.Hooks["on_conflict"] != "echo hi" || cfg.Org != "ethyca" {
		t.Errorf("bad overrides: %+v", cfg)
	}
}

func TestLoadConfigBadInterval(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"interval":"soon"}`), 0o644)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("want error")
	}
}
