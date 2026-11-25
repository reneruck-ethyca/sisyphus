package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	if cfg.ReposRoot != home+"/Dev" || cfg.StateFile != home+"/.local/state/sisyphus/state.json" {
		t.Errorf("bad paths: %+v", cfg)
	}
	if cfg.IncludeDrafts || cfg.User != "" {
		t.Errorf("bad defaults: %+v", cfg)
	}
	r := cfg.Review
	if r.Disabled || !r.Post || r.Agent != (Agent{Vendor: "claude", Model: "opus", Effort: "medium"}) || r.MaxTurns != 200 || r.MaxConcurrent != 2 || !r.Cleanup || r.Timeout.Duration != 60*time.Minute ||
		r.EnvFile != home+"/.config/sisyphus/review.env" {
		t.Errorf("bad review defaults: %+v", r)
	}
	if fp := r.Repos["ethyca/fidesplus"]; fp.Image == "" || fp.Workdir != "/fidesplus" || fp.Setup == "" || len(fp.Cache) == 0 ||
		fp.EnvFile != home+"/Dev/fidesplus/.devcontainer/devcontainer.env" {
		t.Errorf("bad fidesplus defaults: %+v", fp)
	}
	if fp := r.Repos["ethyca/fidesplus"].Notes; !strings.Contains(fp, "next dev -p 3000") {
		t.Errorf("UI start notes missing: fidesplus %q", fp)
	}
	if qa := r.Repos["ethyca/fides-qa"]; qa.Workdir != "/fides-qa" || qa.Stack != nil || qa.Notes == "" ||
		qa.Related == nil || qa.Related.Clone != home+"/Dev/fidesplus" || qa.Related.Target != "/fidesplus" {
		t.Errorf("bad fides-qa defaults: %+v", qa)
	}
}

func TestLoadConfigReview(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"review": {"agent": {"vendor": "claude", "model": "sonnet", "effort": "high"}, "cleanup": false, "env_file": "~/.sisyphus.env",
		"repos": {"ethyca/fides": {"image": "me/fides-reviewer:1", "prompt_file": "~/fides.md"}}}}`), 0o644)
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	r := cfg.Review
	if r.Agent != (Agent{Vendor: "claude", Model: "sonnet", Effort: "high"}) || r.Cleanup || r.MaxConcurrent != 2 || r.EnvFile != home+"/.sisyphus.env" {
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
		`{"review": {"plugins_volume": "/Users/me"}}`, // a bind mount of the host, not a volume
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

func TestLoadConfigBadDurations(t *testing.T) {
	for _, c := range []string{`{"interval":"soon"}`, `{"interval":"0s"}`, `{"hook_timeout":"0s"}`, `{"hook_timeout":"-1m"}`} {
		p := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(p, []byte(c), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s: want error", c)
		}
	}
}

func TestSettings(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	os.WriteFile(p, []byte(`{"org": "acme", "hooks": {"on_conflict": "echo hi"},
		"review": {"agent": {"vendor": "claude", "model": "sonnet", "effort": "medium"}, "repos": {"x/y": {"image": "i"}}}}`), 0o644)
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
	if value(fs, "org") != "acme" || value(fs, "interval") != "10m" || value(fs, "review.timeout") != "1h" || value(fs, "hook_timeout") != "30m" || value(fs, "review.agent.model") != "sonnet" ||
		value(fs, "review.max_turns") != 200.0 || value(fs, "review.post") != true {
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
		cfg.Org != "acme" || cfg.Review.Agent.Model != "sonnet" || cfg.Hooks["on_conflict"] != "echo hi" || cfg.Review.Repos["x/y"].Image != "i" {
		t.Fatalf("saved config = %+v (the rest of the file must survive)", cfg)
	}

	before, _ := os.ReadFile(p)
	for _, bad := range []map[string]any{
		{"interval": "soon"},
		{"review.max_concurrent": 0}, {"review.five_hour_percent": 101},
		{"review.max_turns": "many"},
		{"state_file": "/elsewhere"},
		// Paths mounted into or copied for review containers are file-only.
		{"review.plugins_volume": "/Users/me"}, {"review.codex_home": "/Users/me"}, {"review.env_file": "/etc/passwd"}, {"review.claude_settings": "/x"},
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

func TestLoadConfigLastMile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	p := filepath.Join(home, "c.json")
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	l := cfg.LastMile
	if l.Disabled || l.Interval.Duration != 2*time.Minute || l.BotReviewer != "ethycabot" || l.Reviewers.Count != 1 ||
		l.MaxFixRounds != 5 || l.NeedsHumanLabel != "tlm:needs-human" || l.FlakeReruns != 2 || l.MaxDiffLines != 400 || len(l.Flakes) != 0 ||
		len(l.BotLogins) != 1 || l.BotLogins[0] != "ethycabot" {
		t.Errorf("bad lastmile defaults: %+v", l)
	}

	os.WriteFile(p, []byte(`{"lastmile": {"interval": "30s", "reviewers": {"roster": ["a", "b"]},
		"repo_reviewers": {"ethyca/fides-qa": {"roster": ["c"], "count": 2}}}}`), 0o644)
	if cfg, err = Load(p); err != nil {
		t.Fatal(err)
	}
	l = cfg.LastMile
	if l.Interval.Duration != 30*time.Second || l.BotReviewer != "ethycabot" || l.MaxFixRounds != 5 {
		t.Errorf("lastmile = %+v (unset keys keep their defaults)", l)
	}
	if !l.Manual("ethyca/fidesinfra") || l.Manual("ethyca/kairos") || l.Manual("ethyca/fidesplus") {
		t.Errorf("manual_merge = %v", l.ManualMerge)
	}
	if r := l.ReviewersFor("ethyca/fidesplus"); len(r.Roster) != 2 || r.Count != 1 {
		t.Errorf("global roster = %+v", r)
	}
	if r := l.ReviewersFor("ethyca/fides-qa"); len(r.Roster) != 1 || r.Count != 2 {
		t.Errorf("repo roster = %+v", r)
	}

	for _, bad := range []string{
		`{"lastmile": {"interval": "0s"}}`,
		`{"lastmile": {"max_fix_rounds": 0}}`,
		`{"lastmile": {"flakes": ["("]}}`,
		`{"lastmile": {"bot_logins": [""]}}`,
		`{"lastmile": {"max_diff_lines": 0}}`,
		`{"lastmile": {"flake_reruns": -1}}`,
		`{"lastmile": {"needs_human_label": ""}}`,
		`{"lastmile": {"reviewers": {"roster": [""]}}}`,
		`{"lastmile": {"reviewers": {"count": -1}}}`,
		`{"lastmile": {"repo_reviewers": {"fidesplus": {"roster": ["a"]}}}}`,
		`{"lastmile": {"manual_merge": ["fidesinfra"]}}`,
	} {
		os.WriteFile(p, []byte(bad), 0o644)
		if _, err := Load(p); err == nil {
			t.Errorf("%s should be rejected", bad)
		}
	}
	os.WriteFile(p, []byte(`{"lastmile": {"disabled": true, "max_fix_rounds": 0}}`), 0o644)
	if _, err := Load(p); err != nil {
		t.Errorf("a disabled section isn't checked: %v", err)
	}
}

func TestLoadMergeFreeze(t *testing.T) {
	load := func(body string) error {
		p := filepath.Join(t.TempDir(), "c.json")
		os.WriteFile(p, []byte(body), 0o644)
		_, err := Load(p)
		return err
	}
	if err := load(`{"lastmile":{"merge_freeze":[{"weekday":"Friday","start":"15:00","end":"23:00","tz":"Europe/Berlin"}]}}`); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]string{
		"crossing midnight": `{"start":"22:00","end":"06:00"}`,
		"bad clock":         `{"start":"9am","end":"10:00"}`,
		"bad weekday":       `{"weekday":"someday","start":"09:00","end":"10:00"}`,
		"bad tz":            `{"start":"09:00","end":"10:00","tz":"Mars/Base"}`,
	} {
		if err := load(`{"lastmile":{"merge_freeze":[` + w + `]}}`); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigIgnoresMaxMergeLines(t *testing.T) {
	t.Parallel()
	p := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(p, []byte(`{"lastmile":{"max_merge_lines":400}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err != nil {
		t.Fatal(err)
	}
}

func TestFidesplusScratchAndMountPoints(t *testing.T) {
	for _, f := range []string{"src/fides/service/pbac/__pycache__/engine.cpython-313.pyc", "clients/openapi.json",
		"clients/shared/node_modules/.cache/jiti/x.mjs",
		"clients/astralis/.next/dev/build/chunks/a.js", "clients/astralis/next-env.d.ts"} {
		if !slices.ContainsFunc(fidesplus.Scratch, func(g string) bool { return GlobMatch(g, f) }) {
			t.Errorf("%s not scratch", f)
		}
	}
	if got := fidesplus.MountPoints(); slices.Contains(got, "/sisyphus/cache/uv") || !slices.Contains(got, "src/fides/bin") {
		t.Errorf("MountPoints = %v", got)
	}
}
