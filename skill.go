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
	homes := skillHomes()
	var errs []error
	for _, a := range commaList(*agents) {
		dir, ok := homes[a]
		if !ok {
			errs = append(errs, fmt.Errorf("unknown agent %q (claude or codex)", a))
			continue
		}
		path := filepath.Join(dir, skillName, "SKILL.md")
		old, err := os.ReadFile(path)
		what := "updated"
		switch {
		case errors.Is(err, os.ErrNotExist):
			what = "installed"
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		case bytes.Equal(old, handoffSkill):
			_, _ = fmt.Fprintf(stdout, "%s: up to date (%s)\n", a, path)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		if err := os.WriteFile(path, handoffSkill, 0o644); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", a, err))
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s %s\n", a, what, path)
	}
	return errors.Join(errs...)
}
