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
	if r.Disabled || !r.Post || r.Model != "opus" || r.MaxTurns != 200 || r.MaxConcurrent != 1 || !r.Cleanup || r.Timeout.Duration != 60*time.Minute ||
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

func TestSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"org": "acme", "hooks": {"on_conflict": "echo hi"},
		"review": {"model": "sonnet", "repos": {"x/y": {"image": "i"}}}}`), 0o644)
	value := func(fs []Field, key string) any {
		for _, f := range fs {
			if f.Key == key {
				return f.Value
			}
		}
		t.Fatalf("no field %s", key)
		return nil
	}
	fs, err := Settings(p)
	if err != nil {
		t.Fatal(err)
	}
	if value(fs, "org") != "acme" || value(fs, "interval") != "10m" || value(fs, "review.timeout") != "1h" || value(fs, "hook_timeout") != "30m" || value(fs, "review.model") != "sonnet" ||
		value(fs, "review.max_turns") != 200.0 || value(fs, "review.post") != true || value(fs, "review.env_file") != "~/.config/waterboy/review.env" {
		t.Fatalf("settings = %+v", fs)
	}

	if err := SaveSettings(p, map[string]any{"interval": "5m", "review.max_turns": 7, "include_drafts": true}); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Interval.Duration != 5*time.Minute || cfg.Review.MaxTurns != 7 || !cfg.IncludeDrafts ||
		cfg.Org != "acme" || cfg.Review.Model != "sonnet" || cfg.Hooks["on_conflict"] != "echo hi" || cfg.Review.Repos["x/y"].Image != "i" {
		t.Fatalf("saved config = %+v (the rest of the file must survive)", cfg)
	}

	before, _ := os.ReadFile(p)
	for _, bad := range []map[string]any{
		{"interval": "soon"},
		{"review.max_concurrent": 0}, {"review.five_hour_percent": 101},
		{"review.max_turns": "many"},
		{"state_file": "/elsewhere"},
	} {
		if err := SaveSettings(p, bad); err == nil {
			t.Errorf("%v should be rejected", bad)
		}
	}
	if after, _ := os.ReadFile(p); string(after) != string(before) {
		t.Fatalf("a rejected save changed the file:\n%s", after)
	}

	missing := filepath.Join(home, "new", "c.json")
	if err := SaveSettings(missing, map[string]any{"org": "x"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := Load(missing); cfg.Org != "x" {
		t.Fatalf("new file: org = %q", cfg.Org)
	}
}

func TestLoadConfigStack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfg, err := Load(filepath.Join(home, "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	st := cfg.Review.Repos["ethyca/fidesplus"].Stack
	if cfg.Review.StackFrom != StackFromWorktree || st == nil || st.From != "" || st.SafeDir != home+"/Dev/fidesplus" ||
		st.Up != "uv run nox -s dev_datastores" || st.Project != "fidesplus-{id}" {
		t.Fatalf("stack defaults: %q %+v", cfg.Review.StackFrom, st)
	}
	if fidesplus.Stack.SafeDir != "" {
		t.Fatal("Load changed the built-in defaults")
	}
	for _, bad := range []string{
		`{"review": {"stack_from": "yolo"}}`,
		`{"review": {"repos": {"o/r": {"image": "i", "stack": {"from": "x", "up": "u", "project": "p"}}}}}`,
		`{"review": {"repos": {"o/r": {"image": "i", "stack": {"up": "u"}}}}}`,
	} {
		p := filepath.Join(home, "c.json")
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}
