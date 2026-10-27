package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/reneruck/waterboy/internal/jsonfile"
)

// Field is a setting the UIs edit, named by its key in the config file. The rest
// (repos, hooks, env, state_file, http_addr) stays file-only.
type Field struct {
	Key   string `json:"key"`  // "review.model"
	Type  string `json:"type"` // "text" | "number" | "bool"; durations are text ("10m")
	Help  string `json:"help"`
	Value any    `json:"value"` // the file's value, else the default
}

var fields = []Field{
	{Key: "user", Type: "text", Help: "GitHub login whose reviews are tracked; empty = gh api user"},
	{Key: "org", Type: "text", Help: "owner searched for PRs"},
	{Key: "interval", Type: "text", Help: "time between ticks (Go duration, e.g. 10m)"},
	{Key: "repos_root", Type: "text", Help: "clones live at <repos_root>/<repo>"},
	{Key: "include_drafts", Type: "bool", Help: "also track draft PRs"},
	{Key: "hook_timeout", Type: "text", Help: "per hook run"},
	{Key: "review.disabled", Type: "bool", Help: "don't run reviews"},
	{Key: "review.post", Type: "bool", Help: "post the agent's review to GitHub; off = dry run"},
	{Key: "review.model", Type: "text", Help: "Claude model"},
	{Key: "review.max_turns", Type: "number", Help: "Claude turn limit; hitting it fails the run"},
	{Key: "review.max_budget_usd", Type: "number", Help: "spend limit, 0 = none (only enforced with API-key auth)"},
	{Key: "review.timeout", Type: "text", Help: "runs longer than this are flagged, never stopped"},
	{Key: "review.max_concurrent", Type: "number", Help: "review containers at a time"},
	{Key: "review.cleanup", Type: "bool", Help: "reset worktrees waterboy created after each run"},
	{Key: "review.stack_from", Type: "text", Help: "where local stacks start: worktree (the PR's code, on the host) or safe (repos_root clone)"},
	{Key: "review.setup_step_timeout", Type: "text", Help: "each setup step (uv sync, npm install, …); past it the step is killed and the run fails"},
	{Key: "review.retries", Type: "number", Help: "re-runs of a review that failed before Claude started (setup, stack); 0 = none"},
	{Key: "review.retry_backoff", Type: "text", Help: "wait before the first retry, doubling after each (e.g. 2m)"},
	{Key: "review.env_file", Type: "text", Help: "CLAUDE_CODE_OAUTH_TOKEN and a read-only GH_TOKEN"},
	{Key: "review.prompt_file", Type: "text", Help: "Go text/template prompt; empty = built-in"},
	{Key: "review.claude_settings", Type: "text", Help: "settings.json given to Claude in each run"},
	{Key: "review.plugins_volume", Type: "text", Help: "Docker volume mounted as Claude's plugins/"},
}

// Settings returns the editable fields as path (over the defaults) has them now.
func Settings(path string) ([]Field, error) {
	cfg, err := read(path)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	out := slices.Clone(fields)
	for i := range out {
		sec, name := section(m, out[i].Key)
		out[i].Value = sec[name]
	}
	return out, nil
}

// SaveSettings writes values (field key → value) into the config file, keeping
// everything else in it, once the result checks out. They apply on restart.
func SaveSettings(path string, values map[string]any) error {
	var m map[string]any
	if err := jsonfile.Load(path, &m); err != nil {
		return err
	}
	if m == nil {
		m = map[string]any{}
	}
	for k, v := range values {
		if !slices.ContainsFunc(fields, func(f Field) bool { return f.Key == k }) {
			return fmt.Errorf("%s isn't an editable setting", k)
		}
		sec, name := section(m, k)
		sec[name] = v
	}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if _, err := parse(path, b); err != nil {
		return err
	}
	return jsonfile.Save(path, m)
}

// section returns the object holding key: "review.model" → m["review"], "model".
// ponytail: one level of nesting, as deep as fields go.
func section(m map[string]any, key string) (map[string]any, string) {
	sec, name, ok := strings.Cut(key, ".")
	if !ok {
		return m, key
	}
	sub, _ := m[sec].(map[string]any)
	if sub == nil {
		sub = map[string]any{}
		m[sec] = sub
	}
	return sub, name
}
