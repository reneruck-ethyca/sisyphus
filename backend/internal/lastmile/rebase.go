package lastmile

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// restackSpec is one rebase of a handoff's branch (design §3, §4).
type restackSpec struct {
	onto     string // the base branch to land on, as origin/<onto>
	upstream string // the old base point for `rebase --onto`; "" = a plain rebase onto origin/<onto>
	head     string // the head the PR shows; the worktree must be at it
}

// rebaseInProgress says dir has a rebase stopped in it.
func rebaseInProgress(ctx context.Context, dir string) bool {
	for _, n := range []string{"rebase-merge", "rebase-apply"} {
		p, err := proc.Run(ctx, dir, "git", "rev-parse", "--path-format=absolute", "--git-path", n)
		if err != nil {
			return false
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// abortStaleRebase backs out a rebase a conflict run lost to a restart left in the
// worktree (see lostRun): it would wedge every later git command. Only then: a rebase
// with no such run behind it is a human's (resolving a needs-human conflict) and stays.
func (m *Manager) abortStaleRebase(ctx context.Context, h *Handoff) {
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		return
	}
	defer unlock()
	if !rebaseInProgress(ctx, h.Worktree) {
		return
	}
	if _, err := proc.Run(ctx, h.Worktree, "git", "rebase", "--abort"); err != nil {
		slog.Warn("lastmile aborting a stale rebase failed", "id", h.ID, "err", err)
		return
	}
	logEvent(h, m.now(), "rebase", "aborted a rebase left in progress (a crash)")
}

var treeID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// mergeTree predicts a rebase onto origin/<base>: the files `git merge-tree` reports in
// conflict, none when it merges cleanly. It writes objects, never the worktree.
func mergeTree(ctx context.Context, dir, base string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, proc.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--name-only", "-z", "origin/"+base, "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && (!errors.As(err, &ee) || ee.ExitCode() != 1) { // 1 = conflicts
		return nil, fmt.Errorf("git merge-tree: %w", err)
	}
	if err == nil {
		return nil, nil
	}
	var files []string
	parts := strings.Split(string(out), "\x00") // tree id, the conflicted names, an empty entry, then messages
	if !treeID.MatchString(parts[0]) {          // exit 1 is also what a bad ref gets, with the complaint on stderr
		return nil, fmt.Errorf("git merge-tree: %s", strings.TrimSpace(string(out)+" "+string(ee.Stderr)))
	}
	for _, f := range parts[1:] {
		if f == "" {
			break
		}
		if !slices.Contains(files, f) {
			files = append(files, f)
		}
	}
	if len(files) == 0 {
		files = []string{"(unnamed)"}
	}
	return files, nil
}

// drift handles a PR whose base moved (design §4): only when GitHub says it is behind or
// in conflict, so a busy base doesn't restart CI on every push. A rebase that merge-tree
// predicts clean runs in the background; a conflicting one needs the conflict agent
// (fix-ci), else a human. It returns true when it took over this tick.
func (m *Manager) drift(ctx context.Context, h *Handoff, s Snapshot, base string) (bool, error) {
	if stale := s.MergeState == "BEHIND" || s.MergeState == "DIRTY" || s.Mergeable == "CONFLICTING"; s.Behind == 0 || !stale {
		return false, nil
	}
	if head, err := m.lockedGit(ctx, *h, "rev-parse", "HEAD"); err != nil || head != s.Head {
		return false, err // the PR isn't what the worktree has: leave it to the agents' own checks
	}
	conflicts, err := mergeTree(ctx, h.Worktree, base)
	if err != nil {
		return false, err
	}
	files := strings.Join(conflicts, ", ")
	if len(conflicts) > 0 && !h.Autonomy.FixCI {
		h.EscalatedBy = TriggerConflict
		m.escalate(ctx, h, "rebase conflict: "+files)
		return true, nil
	}
	spec := restackSpec{onto: base, head: s.Head}
	if !m.launch(ctx, h, TriggerConflict, func(ctx context.Context, snap Handoff) outcome { return m.restack(ctx, snap, spec) }) {
		return true, nil
	}
	if len(conflicts) > 0 {
		logEvent(h, m.now(), "rebase", "base moved, conflicts predicted in "+files)
		m.setState(h, StateConflict, "resolving a rebase conflict in "+files)
	} else {
		logEvent(h, m.now(), "rebase", "base moved, no conflict predicted: rebasing onto origin/"+base)
		m.setState(h, StateCIRunning, "rebasing onto "+base)
	}
	return true, nil
}

// stack handles a child of a stack (design §3). While the parent is open it notes the
// parent's head, the old base point once a squash merge hides it. When the parent
// merged it notifies, retargets the PR to the parent's own base and rebases onto it;
// when it was closed or cancelled the child needs a human. true = took over this tick.
func (m *Manager) stack(ctx context.Context, h *Handoff, p github.PRSnapshot, s Snapshot) (bool, error) {
	if h.StackParent == "" {
		return false, nil
	}
	parent, ok := m.Get(h.StackParent)
	if !ok {
		return false, nil
	}
	switch parent.State {
	case StateClosed, StateCancelled:
		h.StackParent, h.ParentHead = "", "" // a resumed handoff is a plain one on its base
		h.EscalatedBy = ""
		m.escalate(ctx, h, "stack parent closed: "+parent.ID)
		return true, nil
	case StateMerged:
	default:
		// Only a parent head the branch is built on: after the parent rewrote its history
		// (its own rebase) the old head is still where this branch forked off.
		if sha, err := m.lockedGit(ctx, *h, "rev-parse", "origin/"+h.Base); err == nil && sha != "" {
			if _, err := m.lockedGit(ctx, *h, "merge-base", "--is-ancestor", sha, "HEAD"); err == nil {
				h.ParentHead = sha
			}
		}
		return false, nil
	}
	h.ParentMerged = true // afterCommit sends stack-parent-merged on this change
	newBase := parent.Base
	if cmp.Or(p.BaseRef, h.Base) != newBase {
		if err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{Base: newBase}); err != nil {
			return true, fmt.Errorf("retargeting to %s: %w", newBase, err)
		}
		logEvent(h, m.now(), "retarget", fmt.Sprintf("parent %s merged: PR base %s -> %s", parent.ID, h.Base, newBase))
	}
	h.Base = newBase
	upstream := h.ParentHead
	if upstream == "" && parent.Snapshot != nil {
		upstream = parent.Snapshot.Head
	}
	if upstream == "" {
		h.EscalatedBy = TriggerConflict
		m.escalate(ctx, h, "stack parent merged, but its last head is unknown, so the rebase has no old base point")
		return true, nil
	}
	spec := restackSpec{onto: newBase, upstream: upstream, head: s.Head}
	if m.launch(ctx, h, TriggerConflict, func(ctx context.Context, snap Handoff) outcome { return m.restack(ctx, snap, spec) }) {
		m.setState(h, StateCIRunning, "rebasing onto "+newBase)
	}
	return true, nil
}

// restack is the background work of a rebase: it takes the worktree lock, rebases on
// the host, resolves conflicts with the agent, and force-pushes with a lease.
func (m *Manager) restack(ctx context.Context, h Handoff, r restackSpec) outcome {
	var fx effects
	reason := m.restackRun(ctx, h, r, &fx)
	fx.unstack = reason == "" && r.upstream != ""
	return outcome{reason: reason, fx: fx}
}

func (m *Manager) restackRun(ctx context.Context, h Handoff, r restackSpec, fx *effects) string {
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		return "rebase: " + err.Error()
	}
	defer unlock()
	dir := h.Worktree
	if st, err := status(ctx, h); err != nil || st != "" {
		return "rebase: worktree has uncommitted changes"
	}
	pre, err := worktree.HeadSHA(ctx, dir)
	if err != nil || pre != r.head {
		return fmt.Sprintf("rebase: worktree is at %.12s, the PR at %.12s", pre, r.head)
	}
	// Whatever way out, the worktree is never left mid-rebase: back to where it was.
	rollBack := func() {
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		_, _ = proc.Run(c, dir, "git", "rebase", "--abort")
		if err := reset(c, h, pre); err != nil {
			slog.Warn("lastmile rebase rollback failed", "id", h.ID, "err", err)
		}
	}
	if _, err := proc.Run(ctx, dir, "git", "fetch", "--quiet", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", r.onto, r.onto)); err != nil {
		return "rebase: " + err.Error()
	}
	onto := "origin/" + r.onto
	args := []string{"rebase", onto}
	if r.upstream != "" {
		args = []string{"rebase", "--onto", onto, r.upstream, h.Branch}
	}
	// Hooks are off for every rebase: a post-rewrite or post-checkout hook from the user's
	// config, or from files the agent wrote, would run on each one.
	rb := append(hostGit(ctx, dir), "-c", "core.editor=true", "-c", "sequence.editor=true")
	if _, err := proc.Run(ctx, dir, "git", append(slices.Clone(rb), args...)...); err != nil && !rebaseInProgress(ctx, dir) {
		rollBack()
		return "rebase failed: " + err.Error()
	}
	stops := 0
	for rebaseInProgress(ctx, dir) {
		files, err := unmergedFiles(ctx, dir)
		if err != nil || len(files) == 0 {
			rollBack()
			return fmt.Sprintf("rebase stopped without conflicts to resolve (%v)", err)
		}
		stops++
		if why := m.resolveStop(ctx, h, r, rb, files, fx); why != "" {
			fx.log("rebase", "conflict in %s: %s; rebase aborted", strings.Join(files, ", "), why)
			rollBack()
			return strings.TrimSuffix("rebase conflict: "+strings.Join(files, ", ")+" ("+why+")", " ()")
		}
	}
	// ponytail: a resolution that leaves a commit empty makes `rebase --continue` stop; that
	// lands in the "without conflicts" branch above and escalates instead of --skip.
	if stops == 0 {
		fx.log("rebase", "rebased onto %s cleanly", onto)
	} else {
		fx.log("rebase", "rebased onto %s, %d conflict stop(s) resolved by the conflict agent", onto, stops)
	}
	ref := "refs/heads/" + h.Branch
	if _, err := proc.Run(ctx, dir, "git", "-c", "core.hooksPath=/dev/null", "push", "--force-with-lease="+ref+":"+pre, "origin", ref+":"+ref); err != nil {
		rollBack() // the remote isn't what we saw: put the worktree back with it
		return "push failed (force-with-lease): " + err.Error()
	}
	fx.log("push", "force-pushed %s with lease (was %.7s)", h.Branch, pre)
	return ""
}

// unmergedFiles lists the paths the stopped rebase has in conflict.
func unmergedFiles(ctx context.Context, dir string) ([]string, error) {
	out, err := proc.Run(ctx, dir, "git", "diff", "--name-only", "--diff-filter=U", "-z")
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, f)
		}
	}
	return files, nil
}

// resolveStop gets the rebase past one stop: the conflict agent resolves the files and
// the host continues. It returns "" when it moved on, else why it didn't (the caller
// aborts). The caller holds the worktree lock.
func (m *Manager) resolveStop(ctx context.Context, h Handoff, r restackSpec, rb, files []string, fx *effects) string {
	switch {
	case !h.Autonomy.FixCI:
		return "fix-ci is off"
	case m.Tasks == nil:
		return "agents need reviews enabled"
	case h.FixRounds[TriggerConflict]+fx.rounds >= h.MaxFixRounds:
		return fmt.Sprintf("still conflicting after %d rounds", h.FixRounds[TriggerConflict]+fx.rounds)
	}
	for _, f := range files {
		if m.protected(f) {
			return "a protected path is in conflict"
		}
	}
	fx.rounds++
	dir := h.Worktree
	run := func(args ...string) string { out, _ := proc.Run(ctx, dir, "git", args...); return out }
	pathspec := append([]string{"--literal-pathspecs", "diff", "--"}, files...)
	diff := clip(run(pathspec...), maxDiffPrompt, "\n[truncated]")
	replayed := run("show", "-s", "--format=%h %s%n%n%b", "REBASE_HEAD")
	ctxFiles := map[string]string{"conflict.diff": diff}
	var list strings.Builder
	for i, f := range files {
		ours, theirs := fmt.Sprintf("ours-%d", i), fmt.Sprintf("theirs-%d", i)
		ctxFiles[ours] = clip(run("show", ":2:"+f), 256<<10, "\n[truncated]")
		ctxFiles[theirs] = clip(run("show", ":3:"+f), 256<<10, "\n[truncated]")
		fmt.Fprintf(&list, "- %s: /sisyphus/context/%s (the base's side), /sisyphus/context/%s (the replayed commit's side)\n", f, ours, theirs)
	}
	snapJSON, _ := json.MarshalIndent(map[string]any{"branch": h.Branch, "onto": "origin/" + r.onto, "files": files}, "", "  ")
	ctxFiles["conflict.json"] = string(snapJSON)
	return m.runAgent(ctx, h, agentJob{kind: "conflict agent", held: true, base: r.onto,
		build: func(patch string, truncated bool) (string, map[string]string, error) {
			prompt, err := render(conflictPrompt, conflictPromptData{Title: h.Title, Branch: h.Branch, Onto: "origin/" + r.onto, Replayed: replayed,
				Files: list.String(), ConflictDiff: diff, Diff: patch, DiffTruncated: truncated})
			return prompt, ctxFiles, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out AgentResult
			if why := decode("conflict agent", raw, &out); why != "" {
				return why
			}
			if why := m.checkResolution(ctx, dir, files, h.Untracked, out, before); why != "" {
				return why
			}
			add := append([]string{"--literal-pathspecs", "add", "--"}, files...)
			if _, err := proc.Run(ctx, dir, "git", add...); err != nil {
				return "git add failed: " + err.Error()
			}
			fx.log("agent", "conflict agent resolved %s: %s (confidence %s)", strings.Join(files, ", "), out.Summary, out.Confidence)
			if _, err := proc.Run(ctx, dir, "git", append(slices.Clone(rb), "rebase", "--continue")...); err != nil && !rebaseInProgress(ctx, dir) {
				return "rebase --continue failed: " + err.Error()
			}
			return ""
		})
}

// markers are the lines a git conflict leaves in a file.
var markers = regexp.MustCompile(`(?m)^(<<<<<<<|>>>>>>>|=======\r?$)`)

// checkResolution holds the agent's edits to the write path's rules (design §5): it
// edited only the files in conflict, none is protected, nested or a link, and no
// conflict marker is left. A non-empty result is why the resolution is refused.
func (m *Manager) checkResolution(ctx context.Context, dir string, files, untracked []string, res AgentResult, before map[string]stamp) string {
	ignored, err := ignoredChanges(ctx, dir, before)
	if err != nil {
		return "listing ignored files: " + err.Error()
	}
	if len(ignored) > 0 {
		return "agent created or changed gitignored files (new ones removed): " + pathList(ignored)
	}
	if res.Escalate {
		return "agent escalated: " + res.EscalateReason
	}
	// Against the index, not HEAD: the replayed commit's clean changes are staged already.
	var edited []string
	for _, args := range [][]string{{"diff", "--name-only", "-z", "--no-renames"}, {"ls-files", "-z", "--others", "--exclude-standard"}} {
		out, err := proc.Run(ctx, dir, "git", args...)
		if err != nil {
			return "inspecting the worktree: " + err.Error()
		}
		edited = append(edited, strings.Split(out, "\x00")...)
	}
	for _, f := range edited {
		if f != "" && !slices.Contains(files, f) && !slices.Contains(untracked, f) {
			return "agent edited a file that wasn't in conflict: " + f
		}
	}
	for _, f := range files {
		if why := m.pathProblem(f); why != "" {
			return why
		}
		fi, err := os.Lstat(filepath.Join(dir, f))
		if err != nil {
			continue // deleted: a valid way to resolve a modify/delete conflict
		}
		if !fi.Mode().IsRegular() {
			return "agent left something other than a regular file: " + f
		}
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			return "reading " + f + ": " + err.Error()
		}
		if markers.Match(b) {
			return "conflict markers are left in " + f
		}
	}
	return ""
}

// clip cuts s to n bytes, on a character boundary.
