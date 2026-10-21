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
	cfg.ReposRoot = expandHome(cfg.ReposRoot)
	cfg.StateFile = expandHome(cfg.StateFile)
	return cfg, nil
}

func expandHome(p string) string {
	if !strings.HasPrefix(p, "~/") {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, p[2:])
}
