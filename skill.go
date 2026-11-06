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
	"sync"
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

// agentSkill is one agent's copy of the skill as the dashboard shows it.
type agentSkill struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // "absent", "current" or "stale" (differs from this binary's; a hand edit too)
}

// skills is the marker the dashboard reads: each agent found on the machine (or
// holding the skill) with the state of its copy. Set at startup and after each
// install, update or uninstall, never per page: the skill only changes with the
// binary or through these actions.
var skills struct {
	sync.Mutex
	agents []agentSkill
	busy   bool   // an action runs in the background
	err    string // why the last action failed ("" = it didn't)
}

// markSkills sets the marker from the agents' skill dirs.
func markSkills() {
	var out []agentSkill
	for _, a := range []struct{ id, cmd string }{{"claude", "claude"}, {"codex", "codex"}} {
		st := "absent"
		if b, err := os.ReadFile(filepath.Join(skillHomes()[a.id], skillName, "SKILL.md")); err == nil {
			st = "stale"
			if bytes.Equal(b, handoffSkill) {
				st = "current"
			}
		}
		if st != "absent" || agentPresent(a.id, a.cmd) {
			out = append(out, agentSkill{a.id, agentNames[a.id], st})
		}
	}
	skills.Lock()
	skills.agents = out
	skills.Unlock()
}

// skillStatus is the marker as the dashboard shows it.
type skillStatus struct {
	Agents []agentSkill `json:"agents"`
	Busy   bool         `json:"busy"`
	Error  string       `json:"error,omitempty"`
}

// Stale says whether any agent has an out-of-date copy, for the Settings tab's dot.
func (s skillStatus) Stale() bool {
	return slices.ContainsFunc(s.Agents, func(a agentSkill) bool { return a.State == "stale" })
}

func skillState() skillStatus {
	skills.Lock()
	defer skills.Unlock()
	return skillStatus{Agents: slices.Clone(skills.agents), Busy: skills.busy, Error: skills.err}
}

// startSkillAction installs (or updates) or uninstalls the skill for one agent
// in the background, then re-reads the marker. A no-op while another runs.
func startSkillAction(agent, action string) error {
	dir, ok := skillHomes()[agent]
	if !ok {
		return fmt.Errorf("unknown agent %q", agent)
	}
	if action != "install" && action != "uninstall" {
		return fmt.Errorf("unknown action %q", action)
	}
	skills.Lock()
	defer skills.Unlock()
	if skills.busy {
		return nil
	}
	skills.busy, skills.err = true, ""
	go func() {
		var err error
		if action == "install" {
			_, err = installSkill(agent)
		} else {
			err = os.RemoveAll(filepath.Join(dir, skillName))
		}
		markSkills()
		skills.Lock()
		defer skills.Unlock()
		skills.busy = false
		if err != nil {
			skills.err = err.Error()
		}
	}()
	return nil
}
