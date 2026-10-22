package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Duration is a time.Duration that reads from JSON strings like "10m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

type Config struct {
	User          string            `json:"user"` // empty = `gh api user`
	Org           string            `json:"org"`
	Interval      Duration          `json:"interval"`
	ReposRoot     string            `json:"repos_root"`
	StateFile     string            `json:"state_file"`
	IncludeDrafts bool              `json:"include_drafts"`
	Hooks         map[string]string `json:"hooks"` // "on_conflict": "<shell cmd>"
	HookTimeout   Duration          `json:"hook_timeout"`
	HTTPAddr      string            `json:"http_addr"` // dashboard + JSON API
	Review        Review            `json:"review"`
}

// Review configures the Claude Code review runs.
type Review struct {
	Disabled       bool                  `json:"disabled"`
	Post           bool                  `json:"post"` // post the agent's review to GitHub (false = dry run)
	Model          string                `json:"model"`
	MaxTurns       int                   `json:"max_turns"`
	MaxBudgetUSD   float64               `json:"max_budget_usd"`  // 0 = no limit (only enforced with API-key auth)
	PromptFile     string                `json:"prompt_file"`     // Go text/template; "" = built-in prompt
	Env            []string              `json:"env"`             // names copied from waterboy's own environment
	EnvFile        string                `json:"env_file"`        // CLAUDE_CODE_OAUTH_TOKEN, GH_TOKEN, …
	ClaudeSettings string                `json:"claude_settings"` // settings.json given to the agent ("" = none)
	PluginsVolume  string                `json:"plugins_volume"`  // Docker volume with Claude plugins ("" = none)
	Timeout        Duration              `json:"timeout"`         // runs past this are flagged, never stopped
	MaxConcurrent  int                   `json:"max_concurrent"`
	Cleanup        bool                  `json:"cleanup"` // reset worktrees waterboy created after each run
	Repos          map[string]RepoReview `json:"repos"`   // only these repos are reviewed
}

// RepoReview is how one repo's PRs are reviewed.
type RepoReview struct {
	Image      string            `json:"image"`
	Workdir    string            `json:"workdir"` // container path for the worktree; "" = its host path
	EnvFile    string            `json:"env_file"`
	Env        map[string]string `json:"env"`
	Setup      string            `json:"setup"` // shell, run in workdir before Claude starts
	Cache      []string          `json:"cache"` // workdir-relative dirs kept in per-repo volumes
	PromptFile string            `json:"prompt_file"`
}

// fidesplus mirrors its devcontainer (.devcontainer/devcontainer.json, post-create.sh).
var fidesplus = RepoReview{
	Image:   "waterboy-reviewer:fidesplus",
	Workdir: "/fidesplus",
	EnvFile: "~/Dev/fidesplus/.devcontainer/devcontainer.env",
	Env:     map[string]string{"PYTHONPATH": "/fidesplus/src", "RUNNING_IN_DOCKER": "TRUE"},
	Setup: `uv sync --all-groups
uv pip install --no-deps -e .
cp -f /usr/local/share/fides/libpbac.so src/fides/bin/libpbac.so
(cd src/fidesplus/gpp_js_integration && npm install && npm run build)
(cd clients && npm install)`,
	Cache: []string{".venv", "src/fides/bin", "src/fidesplus/gpp_js_integration/node_modules",
		"src/fidesplus/gpp_js_integration/compiled", "clients/node_modules"},
}

func DefaultPath() string {
	return expandHome("~/.config/waterboy/config.json")
}

// Load reads path over the defaults. A missing file means all defaults.
func Load(path string) (Config, error) {
	cfg := Config{
		Org:         "ethyca",
		Interval:    Duration{10 * time.Minute},
		ReposRoot:   "~/Dev",
		StateFile:   "~/.local/state/waterboy/state.json",
		HookTimeout: Duration{30 * time.Minute},
		HTTPAddr:    "127.0.0.1:7465",
		Review: Review{
			Post:          true,
			Model:         "opus",
			MaxTurns:      100,
			EnvFile:       "~/.config/waterboy/review.env",
			PluginsVolume: "waterboy-claude-plugins",
			Timeout:       Duration{60 * time.Minute},
			MaxConcurrent: 1,
			Cleanup:       true,
			Repos:         map[string]RepoReview{"ethyca/fidesplus": fidesplus},
		},
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return cfg, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
	}
	if cfg.Interval.Duration <= 0 {
		return cfg, fmt.Errorf("config %s: interval must be positive", path)
	}
	if err := cfg.Review.check(); err != nil {
		return cfg, fmt.Errorf("config %s: review: %w", path, err)
	}
	cfg.ReposRoot = expandHome(cfg.ReposRoot)
	r := &cfg.Review
	r.EnvFile, r.PromptFile, r.ClaudeSettings = expandHome(r.EnvFile), expandHome(r.PromptFile), expandHome(r.ClaudeSettings)
	for name, repo := range r.Repos {
		repo.EnvFile, repo.PromptFile = expandHome(repo.EnvFile), expandHome(repo.PromptFile)
		r.Repos[name] = repo
	}
	cfg.StateFile = expandHome(cfg.StateFile)
	return cfg, nil
}

func (r Review) check() error {
	if r.Disabled {
		return nil
	}
	if r.Model == "" || r.MaxTurns < 1 || r.Timeout.Duration <= 0 || r.MaxConcurrent < 1 {
		return errors.New("needs a model, max_turns >= 1, a positive timeout and max_concurrent >= 1")
	}
	for name, repo := range r.Repos {
		if repo.Image == "" {
			return fmt.Errorf("repos[%q] needs an image", name)
		}
		if repo.Workdir != "" && !filepath.IsAbs(repo.Workdir) {
			return fmt.Errorf("repos[%q].workdir must be absolute", name)
		}
		for _, c := range repo.Cache {
			if !filepath.IsLocal(c) {
				return fmt.Errorf("repos[%q].cache %q must be a path inside the worktree", name, c)
			}
		}
	}
	return nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, p[2:])
}
