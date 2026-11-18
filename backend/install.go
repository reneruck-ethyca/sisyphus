package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/reneruck/sisyphus/internal/config"
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
		return strings.TrimSpace(line)
	}
	yes := func(q string, def bool) bool {
		hint := map[bool]string{true: "Y/n", false: "y/N"}[def]
		switch strings.ToLower(ask(q, hint)) {
		case "":
			return def
		case "y", "yes":
			return true
		}
		return false
	}

	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		return fmt.Errorf("finding this binary: %w", err)
	}
	dir := binDir()
	if d := ask("Install tlm into", dir); d != "" {
		dir = d
	}
	dir, err = filepath.Abs(config.ExpandHome(dir))
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
	for _, a := range agents {
		if !a.present() {
			_, _ = fmt.Fprintf(stdout, "%s: not found, skipped (tlm install-skill --for %s installs the skill anyway)\n", a.name, a.id)
			continue
		}
		if !yes("Install the "+skillName+" skill for "+a.name+"?", true) {
			continue
		}
		msg, err := a.installSkill()
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
