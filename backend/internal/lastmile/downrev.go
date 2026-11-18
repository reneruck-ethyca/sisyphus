package lastmile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
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
		f, head, src, err := staleMigration(ctx, dir, "origin/"+onto, d, added[d])
		if err != nil {
			return changed, fmt.Errorf("%s: %w", d, err)
		}
		if f == "" {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f), repoint(src, head), 0o644); err != nil {
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

// repoint sets src's down_revision, and its docstring's Revises: line, to head. A
// string value keeps its quotes and anything after it on the line.
func repoint(src []byte, head string) []byte {
	start, end, _ := assignment(src, downRevisionRe)
	if m := pyString.FindIndex(src[start:end]); m != nil {
		end = start + m[1]
	}
	q := `"`
	if src[start] == '\'' {
		q = "'"
	}
	out := slices.Concat(src[:start], []byte(q+head+q), src[end:])
	if loc := revisesRe.FindSubmatchIndex(out); loc != nil {
		out = slices.Concat(out[:loc[2]], []byte(head), out[loc[3]:])
	}
	return out
}

// migration is one revision file's place in the alembic graph.
type migration struct {
	file  string
	rev   string
	downs []string
	src   []byte
}

var errNotMigration = errors.New("no revision")

// staleMigration returns the PR's first migration in versions directory d, of added
// (as at HEAD), with the head of base's migrations there it should hang off, and its
// content; "" when it already hangs off it. The base's head comes from base alone,
// never from files the PR changed.
func staleMigration(ctx context.Context, dir, base, d string, added []string) (file, head string, src []byte, err error) {
	read := func(rev string, keep func(string) bool) ([]migration, error) {
		files, err := treeFiles(ctx, dir, rev, d)
		if err != nil {
			return nil, err
		}
		var ms []migration
		for _, f := range slices.Sorted(maps.Keys(files)) {
			if !keep(f) {
				continue
			}
			rev, downs, err := parseMigration(files[f])
			if errors.Is(err, errNotMigration) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("%s: %w", path.Base(f), err)
			}
			ms = append(ms, migration{f, rev, downs, files[f]})
		}
		return ms, nil
	}
	ours, err := read("HEAD", func(f string) bool { return slices.Contains(added, f) })
	if err != nil || len(ours) == 0 {
		return "", "", nil, err
	}
	theirs, err := read(base, func(string) bool { return true })
	if err != nil || len(theirs) == 0 {
		return "", "", nil, err
	}
	var named, heads, mine []string
	for _, m := range theirs {
		named = append(named, m.downs...)
	}
	for _, m := range theirs {
		if !slices.Contains(named, m.rev) {
			heads = append(heads, m.rev)
		}
	}
	if len(heads) != 1 {
		return "", "", nil, fmt.Errorf("the base has %d migration heads (%s), not one", len(heads), strings.Join(heads, ", "))
	}
	for _, m := range ours {
		mine = append(mine, m.rev)
	}
	first := slices.DeleteFunc(slices.Clone(ours), func(m migration) bool {
		return slices.ContainsFunc(m.downs, func(r string) bool { return slices.Contains(mine, r) })
	})
	if len(first) != 1 || len(first[0].downs) != 1 {
		return "", "", nil, fmt.Errorf("the PR's migrations (%s) don't hang off one revision", strings.Join(mine, ", "))
	}
	if first[0].downs[0] == heads[0] {
		return "", "", nil, nil
	}
	return first[0].file, heads[0], first[0].src, nil
}

// treeFiles reads the regular .py files directly in d at rev, by path, from git's
// objects: one ls-tree, one cat-file.
func treeFiles(ctx context.Context, dir, rev, d string) (map[string][]byte, error) {
	ls, err := proc.Run(ctx, dir, "git", "--literal-pathspecs", "ls-tree", "-z", rev, "--", d+"/")
	if err != nil {
		return nil, err
	}
	var paths, shas []string
	for _, e := range strings.Split(ls, "\x00") {
		meta, f, ok := strings.Cut(e, "\t")
		if fields := strings.Fields(meta); ok && len(fields) == 3 && (fields[0] == "100644" || fields[0] == "100755") &&
			path.Dir(f) == d && strings.HasSuffix(f, ".py") {
			paths, shas = append(paths, f), append(shas, fields[2])
		}
	}
	if len(paths) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(ctx, proc.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "cat-file", "--batch")
	cmd.Dir, cmd.Stdin = dir, strings.NewReader(strings.Join(shas, "\n")+"\n")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git cat-file: %w", err)
	}
	r := bufio.NewReader(bytes.NewReader(out))
	files := map[string][]byte{}
	for _, p := range paths {
		hdr, err := r.ReadString('\n')
		fields := strings.Fields(hdr)
		if err != nil || len(fields) != 3 {
			return nil, fmt.Errorf("git cat-file: bad header %q", hdr)
		}
		n, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, err
		}
		b := make([]byte, n+1) // the content and its trailing newline
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, fmt.Errorf("git cat-file: %w", err)
		}
		files[p] = b[:n]
	}
	return files, nil
}

var (
	revisionRe     = regexp.MustCompile(`(?m)^revision[ \t]*(?::[^=\n]*)?=[ \t]*`)
	downRevisionRe = regexp.MustCompile(`(?m)^down_revision[ \t]*(?::[^=\n]*)?=[ \t]*`)
	revisesRe      = regexp.MustCompile(`(?m)^Revises:[ \t]*(.*)$`)
	pyString       = regexp.MustCompile(`^(?:"([^"\\\n]*)"|'([^'\\\n]*)')`)
	comment        = regexp.MustCompile(`[ \t]*#.*$`)
	revisionID     = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// assignment finds the top-level assignment re matches in a revision file and returns
// the span of its value: the rest of the line, or a parenthesised tuple.
func assignment(src []byte, re *regexp.Regexp) (start, end int, ok bool) {
	loc := re.FindIndex(src)
	if loc == nil {
		return 0, 0, false
	}
	start, end = loc[1], len(src)
	if i := bytes.IndexByte(src[start:], '\n'); i >= 0 {
		end = start + i
	}
	if start < len(src) && src[start] == '(' {
		if i := bytes.IndexByte(src[start:], ')'); i >= 0 {
			end = start + i + 1
		}
	}
	return start, end, true
}

// parseMigration reads revision and down_revision: string literals, None, or a tuple
// of strings. Every id must be a plain one: it ends up in a commit and a file TLM writes.
func parseMigration(src []byte) (rev string, downs []string, err error) {
	value := func(name string, re *regexp.Regexp) ([]string, bool, error) {
		s, e, ok := assignment(src, re)
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
			if m == nil || (!tuple && len(out) > 0) || !revisionID.MatchString(m[1]+m[2]) {
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
	revs, ok, err := value("revision", revisionRe)
	if !ok {
		return "", nil, errNotMigration
	}
	if err == nil && len(revs) != 1 {
		err = errors.New("can't read revision")
	}
	if err != nil {
		return "", nil, err
	}
	downs, ok, err = value("down_revision", downRevisionRe)
	if !ok {
		err = errors.New("no down_revision")
	}
	return revs[0], downs, err
}

// setDownrevLabel puts label on h's PR (creating it in the repo if need be), or takes it off.
func (m *Manager) setDownrevLabel(ctx context.Context, h Handoff, label string, on bool) error {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	if !on {
		return m.GH.EditPR(ctx, ref, github.PREdit{RemoveLabels: []string{label}}) // the API takes an absent label as removed
	}
	if err := m.GH.CreateLabel(ctx, h.Repo, label, "c5def5", "Only a migration's down_revision changed"); err != nil {
		return err
	}
	return m.GH.EditPR(ctx, ref, github.PREdit{AddLabels: []string{label}})
}

// unlabelBeforePush takes the downrev label off before TLM pushes a head that doesn't
// qualify for it: CI reads labels off the push's event. Always, not only when h
// records the label: a crash or a failed push can leave it on unrecorded. It returns
// "" when the push may go ahead, else why not.
func (m *Manager) unlabelBeforePush(ctx context.Context, h Handoff, fx *effects) string {
	label := m.cfg.LastMile.DownrevLabels[h.Repo]
	if label == "" {
		return ""
	}
	if err := m.setDownrevLabel(ctx, h, label, false); err != nil {
		slog.Warn("lastmile removing the downrev label before a push failed", "id", h.ID, "err", err)
		return "couldn't remove the " + label + " label before pushing"
	}
	if fx != nil && h.DownrevHead != "" {
		fx.downrevDropped = true
		fx.log("label", "%s removed before pushing", label)
	}
	return ""
}

// unlabelDownrev takes the downrev label off h's PR and forgets the bump. A failure
// is logged, and h keeps it for the next tick to try again.
func (m *Manager) unlabelDownrev(ctx context.Context, h *Handoff, why string) {
	if label := m.cfg.LastMile.DownrevLabels[h.Repo]; label != "" {
		if err := m.setDownrevLabel(ctx, *h, label, false); err != nil {
			slog.Warn("lastmile removing the downrev label failed", "id", h.ID, "err", err)
			return
		}
		logEvent(h, m.now(), "label", label+" removed: "+why)
	}
	h.DownrevHead, h.DownrevFrom, h.DownrevRoot = "", "", ""
}
