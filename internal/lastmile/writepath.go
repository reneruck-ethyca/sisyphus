package lastmile

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// denied are the paths an agent never changes unless lastmile.allow_paths exempts
// them. Git hook configs are here too: hooks are off for TLM's commits, but the
// user's next commit in the worktree would run what the agent wrote.
var denied = []string{
	".github/workflows/**", "**/migrations/**",
	"**/uv.lock", "**/poetry.lock", "**/package-lock.json", "**/yarn.lock", "**/pnpm-lock.yaml", "**/go.sum",
	".husky/**", ".pre-commit-config.yaml", ".githooks/**", "lefthook.yml", ".lefthook.yml",
}

var (
	globsMu sync.Mutex // handoffs' write paths run concurrently
	globs   = map[string]*regexp.Regexp{}
)

// globMatch matches a slash-separated path against a glob: * and ? stay within
// a segment, ** crosses them, and a leading **/ also matches no directory at all.
func globMatch(glob, p string) bool {
	globsMu.Lock()
	defer globsMu.Unlock()
	re, ok := globs[glob]
	if !ok {
		var sb strings.Builder
		sb.WriteString("^")
		for i := 0; i < len(glob); i++ {
			switch {
			case strings.HasPrefix(glob[i:], "**/"):
				sb.WriteString("(?:.*/)?")
				i += 2
			case strings.HasPrefix(glob[i:], "**"):
				sb.WriteString(".*")
				i++
			case glob[i] == '*':
				sb.WriteString("[^/]*")
			case glob[i] == '?':
				sb.WriteString("[^/]")
			default:
				sb.WriteString(regexp.QuoteMeta(string(glob[i])))
			}
		}
		sb.WriteString("$")
		re = regexp.MustCompile(sb.String())
		globs[glob] = re
	}
	return re.MatchString(p)
}

func (m *Manager) protected(p string) bool {
	p = path.Clean(filepath.ToSlash(p))
	for _, g := range m.cfg.LastMile.AllowPaths {
		if globMatch(g, p) {
			return false
		}
	}
	for _, g := range denied {
		if globMatch(g, p) {
			return true
		}
	}
	return false
}

// AgentResult is the contract of /out/result.json for every agent kind.
type AgentResult struct {
	Summary        string `json:"summary"`
	Confidence     string `json:"confidence"` // low | medium | high
	Escalate       bool   `json:"escalate"`
	EscalateReason string `json:"escalate_reason"`
	Body           string `json:"body,omitempty"`        // a PR body update; acted on from phase 4
	BodyReason     string `json:"body_reason,omitempty"` // why the body should change
}

// changedFiles lists what the worktree differs from HEAD in: tracked changes
// (staged or not, renames as delete plus add) and untracked files that
// .gitignore doesn't hide.
func changedFiles(ctx context.Context, dir string) ([]string, error) {
	var files []string
	for _, args := range [][]string{
		{"diff", "--name-only", "-z", "--no-renames", "HEAD"},
		{"ls-files", "-z", "--others", "--exclude-standard"},
	} {
		out, err := proc.Run(ctx, dir, "git", args...)
		if err != nil {
			return nil, err
		}
		for _, f := range strings.Split(out, "\x00") {
			if f != "" {
				files = append(files, f)
			}
		}
	}
	return files, nil
}

// changedLines counts added plus removed lines of the change, untracked files
// included. A binary file has no line count to cap, so it's refused (binary).
func changedLines(ctx context.Context, dir string, files []string) (n int, binary string, err error) {
	out, err := proc.Run(ctx, dir, "git", "diff", "--numstat", "--no-renames", "HEAD")
	if err != nil {
		return 0, "", err
	}
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 3 {
			continue
		}
		if f[0] == "-" {
			return 0, f[2], nil
		}
		a, _ := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		n += a + d
	}
	tracked, err := proc.Run(ctx, dir, "git", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return 0, "", err
	}
	for _, f := range strings.Split(tracked, "\x00") {
		if f == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		if bytes.IndexByte(b, 0) >= 0 { // git's own binary test
			return 0, f, nil
		}
		n += bytes.Count(b, []byte("\n"))
		if len(b) > 0 && b[len(b)-1] != '\n' {
			n++
		}
	}
	return n, "", nil
}

// stamp is what tells a file changed without reading it.
type stamp struct {
	size int64
	mod  time.Time
	mode os.FileMode
}

// ignoredFiles stamps the worktree's gitignored files. The write path never
// commits them, so an agent could otherwise leave something there the host runs
// later unseen (a .venv binary, an .envrc).
// ponytail: walks everything ignored on each agent run, node_modules included; fine
// next to an agent run's minutes. Caches mounted as volumes are never written here.
func ignoredFiles(ctx context.Context, dir string) (map[string]stamp, error) {
	out, err := proc.Run(ctx, dir, "git", "ls-files", "-z", "--others", "--ignored", "--exclude-standard")
	if err != nil {
		return nil, err
	}
	files := map[string]stamp{}
	for _, f := range strings.Split(out, "\x00") {
		if f == "" {
			continue
		}
		if fi, err := os.Lstat(filepath.Join(dir, f)); err == nil {
			files[f] = stamp{fi.Size(), fi.ModTime(), fi.Mode()}
		}
	}
	return files, nil
}

// ignoredChanges removes the ignored files the run created and names every one
// it created or changed; a changed one can't be put back, so a human looks.
func ignoredChanges(ctx context.Context, dir string, before map[string]stamp) ([]string, error) {
	after, err := ignoredFiles(ctx, dir)
	if err != nil {
		return nil, err
	}
	var changed []string
	for f, st := range after {
		old, ok := before[f]
		if ok && old == st {
			continue
		}
		changed = append(changed, f)
		if !ok {
			_ = os.Remove(filepath.Join(dir, f))
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// commitAgentWork is the one way an agent's edits reach the branch (design §5):
// it inspects the worktree, throws everything away if the change breaks a rule,
// else commits it as "TLM: <summary>" and pushes. A non-empty reason means the
// handoff needs a human, and the worktree is back at its head. The caller holds
// the worktree lock.
func (m *Manager) commitAgentWork(ctx context.Context, h Handoff, res AgentResult, ignoredBefore map[string]stamp) (reason string, err error) {
	head, err := worktree.HeadSHA(ctx, h.Worktree)
	if err != nil {
		return "", err
	}
	reject := func(format string, a ...any) (string, error) {
		return fmt.Sprintf(format, a...), worktree.Reset(ctx, h.Worktree, head)
	}
	ignored, err := ignoredChanges(ctx, h.Worktree, ignoredBefore)
	if err != nil {
		return "", err
	}
	if len(ignored) > 0 {
		return reject("agent created or changed gitignored files (new ones removed): %s", strings.Join(ignored, ", "))
	}
	if res.Escalate {
		return "agent escalated: " + res.EscalateReason, worktree.Reset(ctx, h.Worktree, head)
	}
	files, err := changedFiles(ctx, h.Worktree)
	if err != nil {
		return "", err
	}
	if len(files) == 0 {
		return "agent made no changes", nil
	}
	for _, f := range files {
		if m.protected(f) {
			return reject("agent changed a protected path: %s", f)
		}
	}
	lines, binary, err := changedLines(ctx, h.Worktree, files)
	if err != nil {
		return "", err
	}
	if binary != "" {
		return reject("agent made a binary change: %s", binary)
	}
	if limit := m.cfg.LastMile.MaxDiffLines; lines > limit {
		return reject("agent's change is %d lines, over max_diff_lines (%d)", lines, limit)
	}
	// ponytail: no host-side lint/test run over the change: it would execute code the agent
	// wrote on the host. The agent runs the repo's checks in its container; CI is the gate.
	if _, err := proc.Run(ctx, h.Worktree, "git", "add", "-A"); err != nil {
		return reject("git add failed: %v", err)
	}
	// The user's own git identity when there is one. Hooks are off: husky, pre-commit and
	// friends read their config from the worktree, i.e. would run what the agent wrote.
	args := []string{"-c", "core.hooksPath=/dev/null"}
	for _, k := range []struct{ key, val string }{{"user.name", "TLM"}, {"user.email", "tlm@localhost"}} {
		if v, err := proc.Run(ctx, h.Worktree, "git", "config", k.key); err != nil || v == "" {
			args = append(args, "-c", k.key+"="+k.val)
		}
	}
	summary := strings.Join(strings.Fields(res.Summary), " ")
	if summary == "" {
		summary = "fix"
	}
	if r := []rune(summary); len(r) > 100 {
		summary = string(r[:100])
	}
	if _, err := proc.Run(ctx, h.Worktree, "git", append(args, "commit", "-q", "-m", "TLM: "+summary)...); err != nil {
		return reject("commit failed: %v", err)
	}
	if _, err := proc.Run(ctx, h.Worktree, "git", "-c", "core.hooksPath=/dev/null", "push"); err != nil { // pre-push hooks too
		// Unpushed work would pile onto the next round: back to the head.
		return reject("push failed: %v", err)
	}
	slog.Info("lastmile pushed agent work", "id", h.ID, "lines", lines, "files", len(files))
	return "", nil
}
