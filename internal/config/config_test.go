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
	r := cfg.Review
	if r.Disabled || r.Model != "opus" || r.MaxTurns != 100 || r.MaxConcurrent != 1 || !r.Cleanup || r.Timeout.Duration != 60*time.Minute ||
		r.EnvFile != home+"/.config/waterboy/review.env" {
		t.Errorf("bad review defaults: %+v", r)
	}
	if fp := r.Repos["ethyca/fidesplus"]; fp.Image == "" || fp.Workdir != "/fidesplus" || fp.Setup == "" || len(fp.Cache) == 0 ||
		fp.EnvFile != home+"/Dev/fidesplus/.devcontainer/devcontainer.env" {
		t.Errorf("bad fidesplus defaults: %+v", fp)
	}
}

func TestLoadConfigReview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"review": {"model": "sonnet", "cleanup": false, "env_file": "~/.waterboy.env",
		"repos": {"ethyca/fides": {"image": "me/fides-reviewer:1", "prompt_file": "~/fides.md"}}}}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Review
	if r.Model != "sonnet" || r.Cleanup || r.MaxConcurrent != 1 || r.EnvFile != home+"/.waterboy.env" {
		t.Errorf("review = %+v", r)
	}
	if r.Repos["ethyca/fides"].PromptFile != home+"/fides.md" || r.Repos["ethyca/fidesplus"].Image == "" {
		t.Errorf("repos = %+v (added repos merge with the defaults)", r.Repos)
	}
	for _, bad := range []string{
		`{"review": {"max_concurrent": 0}}`,
		`{"review": {"repos": {"x/y": {"image": ""}}}}`,
		`{"review": {"repos": {"x/y": {"image": "i", "cache": ["../outside"]}}}}`,
		`{"review": {"repos": {"x/y": {"image": "i", "workdir": "relative"}}}}`,
	} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
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
