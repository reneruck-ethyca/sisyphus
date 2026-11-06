package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// tlmInstall links `tlm` into a directory on PATH, pointing at this binary (so a
// rebuild needs no reinstall), then offers the handoff skill to each agent CLI
// found on the machine. It asks before every change; an empty answer takes the default.
func tlmInstall(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("tlm install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		return err
	}
	in := bufio.NewReader(stdin)
	ask := func(q, def string) string {
		_, _ = fmt.Fprintf(stdout, "%s [%s]: ", q, def)
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return def
	}
	yes := func(q string, def bool) bool {
		d := "y/N"
		if def {
			d = "Y/n"
		}
		a := strings.ToLower(ask(q, d))
		if a == strings.ToLower(d) {
			return def
		}
		return a == "y" || a == "yes"
	}

	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	dir, err := filepath.Abs(expandHome(ask("Install tlm into", binDir())))
	if err != nil {
		return err
	}
	if err := linkTLM(self, dir, func(old string) bool {
		return yes(fmt.Sprintf("%s exists (%s); replace it?", filepath.Join(dir, "tlm"), old), false)
	}, stdout); err != nil {
		return err
	}
	if !slices.Contains(filepath.SplitList(os.Getenv("PATH")), dir) {
		_, _ = fmt.Fprintf(stdout, "note: %s isn't on your PATH; add it to your shell profile\n", dir)
	}

	var errs []error
	for _, a := range []struct{ id, name, cmd string }{{"claude", "Claude Code", "claude"}, {"codex", "Codex", "codex"}} {
		if !agentPresent(a.id, a.cmd) {
			_, _ = fmt.Fprintf(stdout, "%s: not found, skipped (tlm install-skill --for %s installs the skill anyway)\n", a.name, a.id)
			continue
		}
		if !yes("Install the "+skillName+" skill for "+a.name+"?", true) {
			continue
		}
		msg, err := installSkill(a.id)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s: %s\n", a.id, msg)
	}
	return errors.Join(errs...)
}

// linkTLM makes dir/tlm a symlink to target. An existing file there is replaced
// only if replace (given what it is) says so.
func linkTLM(target, dir string, replace func(old string) bool, stdout io.Writer) error {
	link := filepath.Join(dir, "tlm")
	old, rerr := os.Readlink(link)
	if rerr == nil && old == target {
		_, _ = fmt.Fprintf(stdout, "tlm: up to date (%s -> %s)\n", link, target)
		return nil
	}
	if _, err := os.Lstat(link); err == nil {
		desc := "a file"
		if rerr == nil {
			desc = "a link to " + old
		}
		if !replace(desc) {
			_, _ = fmt.Fprintln(stdout, "tlm: left as it is")
			return nil
		}
		if err := os.Remove(link); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.Symlink(target, link); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "tlm: installed %s -> %s\n", link, target)
	return nil
}

// binDir suggests where to put tlm: the first of the usual user-writable bin
// dirs that is on PATH, else the first writable PATH entry, else ~/.local/bin.
func binDir() string {
	home, _ := os.UserHomeDir()
	path := filepath.SplitList(os.Getenv("PATH"))
	writable := func(d string) bool { return syscall.Access(d, 0x2) == nil } // W_OK
	for _, d := range []string{filepath.Join(home, ".local", "bin"), filepath.Join(home, "bin"), "/opt/homebrew/bin", "/usr/local/bin"} {
		if slices.Contains(path, d) && writable(d) {
			return d
		}
	}
	for _, d := range path {
		if filepath.IsAbs(d) && writable(d) {
			return d
		}
	}
	return filepath.Join(home, ".local", "bin")
}

// agentPresent: the agent's CLI is on PATH or its config dir exists.
func agentPresent(id, cmd string) bool {
	if _, err := exec.LookPath(cmd); err == nil {
		return true
	}
	_, err := os.Stat(filepath.Dir(skillHomes()[id]))
	return err == nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}
