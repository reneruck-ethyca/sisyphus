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
	"strconv"
	"strings"

	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// denied are the paths an agent never changes unless lastmile.allow_paths exempts them.
var denied = []string{
	".github/workflows/**", "**/migrations/**",
	"**/uv.lock", "**/poetry.lock", "**/package-lock.json", "**/yarn.lock", "**/pnpm-lock.yaml", "**/go.sum",
}

var globs = map[string]*regexp.Regexp{}

// globMatch matches a slash-separated path against a glob: * and ? stay within
// a segment, ** crosses them, and a leading **/ also matches no directory at all.
func globMatch(glob, p string) bool {
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
		globs[glob] = re // ponytail: unsynchronised; the write path runs one at a time per handoff but handoffs run concurrently
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

// changedLines counts added plus removed lines of the change, untracked files included.
func changedLines(ctx context.Context, dir string, files []string) (int, error) {
	out, err := proc.Run(ctx, dir, "git", "diff", "--numstat", "--no-renames", "HEAD")
	if err != nil {
		return 0, err
	}
	n := 0
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) < 2 {
			continue
		}
		a, _ := strconv.Atoi(f[0]) // "-" for a binary file: counts 0 lines
		d, _ := strconv.Atoi(f[1])
		n += a + d
	}
	tracked, err := proc.Run(ctx, dir, "git", "ls-files", "-z", "--others", "--exclude-standard")
	if err != nil {
		return 0, err
	}
	for _, f := range strings.Split(tracked, "\x00") {
		if f == "" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			continue
		}
		n += bytes.Count(b, []byte("\n"))
		if len(b) > 0 && b[len(b)-1] != '\n' {
			n++
		}
	}
	return n, nil
}

// commitAgentWork is the one way an agent's edits reach the branch (design §5):
// it inspects the worktree, throws everything away if the change breaks a rule,
// else commits it as "TLM: <summary>" and pushes. A non-empty reason means the
// handoff needs a human, and the worktree is back at its head. The caller holds
// the worktree lock.
func (m *Manager) commitAgentWork(ctx context.Context, h Handoff, res AgentResult) (reason string, err error) {
	head, err := worktree.HeadSHA(ctx, h.Worktree)
	if err != nil {
		return "", err
	}
	reject := func(format string, a ...any) (string, error) {
		return fmt.Sprintf(format, a...), worktree.Reset(ctx, h.Worktree, head)
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
	lines, err := changedLines(ctx, h.Worktree, files)
	if err != nil {
		return "", err
	}
	if limit := m.cfg.LastMile.MaxDiffLines; lines > limit {
		return reject("agent's change is %d lines, over max_diff_lines (%d)", lines, limit)
	}
	if check := m.cfg.LastMile.RepoChecks[h.Repo]; check != "" {
		env := map[string]string{"TLM_ID": h.ID, "TLM_REPO": h.Repo, "TLM_BRANCH": h.Branch}
		if err := m.runHook(ctx, check, m.cfg.HookTimeout.Duration, h.Worktree, env); err != nil {
			return reject("repo check failed: %v", err)
		}
	}
	if _, err := proc.Run(ctx, h.Worktree, "git", "add", "-A"); err != nil {
		return reject("git add failed: %v", err)
	}
	// The user's own git identity when there is one; never --no-verify.
	args := []string{}
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
	if _, err := proc.Run(ctx, h.Worktree, "git", "push"); err != nil {
		// Unpushed work would pile onto the next round: back to the head.
		return reject("push failed: %v", err)
	}
	slog.Info("lastmile pushed agent work", "id", h.ID, "lines", lines, "files", len(files))
	return "", nil
}
