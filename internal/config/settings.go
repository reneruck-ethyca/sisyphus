package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/reneruck/sisyphus/internal/jsonfile"
)

// Field is a setting the UIs edit, named by its key in the config file. The rest
// (repos, hooks, env, state_file, http_addr) stays file-only.
type Field struct {
	Key     string   `json:"key"`  // "review.model"
	Type    string   `json:"type"` // "text" | "number" | "bool"; durations are text ("10m")
	Help    string   `json:"help"`
	Value   any      `json:"value"`             // the file's value, else the default
	Group   string   `json:"group"`             // the UI section it is shown in
	Label   string   `json:"label"`             // what the UI calls it
	Options []string `json:"options,omitempty"` // a text field's allowed (or suggested, with Suggest) values
	Suggest bool     `json:"suggest,omitempty"` // Options are suggestions; any value goes
	Invert  bool     `json:"invert,omitempty"`  // a bool the UI shows the other way round ("disabled" as "Run reviews")
}

// Field groups, in the order the UIs show them.
var Groups = []string{"GitHub", "Reviews", "Local stack", "Advanced"}

var fields = []Field{
	{Key: "user", Type: "text", Group: "GitHub", Label: "GitHub user", Help: "Whose review requests are tracked. Empty uses the gh login."},
	{Key: "org", Type: "text", Group: "GitHub", Label: "Organisation", Help: "Owner searched for PRs."},
	{Key: "interval", Type: "text", Group: "GitHub", Label: "Check every", Help: "Time between checks, e.g. 10m."},
	{Key: "include_drafts", Type: "bool", Group: "GitHub", Label: "Include draft PRs", Help: "Also track PRs still in draft."},
	{Key: "review.disabled", Type: "bool", Group: "Reviews", Label: "Run reviews", Invert: true, Help: "Off keeps worktrees in sync without reviewing."},
	{Key: "review.post", Type: "bool", Group: "Reviews", Label: "Post reviews to GitHub", Help: "Off is a dry run: reviews stay local."},
	{Key: "review.model", Type: "text", Group: "Reviews", Label: "Model", Options: []string{"opus", "sonnet", "haiku"}, Suggest: true, Help: "Claude model used for reviews."},
	{Key: "review.max_turns", Type: "number", Group: "Reviews", Label: "Turn limit", Help: "Hitting it fails the run."},
	{Key: "review.max_budget_usd", Type: "number", Group: "Reviews", Label: "Spend limit (USD)", Help: "0 means no limit. Only enforced with API-key auth."},
	{Key: "review.timeout", Type: "text", Group: "Reviews", Label: "Flag runs after", Help: "Longer runs are flagged, never stopped. E.g. 1h."},
	{Key: "review.max_concurrent", Type: "number", Group: "Reviews", Label: "Reviews at a time", Help: "Review containers running at once."},
	{Key: "review.five_hour_percent", Type: "number", Group: "Reviews", Label: "Share of 5-hour limit (%)", Help: "How much of each 5-hour subscription window reviews may use. Past it, new reviews wait for the window to reset. 0 means no limit."},
	{Key: "review.retries", Type: "number", Group: "Reviews", Label: "Retries", Help: "Re-runs of a review that failed before Claude started (setup, stack). 0 means none."},
	{Key: "review.retry_backoff", Type: "text", Group: "Reviews", Label: "Retry backoff", Help: "Wait before the first retry, doubling after each, e.g. 2m."},
	{Key: "review.stack_from", Type: "text", Group: "Local stack", Label: "Start stacks from", Options: []string{"worktree", "safe"}, Help: "worktree: the PR's code, on the host. safe: the clone in the repos root."},
	{Key: "review.setup_step_timeout", Type: "text", Group: "Local stack", Label: "Setup step timeout", Help: "Each setup step (uv sync, npm install…) is killed past this, failing the run. E.g. 20m."},
	{Key: "review.cleanup", Type: "bool", Group: "Local stack", Label: "Reset worktrees after each run", Help: "Only worktrees sisyphus created."},
	{Key: "repos_root", Type: "text", Group: "Advanced", Label: "Repos root", Help: "Clones live at <repos root>/<repo>."},
	{Key: "hook_timeout", Type: "text", Group: "Advanced", Label: "Hook timeout", Help: "Per hook run, e.g. 30m."},
	{Key: "review.env_file", Type: "text", Group: "Advanced", Label: "Env file", Help: "Holds CLAUDE_CODE_OAUTH_TOKEN and a read-only GH_TOKEN."},
	{Key: "review.prompt_file", Type: "text", Group: "Advanced", Label: "Prompt file", Help: "Go text/template prompt. Empty uses the built-in one."},
	{Key: "review.claude_settings", Type: "text", Group: "Advanced", Label: "Claude settings", Help: "settings.json given to Claude in each run."},
	{Key: "review.plugins_volume", Type: "text", Group: "Advanced", Label: "Plugins volume", Help: "Docker volume mounted as Claude's plugins/."},
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
