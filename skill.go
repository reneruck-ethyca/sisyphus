package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
)

// handoffSkill teaches coding agents when and how to use `tlm handoff`. It ships
// in the binary, so `tlm install-skill` after a rebuild brings agents up to date.
//
//go:embed skills/handing-off-to-tlm/SKILL.md
var handoffSkill []byte

const skillName = "handing-off-to-tlm"

// skillHomes are where each agent CLI looks for personal skills, honouring
// their config-dir overrides.
func skillHomes() map[string]string {
	home, _ := os.UserHomeDir()
	or := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return filepath.Join(home, def)
	}
	return map[string]string{
		"claude": filepath.Join(or("CLAUDE_CONFIG_DIR", ".claude"), "skills"),
		"codex":  filepath.Join(or("CODEX_HOME", ".codex"), "skills"),
	}
}

// tlmInstallSkill writes the skill for each agent in --for, saying per agent
// whether it was installed, updated or already current.
func tlmInstallSkill(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tlm install-skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	agents := fs.String("for", "claude,codex", "agents to install the skill for: claude, codex")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var errs []error
	for _, a := range commaList(*agents) {
		msg, err := installSkill(a)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", a, msg)
	}
	return errors.Join(errs...)
}

// installSkill writes the skill for one agent and says what it did.
func installSkill(agent string) (string, error) {
	dir, ok := skillHomes()[agent]
	if !ok {
		return "", fmt.Errorf("unknown agent %q (claude or codex)", agent)
	}
	path := filepath.Join(dir, skillName, "SKILL.md")
	old, err := os.ReadFile(path)
	what := "updated"
	switch {
	case errors.Is(err, os.ErrNotExist):
		what = "installed"
	case err != nil:
		return "", fmt.Errorf("%s: %w", agent, err)
	case bytes.Equal(old, handoffSkill):
		return "up to date (" + path + ")", nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("%s: %w", agent, err)
	}
	if err := os.WriteFile(path, handoffSkill, 0o644); err != nil {
		return "", fmt.Errorf("%s: %w", agent, err)
	}
	return what + " " + path, nil
}

// agentNames are the display names of the agents in skillHomes.
var agentNames = map[string]string{"claude": "Claude Code", "codex": "Codex"}

// staleSkills lists the agents (sorted ids) whose installed skill differs from
// the one in this binary. Agents without the skill aren't listed: not having it
// was someone's choice. ponytail: compares with the embedded copy each call (two
// small reads per page), no stored hash; a hand-edited skill also reads as stale.
func staleSkills() []string {
	var out []string
	for a, dir := range skillHomes() {
		b, err := os.ReadFile(filepath.Join(dir, skillName, "SKILL.md"))
		if err == nil && !bytes.Equal(b, handoffSkill) {
			out = append(out, a)
		}
	}
	slices.Sort(out)
	return out
}
