package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadConfigDefaults(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(filepath.Join(home, "nope.json"))
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
	if r := cfg.Review; r.Disabled || r.Image == "" || len(r.Command) == 0 || r.MaxConcurrent != 1 || !r.Cleanup || r.Timeout.Duration != 30*time.Minute {
		t.Errorf("bad review defaults: %+v", r)
	}
}

func TestLoadConfigReview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"review": {"image": "me/reviewer:1", "cleanup": false, "env_file": "~/.waterboy.env"}}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Review
	if r.Image != "me/reviewer:1" || r.Cleanup || r.MaxConcurrent != 1 || len(r.Command) == 0 || r.EnvFile != home+"/.waterboy.env" {
		t.Errorf("review = %+v", r)
	}
	os.WriteFile(p, []byte(`{"review": {"max_concurrent": 0}}`), 0o644)
	if _, err := Load(p); err == nil {
		t.Error("max_concurrent 0 should be rejected")
	}
}

func TestLoadConfigOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"interval":"2m","repos_root":"~/src","include_drafts":true,"hooks":{"on_conflict":"echo hi"}}`), 0o644)
	cfg, err := Load(p)
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
	if _, err := Load(p); err == nil {
		t.Fatal("want error")
	}
}
