package lastmile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
)

// bumpDownrev points the PR's first alembic migration at the base's head (design §7.4),
// per versions directory the PR adds migrations to: the worktree is rebased onto
// origin/<onto>, and each rewrite is committed on its own. It returns the files it
// rewrote. Several heads on the base, or a migration it can't read, is an error.
func bumpDownrev(ctx context.Context, dir, onto string, migrationGlobs []string) ([]string, error) {
	out, err := proc.Run(ctx, dir, "git", "diff", "--name-only", "--diff-filter=A", "-z", "origin/"+onto+"...HEAD")
	if err != nil {
		return nil, err
	}
	added := map[string][]string{} // by directory
	for _, f := range strings.Split(out, "\x00") {
		if strings.HasSuffix(f, ".py") && slices.ContainsFunc(migrationGlobs, func(g string) bool { return globMatch(g, f) }) {
			added[path.Dir(f)] = append(added[path.Dir(f)], f)
		}
	}
	var changed []string
	for _, d := range slices.Sorted(maps.Keys(added)) {
		f, head, err := staleMigration(ctx, dir, d, added[d])
		if err != nil {
			return changed, fmt.Errorf("%s: %w", d, err)
		}
		if f == "" {
			continue
		}
		p := filepath.Join(dir, f)
		src, err := os.ReadFile(p)
		if err != nil {
			return changed, err
		}
		start, end, _ := assignment(src, "down_revision")
		q := `"`
		if src[start] == '\'' {
			q = "'"
		}
		if err := os.WriteFile(p, slices.Concat(src[:start], []byte(q+head+q), src[end:]), 0o644); err != nil {
			return changed, err
		}
		msg := fmt.Sprintf("chore(migrations): point %s at %s", path.Base(f), head)
		if _, err := proc.Run(ctx, dir, "git", append(hostGit(ctx, dir), "--literal-pathspecs", "commit", "-q", "-m", msg, "--", f)...); err != nil {
			return changed, err
		}
		changed = append(changed, f)
	}
	return changed, nil
}

// migration is one revision file's place in the alembic graph.
type migration struct {
	file  string
	rev   string
	downs []string
}

var errNotMigration = errors.New("no revision")

// staleMigration reads the migrations in versions directory d (at HEAD, which the
// clean worktree is) and returns the PR's first one, of added, with the base head it
// should hang off; "" when it already does.
func staleMigration(ctx context.Context, dir, d string, added []string) (file, head string, err error) {
	ls, err := proc.Run(ctx, dir, "git", "--literal-pathspecs", "ls-files", "-s", "-z", "--", d)
	if err != nil {
		return "", "", err
	}
	var base, ours []migration
	for _, e := range strings.Split(ls, "\x00") {
		meta, f, ok := strings.Cut(e, "\t")
		if !ok || path.Dir(f) != d || !strings.HasSuffix(f, ".py") || !strings.HasPrefix(meta, "1006") { // regular files only
			continue
		}
		src, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "", "", err
		}
		rev, downs, err := parseMigration(src)
		if errors.Is(err, errNotMigration) {
			continue
		}
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", path.Base(f), err)
		}
		if slices.Contains(added, f) {
			ours = append(ours, migration{f, rev, downs})
		} else {
			base = append(base, migration{f, rev, downs})
		}
	}
	if len(ours) == 0 || len(base) == 0 {
		return "", "", nil
	}
	var named, heads, mine []string
	for _, m := range base {
		named = append(named, m.downs...)
	}
	for _, m := range base {
		if !slices.Contains(named, m.rev) {
			heads = append(heads, m.rev)
		}
	}
	if len(heads) != 1 {
		return "", "", fmt.Errorf("the base has %d migration heads (%s), not one", len(heads), strings.Join(heads, ", "))
	}
	for _, m := range ours {
		mine = append(mine, m.rev)
	}
	first := slices.DeleteFunc(slices.Clone(ours), func(m migration) bool {
		return slices.ContainsFunc(m.downs, func(r string) bool { return slices.Contains(mine, r) })
	})
	if len(first) != 1 || len(first[0].downs) != 1 {
		return "", "", fmt.Errorf("the PR's migrations (%s) don't hang off one revision", strings.Join(mine, ", "))
	}
	if first[0].downs[0] == heads[0] {
		return "", "", nil
	}
	return first[0].file, heads[0], nil
}

// assignment finds the top-level `name = …` (or `name: T = …`) of a revision file and
// returns the span of its value: the rest of the line, or a parenthesised tuple.
func assignment(src []byte, name string) (start, end int, ok bool) {
	loc := regexp.MustCompile(`(?m)^` + name + `[ \t]*(?::[^=\n]*)?=[ \t]*`).FindIndex(src)
	if loc == nil {
		return 0, 0, false
	}
	start, end = loc[1], len(src)
	if i := slices.Index(src[start:], '\n'); i >= 0 {
		end = start + i
	}
	if start < len(src) && src[start] == '(' {
		if i := slices.Index(src[start:], ')'); i >= 0 {
			end = start + i + 1
		}
	}
	return start, end, true
}

var (
	pyString = regexp.MustCompile(`^(?:"([^"\\\n]*)"|'([^'\\\n]*)')`)
	comment  = regexp.MustCompile(`[ \t]*#.*$`)
)

// parseMigration reads revision and down_revision: string literals, None, or a tuple of strings.
func parseMigration(src []byte) (rev string, downs []string, err error) {
	value := func(name string) ([]string, bool, error) {
		s, e, ok := assignment(src, name)
		if !ok {
			return nil, false, nil
		}
		v := strings.TrimSpace(string(src[s:e]))
		if !strings.HasPrefix(v, "(") {
			v = comment.ReplaceAllString(v, "")
		}
		if v == "None" {
			return nil, true, nil
		}
		tuple := strings.HasPrefix(v, "(") && strings.HasSuffix(v, ")")
		if tuple {
			v = v[1 : len(v)-1]
		}
		var out []string
		for v = strings.TrimSpace(v); v != ""; v = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(v), ",")) {
			m := pyString.FindStringSubmatch(v)
			if m == nil || (!tuple && len(out) > 0) {
				return nil, true, fmt.Errorf("can't read %s", name)
			}
			out = append(out, m[1]+m[2])
			v = v[len(m[0]):]
		}
		if len(out) == 0 {
			return nil, true, fmt.Errorf("can't read %s", name)
		}
		return out, true, nil
	}
	revs, ok, err := value("revision")
	if !ok {
		return "", nil, errNotMigration
	}
	if err == nil && len(revs) != 1 {
		err = errors.New("can't read revision")
	}
	if err != nil {
		return "", nil, err
	}
	downs, ok, err = value("down_revision")
	if !ok {
		err = errors.New("no down_revision")
	}
	return revs[0], downs, err
}

// labelDownrev puts the repo's downrev label on a PR whose bump TLM just pushed.
// A failure is logged: the label only spares CI work.
func (m *Manager) labelDownrev(ctx context.Context, h *Handoff) {
	label := m.cfg.LastMile.DownrevLabels[h.Repo]
	err := m.GH.CreateLabel(ctx, h.Repo, label, "c5def5", "Only a migration's down_revision changed")
	if err == nil {
		err = m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{AddLabels: []string{label}})
	}
	if err != nil {
		slog.Warn("lastmile downrev label failed", "id", h.ID, "err", err)
		return
	}
	logEvent(h, m.now(), "label", label+" on "+h.DownrevHead[:min(len(h.DownrevHead), 7)])
}

// unlabelDownrev takes the downrev label off once the PR's head is another than the
// one TLM labelled. A failure is logged and tried again on the next tick.
func (m *Manager) unlabelDownrev(ctx context.Context, h *Handoff) {
	if label := m.cfg.LastMile.DownrevLabels[h.Repo]; label != "" {
		if err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{RemoveLabels: []string{label}}); err != nil {
			slog.Warn("lastmile removing the downrev label failed", "id", h.ID, "err", err)
			return
		}
		logEvent(h, m.now(), "label", label+" removed: the head moved on")
	}
	h.DownrevHead, h.DownrevFrom = "", ""
}
