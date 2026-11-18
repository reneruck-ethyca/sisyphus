package lastmile

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/proc"
)

// Dirty is what a needs-human handoff's worktree holds uncommitted. Files are
// sisyphus's own reading; Summary and Message are a model's, filled in later.
// Summary, Message and Err are all "" while that assessment runs.
type Dirty struct {
	Status    string      `json:"status"` // git status --porcelain when seen
	At        time.Time   `json:"at"`
	Files     []DirtyFile `json:"files"`
	Summary   string      `json:"summary,omitempty"`
	Message   string      `json:"message,omitempty"` // suggested commit subject
	Err       string      `json:"err,omitempty"`
	Dismissed bool        `json:"dismissed,omitempty"` // "leave it": collapse the panel until the status changes
	WIP       string      `json:"wip,omitempty"`       // the local commit an agent run is working from; unwrapped if the run dies
}

// DirtyFile is one uncommitted file and its line counts against HEAD.
type DirtyFile struct {
	Path   string `json:"path"`
	Code   string `json:"code"` // M, A, D, R or ??
	Add    int    `json:"add"`
	Del    int    `json:"del"`
	Binary bool   `json:"binary,omitempty"`
}

// Hash names the status the page was rendered for; every action sends it back.
func (d Dirty) Hash() string {
	sum := sha256.Sum256([]byte(d.Status))
	return hex.EncodeToString(sum[:8])
}

func (d Dirty) assessed() bool { return d.Summary != "" || d.Message != "" || d.Err != "" }

// ErrStale is an action against a worktree that changed since the page was rendered.
var ErrStale = errors.New("worktree changed — reload")

const (
	maxDirtyFiles = 500 // ponytail: rows beyond this aren't listed; commit them by hand
	maxShownDiff  = 200 << 10
	maxCommitMsg  = 2000
	maxAgentTask  = 4000
)

// checkDirty, for a needs-human handoff, records what its worktree holds
// uncommitted and starts one assessment per distinct status. It never fails the tick.
func (m *Manager) checkDirty(ctx context.Context, h *Handoff) {
	if h.Worktree == "" {
		return
	}
	st, err := status(ctx, *h)
	if err != nil {
		return
	}
	if st == "" && h.Dirty != nil && h.Dirty.WIP != "" {
		// An agent run started from this WIP commit and never finished (sisyphus restarted):
		// give the human their uncommitted changes back.
		if m.unwrapWIP(ctx, *h, h.Dirty.WIP) {
			logEvent(h, m.now(), "dirty", "agent run lost; the changes are uncommitted again")
		}
		h.Dirty = nil
		return
	}
	if st == "" {
		h.Dirty = nil
		return
	}
	// Files == nil is the old, options-based format: rebuild it.
	if h.Dirty == nil || h.Dirty.Status != st || h.Dirty.Files == nil {
		files, err := dirtyFiles(ctx, h.Worktree, h.Untracked)
		if err != nil {
			slog.Warn("lastmile dirty-worktree files", "id", h.ID, "err", err)
			return
		}
		d := &Dirty{Status: st, At: m.now(), Files: files}
		if h.Dirty != nil {
			d.WIP = h.Dirty.WIP // still the human's, until a lost run unwraps it
		}
		h.Dirty = d
	}
	if m.Assess == nil || h.Dirty.assessed() {
		return
	}
	if _, busy := m.assessing.LoadOrStore(h.ID, true); busy {
		return
	}
	m.wg.Add(1)
	go m.assess(ctx, *h, *h.Dirty, st)
}

// dirtyFiles lists every uncommitted file (untracked directories expanded) with
// its line counts, less the untracked files in skip. It runs off the worktree
// lock, so git mustn't take index.lock to refresh the index meanwhile.
func dirtyFiles(ctx context.Context, dir string, skip []string) ([]DirtyFile, error) {
	st, err := proc.Run(ctx, dir, "git", "--no-optional-locks", "status", "--porcelain", "-z", "-uall")
	if err != nil {
		return nil, err
	}
	nums, err := proc.Run(ctx, dir, "git", "--no-optional-locks", "diff", "--numstat", "-z", "--no-renames", "HEAD")
	if err != nil {
		return nil, err
	}
	type count struct {
		add, del int
		bin      bool
	}
	counts := map[string]count{}
	for _, e := range strings.Split(nums, "\x00") {
		f := strings.SplitN(e, "\t", 3)
		if len(f) < 3 {
			continue
		}
		a, errA := strconv.Atoi(f[0])
		d, _ := strconv.Atoi(f[1])
		counts[f[2]] = count{a, d, errA != nil} // "-" for a binary file
	}
	files := []DirtyFile{}
	entries := strings.Split(st, "\x00")
	for i := 0; i < len(entries) && len(files) < maxDirtyFiles; i++ {
		e := entries[i]
		if i == 0 && len(e) > 2 && e[2] != ' ' { // the trimmed output lost the leading space of " M"
			e = " " + e
		}
		if len(e) < 4 {
			continue
		}
		f := DirtyFile{Path: e[3:], Code: statusCode(e[:2])}
		if e[0] == 'R' || e[0] == 'C' {
			i++ // -z puts a rename's old path in the next entry
		}
		if f.Code == "??" && slices.Contains(skip, f.Path) {
			continue
		}
		if f.Code == "??" {
			f.Add, f.Binary = untrackedLines(filepath.Join(dir, f.Path))
		} else {
			c := counts[f.Path]
			f.Add, f.Del, f.Binary = c.add, c.del, c.bin
		}
		files = append(files, f)
	}
	return files, nil
}

func statusCode(xy string) string {
	if xy == "??" {
		return "??"
	}
	c := xy[0]
	if c == ' ' {
		c = xy[1]
	}
	switch c {
	case 'A', 'D':
		return string(c)
	case 'R', 'C':
		return "R"
	}
	return "M"
}

// untrackedLines counts a regular file's lines; a NUL in its first 8 KB makes it binary.
func untrackedLines(p string) (int, bool) {
	fi, err := os.Lstat(p)
	if err != nil || !fi.Mode().IsRegular() {
		return 0, false
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return 0, false
	}
	return countLines(b)
}

// dirtyFile is the stored file path names, if the handoff is needs-human with
// that file in its dirty status.
func (m *Manager) dirtyFile(id, path string) (Handoff, DirtyFile, error) {
	h, ok := m.Get(id)
	if !ok {
		return h, DirtyFile{}, ErrNotFound
	}
	if h.State != StateNeedsHuman || h.Dirty == nil {
		return h, DirtyFile{}, invalid("handoff %s has no uncommitted changes on record", id)
	}
	i := slices.IndexFunc(h.Dirty.Files, func(f DirtyFile) bool { return f.Path == path })
	if i < 0 {
		return h, DirtyFile{}, invalid("%q is not in the dirty status", path)
	}
	return h, h.Dirty.Files[i], nil
}

// DirtyDiff is path's change against HEAD as text: git diff for a tracked file,
// the content of an untracked one. Cut at maxShownDiff.
func (m *Manager) DirtyDiff(ctx context.Context, id, path string) (string, error) {
	h, f, err := m.dirtyFile(id, path)
	if err != nil {
		return "", err
	}
	if f.Binary {
		return "binary file", nil
	}
	var out string
	if f.Code == "??" {
		p := filepath.Join(h.Worktree, f.Path)
		if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() { // a symlink could point anywhere
			return "not a regular file", nil
		}
		fh, err := os.Open(p)
		if err != nil {
			return "", err
		}
		defer fh.Close()
		b, err := io.ReadAll(io.LimitReader(fh, maxShownDiff+1))
		if err != nil {
			return "", err
		}
		if bytes.IndexByte(b, 0) >= 0 {
			return "binary file", nil
		}
		out = string(b)
	} else if out, err = proc.Run(ctx, h.Worktree, "git", "--literal-pathspecs", "diff", "HEAD", "--", f.Path); err != nil {
		return "", err
	}
	return clip(out, maxShownDiff, "\n… cut short"), nil
}

// DirtyCommit commits paths of a needs-human handoff's dirty worktree with msg,
// pushes, and resumes the handoff. hash is the Dirty.Hash the page showed.
func (m *Manager) DirtyCommit(ctx context.Context, id, hash string, paths []string, msg string) (Handoff, error) {
	msg = strings.TrimSpace(msg)
	if msg == "" {
		return Handoff{}, invalid("empty commit message")
	}
	if len(msg) > maxCommitMsg {
		return Handoff{}, invalid("commit message over %d bytes", maxCommitMsg)
	}
	if len(paths) == 0 {
		return Handoff{}, invalid("no files to commit")
	}
	for _, p := range paths {
		if _, _, err := m.dirtyFile(id, p); err != nil {
			return Handoff{}, err
		}
	}
	subject, _, _ := strings.Cut(msg, "\n")
	_, err := m.dirtyAct(ctx, id, hash, fmt.Sprintf("commit %d files: %s", len(paths), subject), func(h Handoff) error {
		return m.commitHuman(ctx, h.Worktree, msg, paths, true)
	}, nil)
	if err != nil {
		return Handoff{}, err
	}
	return m.Resume(ctx, id)
}

// defaultDirtyTask is what the agent is asked when the human doesn't say.
const defaultDirtyTask = "Finish this work in progress so it is ready to push: complete what is half done, " +
	"run the relevant tests and make them pass, and drop anything that clearly doesn't belong in this pull request."

// DirtyAgent hands the dirty worktree to an agent: every dirty file goes into a
// local commit (msg; not pushed), and a fix agent works from it on task. The run
// ends like any agent round: its work is committed on top and both are pushed,
// then the handoff resumes. If it fails, the commit is unwrapped again, so the
// human's changes are back uncommitted (see dirtyAgentFinished).
func (m *Manager) DirtyAgent(ctx context.Context, id, hash, msg, task string) (Handoff, error) {
	if m.Tasks == nil {
		return Handoff{}, invalid("agent runs are off: reviews are disabled")
	}
	msg, task = strings.TrimSpace(msg), strings.TrimSpace(task)
	if msg == "" {
		return Handoff{}, invalid("empty commit message")
	}
	if len(msg) > maxCommitMsg || len(task) > maxAgentTask {
		return Handoff{}, invalid("commit message or task too long")
	}
	if task == "" {
		task = defaultDirtyTask
	}
	h, ok := m.Get(id)
	if !ok {
		return h, ErrNotFound
	}
	if h.Dirty == nil {
		return h, invalid("handoff %s has no uncommitted changes on record", id)
	}
	if _, busy := m.running.get(id); busy {
		return h, invalid("an agent is already running on %s", id)
	}
	var paths []string
	for _, f := range h.Dirty.Files {
		paths = append(paths, f.Path)
	}
	var wip string
	subject, _, _ := strings.Cut(msg, "\n")
	h, err := m.dirtyAct(ctx, id, hash, "agent started on "+strconv.Itoa(len(paths))+" files, committed locally as: "+subject, func(h Handoff) error {
		if err := m.commitHuman(ctx, h.Worktree, msg, paths, false); err != nil {
			return err
		}
		var err error
		wip, err = proc.Run(ctx, h.Worktree, "git", "rev-parse", "HEAD")
		return err
	}, func(d *Dirty) { d.WIP = wip })
	if err != nil {
		return h, err
	}
	m.mu.Lock()
	life := m.life
	m.mu.Unlock()
	if life == nil {
		life = context.Background() // tests tick by hand
	}
	if !m.launch(life, &h, TriggerDirty, func(ctx context.Context, h Handoff) outcome { return m.dirtyAgent(ctx, h, wip, task) }) {
		m.unwrapWIP(ctx, h, wip)
		return h, invalid("an agent is already running on %s", id)
	}
	m.changed()
	m.Kick() // only now: a tick before launch would see the WIP commit and no run
	return h, nil
}

// dirtyAgent is the agent run DirtyAgent starts, from the WIP commit at wip.
func (m *Manager) dirtyAgent(ctx context.Context, h Handoff, wip, task string) outcome {
	var fx effects
	reason := m.runAgent(ctx, h, agentJob{kind: "agent", head: wip, base: h.Base,
		build: func(diff string, truncated bool) (string, map[string]string, error) {
			show, _ := proc.Run(ctx, h.Worktree, "git", "show", "--format=", wip)
			show = clip(show, maxDiffPrompt, "\n… cut short")
			prompt, err := render(wipPrompt, wipPromptData{Title: h.Title, Body: h.Body, Branch: h.Branch, Base: h.Base,
				WIP: show, Task: task, Diff: diff, DiffTruncated: truncated})
			return prompt, nil, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out AgentResult
			if r := decode("agent", raw, &out); r != "" {
				return r
			}
			reason, err := m.commitAgentWork(ctx, h, out, before)
			if err != nil {
				return "writing the agent's work failed: " + err.Error()
			}
			if reason == reasonNoChanges { // the work in progress was done already: push it alone
				if _, err := proc.Run(ctx, h.Worktree, "git", "-c", "core.hooksPath=/dev/null", "push", "-q"); err != nil {
					return "push failed: " + err.Error()
				}
			} else if reason != "" {
				return reason
			}
			fx.log("agent", "agent: %s (confidence %s)", cmp.Or(out.Summary, "no changes"), out.Confidence)
			fx.log("commit", "pushed %s", m.headShort(ctx, h))
			return ""
		})
	if reason != "" {
		if m.unwrapWIP(ctx, h, wip) {
			reason += "; your changes are uncommitted again"
		} else {
			reason += fmt.Sprintf("; your changes are in local commit %.12s", wip)
		}
	}
	return outcome{reason: reason, fx: fx}
}

// dirtyAgentFinished applies a DirtyAgent run: pushed means the handoff resumes;
// otherwise it stays with the human, saying why.
func (m *Manager) dirtyAgentFinished(ctx context.Context, h *Handoff, reason string) {
	if h.Dirty != nil {
		d := *h.Dirty
		d.WIP = ""
		h.Dirty = &d
	}
	if reason != "" {
		logEvent(h, m.now(), "dirty", "agent failed: "+reason)
		h.Reason = "agent failed: " + reason
		return
	}
	m.unescalate(h)
	m.dropNeedsHumanLabel(ctx, *h)
}

// unwrapWIP undoes the WIP commit at wip, keeping its changes in the worktree,
// if it is still the unpushed head. It reports whether it did.
func (m *Manager) unwrapWIP(ctx context.Context, h Handoff, wip string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		return false
	}
	defer unlock()
	if head, err := proc.Run(ctx, h.Worktree, "git", "rev-parse", "HEAD"); err != nil || head != wip {
		return false
	}
	if on, _ := proc.Run(ctx, h.Worktree, "git", "branch", "-r", "--contains", wip); on != "" {
		return false // pushed after all
	}
	_, err = proc.Run(ctx, h.Worktree, "git", "reset", "-q", "--mixed", "HEAD~1")
	return err == nil
}

// DirtyStash stashes all of the dirty worktree, untracked files included. It
// doesn't resume: the page then offers the usual Resume.
func (m *Manager) DirtyStash(ctx context.Context, id, hash string) (Handoff, error) {
	return m.kicked(m.dirtyAct(ctx, id, hash, "stash all", func(h Handoff) error {
		args := append(hostGit(ctx, h.Worktree), "stash", "push", "-q", "-u", "-m", "tlm: "+h.Branch+" WIP", "--", ".")
		for _, f := range h.Untracked {
			args = append(args, ":(exclude,literal)"+f)
		}
		_, err := proc.Run(ctx, h.Worktree, "git", args...)
		return err
	}, nil))
}

// DirtyLeave collapses the dirty-worktree panel until the status changes.
func (m *Manager) DirtyLeave(ctx context.Context, id, hash string) (Handoff, error) {
	return m.kicked(m.dirtyAct(ctx, id, hash, "left to the human", nil, func(d *Dirty) { d.Dismissed = true }))
}

// kicked starts a tick after a dirty action that succeeded, so the panel catches up.
func (m *Manager) kicked(h Handoff, err error) (Handoff, error) {
	if err == nil {
		m.Kick()
	}
	return h, err
}

// dirtyAct runs fn under the worktree lock if the worktree is still as the page
// showed it, then logs what as a "dirty" event and applies mark on success.
func (m *Manager) dirtyAct(ctx context.Context, id, hash, what string, fn func(Handoff) error, mark func(*Dirty)) (Handoff, error) {
	h, ok := m.Get(id)
	if !ok {
		return h, ErrNotFound
	}
	if h.State != StateNeedsHuman || h.Dirty == nil {
		return h, invalid("handoff %s has no uncommitted changes on record", id)
	}
	if h.Dirty.Hash() != hash {
		return h, ErrStale
	}
	unlock, err := m.lockWorktree(ctx, id, h.Worktree)
	if err != nil {
		return h, err
	}
	st, err := status(ctx, h)
	if err == nil && st != h.Dirty.Status {
		err = ErrStale
	}
	if err == nil && fn != nil {
		err = fn(h)
	}
	unlock()
	if errors.Is(err, ErrStale) {
		m.Kick()
		return h, err
	}
	verb, _, _ := strings.Cut(what, " ")
	text := what
	if err != nil {
		text = verb + " failed: " + err.Error()
	}
	h, uerr := m.update(id, func(h *Handoff) error {
		logEvent(h, m.now(), "dirty", text)
		if err == nil && mark != nil && h.Dirty != nil {
			d := *h.Dirty
			mark(&d)
			h.Dirty = &d
		}
		return nil
	})
	if err != nil {
		return h, err
	}
	if uerr != nil {
		return h, uerr
	}
	slog.Info("lastmile dirty-worktree action", "id", id, "action", verb)
	m.changed()
	return h, nil
}

// commitHuman commits and pushes what's changed under paths with TLM's write-path
// checks, minus the line cap: it's the human's work. Unlike commitAgentWork it
// never resets the worktree; a failure unstages, or uncommits, and stops.
func (m *Manager) commitHuman(ctx context.Context, dir, msg string, paths []string, push bool) error {
	all, err := changedFiles(ctx, dir, nil) // paths are the panel's, which has no baseline files
	if err != nil {
		return err
	}
	var files []string
	for _, f := range all {
		if slices.Contains(paths, f) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		return invalid("nothing to commit under %s", strings.Join(paths, " "))
	}
	for _, f := range files {
		if why := m.pathProblem(f); why != "" {
			return invalid("refused: %s", strings.TrimPrefix(why, "agent "))
		}
		if fi, err := os.Lstat(filepath.Join(dir, f)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return invalid("refused: %s is a symlink", f)
		}
	}
	if _, binary, err := changedLines(ctx, dir, files); err != nil {
		return err
	} else if binary != "" {
		return invalid("refused: %s is binary", binary)
	}
	unstage := func() {
		_, _ = proc.Run(ctx, dir, "git", append([]string{"--literal-pathspecs", "reset", "-q", "--"}, files...)...)
	}
	if _, err := proc.Run(ctx, dir, "git", append([]string{"--literal-pathspecs", "add", "--"}, files...)...); err != nil {
		unstage()
		return fmt.Errorf("git add: %w", err)
	}
	commit := append(hostGit(ctx, dir), "--literal-pathspecs", "commit", "-q", "-m", msg, "--")
	if _, err := proc.Run(ctx, dir, "git", append(commit, files...)...); err != nil {
		unstage()
		return fmt.Errorf("commit: %w", err)
	}
	if !push {
		return nil
	}
	if _, err := proc.Run(ctx, dir, "git", "-c", "core.hooksPath=/dev/null", "push", "-q"); err != nil {
		_, _ = proc.Run(ctx, dir, "git", "reset", "-q", "--soft", "HEAD~1")
		unstage()
		return fmt.Errorf("push: %w", err)
	}
	return nil
}
