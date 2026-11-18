package main

import (
	"bytes"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
)

// handoffSkill teaches coding agents when and how to use `tlm handoff`. It ships
// in the binary; `tlm install-skill` or the Settings page installs it for an agent.
//
//go:embed skills/handing-off-to-tlm/SKILL.md
var handoffSkill []byte

const skillName = "handing-off-to-tlm"

// agent is a coding-agent CLI the skill can be installed for.
type agent struct {
	id, name, cmd string
	env, dir      string // the config-dir override and its default under $HOME
}

var agents = []agent{
	{"claude", "Claude Code", "claude", "CLAUDE_CONFIG_DIR", ".claude"},
	{"codex", "Codex", "codex", "CODEX_HOME", ".codex"},
}

func findAgent(id string) (agent, bool) {
	i := slices.IndexFunc(agents, func(a agent) bool { return a.id == id })
	if i < 0 {
		return agent{}, false
	}
	return agents[i], true
}

// configDir is the agent's config dir, honouring its override.
func (a agent) configDir() string {
	if v := os.Getenv(a.env); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, a.dir)
}

// skillDir is where the agent looks for this personal skill.
func (a agent) skillDir() string { return filepath.Join(a.configDir(), "skills", skillName) }

// present: the agent's CLI is on PATH or its config dir exists.
func (a agent) present() bool {
	if _, err := exec.LookPath(a.cmd); err == nil {
		return true
	}
	_, err := os.Stat(a.configDir())
	return err == nil
}

// skillState says whether the agent's copy is "absent", "current" or "stale"
// (differs from this binary's, a hand edit too).
func (a agent) skillState() (string, error) {
	b, err := os.ReadFile(filepath.Join(a.skillDir(), "SKILL.md"))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "absent", nil
	case err != nil:
		return "", err
	case bytes.Equal(b, handoffSkill):
		return "current", nil
	}
	return "stale", nil
}

// installSkill writes this binary's skill for the agent and says what it did.
func (a agent) installSkill() (string, error) {
	path := filepath.Join(a.skillDir(), "SKILL.md")
	state, err := a.skillState()
	if err == nil && state != "current" {
		err = os.MkdirAll(a.skillDir(), 0o755)
	}
	if err == nil && state != "current" {
		err = os.WriteFile(path, handoffSkill, 0o644)
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", a.id, err)
	}
	return map[string]string{"absent": "installed ", "stale": "updated ", "current": "up to date: "}[state] + path, nil
}

// tlmInstallSkill writes the skill for each agent in --for, saying per agent
// whether it was installed, updated or already current.
func tlmInstallSkill(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tlm install-skill", flag.ContinueOnError)
	fs.SetOutput(stderr)
	ids := fs.String("for", "claude,codex", "agents to install the skill for: claude, codex")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var errs []error
	for _, id := range commaList(*ids) {
		a, ok := findAgent(id)
		if !ok {
			errs = append(errs, fmt.Errorf("unknown agent %q (claude or codex)", id))
			continue
		}
		msg, err := a.installSkill()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", id, msg)
	}
	return errors.Join(errs...)
}

// agentSkill is one agent's copy of the skill as the dashboard shows it.
type agentSkill struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"` // agent.skillState
}

// skills is the marker the dashboard reads: each agent found on the machine (or
// holding the skill) with the state of its copy. Set at startup and after each
// install, update or uninstall, never per page: the skill only changes with the
// binary or through these actions.
var skills struct {
	sync.Mutex
	agents []agentSkill
	busy   string // the action running in the background, as "agent/action" ("" = none)
	err    string // why the last action failed ("" = it didn't)
}

// markSkills sets the marker from the agents' skill dirs.
func markSkills() {
	var out []agentSkill
	for _, a := range agents {
		st, err := a.skillState()
		if err != nil {
			st = "stale" // unreadable: offer to rewrite it
		}
		if st != "absent" || a.present() {
			out = append(out, agentSkill{a.id, a.name, st})
		}
	}
	skills.Lock()
	skills.agents = out
	skills.Unlock()
}

// skillStatus is the marker as the dashboard shows it.
type skillStatus struct {
	Agents []agentSkill `json:"agents"`
	Busy   string       `json:"busy,omitempty"` // "agent/action" while one runs
	Error  string       `json:"error,omitempty"`
}

func skillsStatus() skillStatus {
	skills.Lock()
	defer skills.Unlock()
	return skillStatus{Agents: slices.Clone(skills.agents), Busy: skills.busy, Error: skills.err}
}

// startSkillAction installs (or updates) or uninstalls the skill for one agent
// in the background, then re-reads the marker. A no-op while another runs.
func startSkillAction(id, action string) error {
	a, ok := findAgent(id)
	if !ok {
		return fmt.Errorf("unknown agent %q", id)
	}
	if action != "install" && action != "uninstall" {
		return fmt.Errorf("unknown action %q", action)
	}
	skills.Lock()
	defer skills.Unlock()
	if skills.busy != "" {
		return nil
	}
	skills.busy, skills.err = id+"/"+action, ""
	go func() {
		var err error
		if action == "install" {
			_, err = a.installSkill()
		} else {
			err = os.RemoveAll(a.skillDir())
		}
		markSkills()
		skills.Lock()
		defer skills.Unlock()
		skills.busy = ""
		if err != nil {
			skills.err = err.Error()
		}
	}()
	return nil
}
