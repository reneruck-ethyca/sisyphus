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
	green    bool   // CI was green on head: a clean rebase plus a bump is then downrev-only
	unstack  bool   // the parent merged: once rebased, the child is a plain handoff
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

// started is when this process started; a git lock older than that belongs to a
// git the last sisyphus ran and was killed with it.
var started = time.Now()

// clearStaleIndexLock removes dir's index.lock when it predates this process: a
// git killed mid-rebase leaves it, and every later git command fails on it. The
// caller holds the worktree lock.
func clearStaleIndexLock(ctx context.Context, dir string) {
	p, err := proc.Run(ctx, dir, "git", "rev-parse", "--path-format=absolute", "--git-path", "index.lock")
	if err != nil {
		return
	}
	if fi, err := os.Stat(p); err == nil && fi.ModTime().Before(started) {
		if err := os.Remove(p); err == nil {
			slog.Info("lastmile removed a stale index.lock", "path", p)
		}
	}
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
	clearStaleIndexLock(ctx, h.Worktree)
	if _, err := proc.Run(ctx, h.Worktree, "git", "rebase", "--abort"); err != nil {
		slog.Warn("lastmile aborting a stale rebase failed", "id", h.ID, "err", err)
		return
	}
	logEvent(h, m.now(), "rebase", "aborted a rebase left in progress (a crash)")
}

var treeID = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// mergeTree predicts a rebase onto origin/<base>, of upstream..HEAD when upstream is
// set: the files `git merge-tree` reports in conflict, none when it merges cleanly. It
// writes objects, never the worktree.
func mergeTree(ctx context.Context, dir, base, upstream string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, proc.Timeout)
	defer cancel()
	args := []string{"merge-tree", "--write-tree", "--name-only", "-z"}
	if upstream != "" {
		args = append(args, "--merge-base="+upstream)
	}
	cmd := exec.CommandContext(ctx, "git", append(args, "origin/"+base, "HEAD")...)
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
	asked := h.RebaseAsked
	if s.Behind == 0 {
		h.RebaseAsked = "" // nothing to rebase onto
	}
	if stale := asked != "" || s.MergeState == "BEHIND" || s.MergeState == "DIRTY" || s.Mergeable == "CONFLICTING"; s.Behind == 0 || !stale {
		return false, nil
	}
	if head, err := m.lockedGit(ctx, *h, "rev-parse", "HEAD"); err != nil || head != s.Head {
		return false, err // the PR isn't what the worktree has: leave it to the agents' own checks
	}
	// A stack child whose open parent rewrote its history (a rebase after its own
	// parent's squash merge) still holds the parent's old commits: replay only its own.
	var upstream string
	if h.StackParent != "" && h.ParentHead != "" {
		if _, err := m.lockedGit(ctx, *h, "merge-base", "--is-ancestor", h.ParentHead, "HEAD"); err == nil {
			upstream = h.ParentHead
		}
	}
	// The same under a base TLM doesn't track as a parent, rewritten or merged (GitHub then
	// retargets the PR, and h.Base still names the old branch): the old base ref's reflog
	// still knows the tip the branch forked off.
	if upstream == "" {
		fork, _ := m.lockedGit(ctx, *h, "merge-base", "--fork-point", "origin/"+cmp.Or(h.Base, base), "HEAD")
		if mb, _ := m.lockedGit(ctx, *h, "merge-base", "origin/"+base, "HEAD"); fork != "" && fork != mb {
			upstream = fork
		}
	}
	conflicts, err := mergeTree(ctx, h.Worktree, base, upstream)
	if err != nil {
		return false, err
	}
	files := strings.Join(conflicts, ", ")
	if len(conflicts) > 0 && !h.Autonomy.FixCI {
		h.EscalatedBy = TriggerConflict
		m.escalate(ctx, h, "rebase conflict: "+files)
		return true, nil
	}
	spec := restackSpec{onto: base, upstream: upstream, head: s.Head, green: s.CI == CIGreen}
	if !m.launch(ctx, h, TriggerConflict, func(ctx context.Context, snap Handoff) outcome { return m.restack(ctx, snap, spec) }) {
		return true, nil
	}
	h.RebaseAsked = ""
	if asked != "" {
		logEvent(h, m.now(), "rebase", "rebase asked by "+asked)
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

// detectStack links h to the PR below it when the handoff named none (design §3): an
// open handoff whose branch is h's base, else an open PR nobody handed off, which TLM
// only watches (looked up once per base). A protected base is a long-lived branch, not a parent.
func (m *Manager) detectStack(ctx context.Context, h *Handoff, p github.PRSnapshot) error {
	if p.Stack != nil {
		h.StackNumber = p.Stack.Number
	}
	base := p.BaseRef
	if h.StackParent != "" || base == "" || p.BaseProtected {
		return nil
	}
	hs := m.List()
	if i := slices.IndexFunc(hs, func(o Handoff) bool {
		return o.ID != h.ID && o.Repo == h.Repo && o.Branch == base && !Terminal(o.State)
	}); i >= 0 {
		h.StackParent, h.Base, h.ParentPR, h.ChainBase = hs[i].ID, base, 0, base
		logEvent(h, m.now(), "stack", "joined the stack under "+hs[i].ID)
		return nil
	}
	if base == h.ChainBase || h.ParentPR != 0 { // a watched parent is stack()'s until it merges or closes
		return nil
	}
	ref, ok, err := m.GH.FindPR(ctx, h.Repo, base)
	if err != nil {
		return err
	}
	h.ChainBase = base
	if ok {
		h.ParentPR, h.Base = ref.Number, base
		logEvent(h, m.now(), "stack", fmt.Sprintf("chained on #%d, which nobody handed off: watched, never merged", ref.Number))
	}
	return nil
}

// below is the PR a stack child sits on: a handoff, or a PR TLM only watches.
type below struct {
	name, state string // state: a handoff state; a watched PR's is mapped onto them
	base, head  string // the parent's base, and its last head
}

// parent reads the PR below h; ok is false when h isn't a stack child.
func (m *Manager) parent(ctx context.Context, h Handoff) (b below, ok bool, err error) {
	switch {
	case h.StackParent != "":
		p, found := m.Get(h.StackParent)
		if !found {
			return b, false, nil
		}
		b = below{name: p.ID, state: p.State, base: p.Base}
		if p.Snapshot != nil {
			b.head = p.Snapshot.Head
		}
		return b, true, nil
	case h.ParentPR != 0:
		// ponytail: one PR read per tick per watched parent; fine at a handful of chains.
		p, err := m.GH.PRView(ctx, github.PRRef{Repo: h.Repo, Number: h.ParentPR})
		if err != nil {
			return b, false, err
		}
		b = below{name: fmt.Sprintf("#%d", h.ParentPR), state: StatePROpen, base: p.BaseRef, head: p.HeadSHA}
		switch p.State {
		case "MERGED":
			b.state = StateMerged
		case "CLOSED":
			b.state = StateClosed
		}
		return b, true, nil
	}
	return b, false, nil
}

// stack handles a child of a stack (design §3). While the parent is open it notes the
// parent's head, the old base point once a squash merge hides it. When the parent
// merged it notifies, retargets the PR to the parent's own base and rebases onto it;
// when it was closed or cancelled the child needs a human. true = took over this tick.
func (m *Manager) stack(ctx context.Context, h *Handoff, p github.PRSnapshot, s Snapshot) (bool, error) {
	parent, ok, err := m.parent(ctx, *h)
	if !ok || err != nil {
		return false, err
	}
	switch parent.state {
	case StateClosed, StateCancelled:
		h.StackParent, h.ParentHead, h.ParentPR = "", "", 0 // a resumed handoff is a plain one on its base
		h.EscalatedBy = ""
		m.escalate(ctx, h, "stack parent closed: "+parent.name)
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
	if h.StackNumber != 0 {
		return true, m.nativeRestacked(ctx, h, p, parent)
	}
	newBase := parent.base
	if cmp.Or(p.BaseRef, h.Base) != newBase {
		if err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{Base: newBase}); err != nil {
			return true, fmt.Errorf("retargeting to %s: %w", newBase, err)
		}
		logEvent(h, m.now(), "retarget", fmt.Sprintf("parent %s merged: PR base %s -> %s", parent.name, h.Base, newBase))
	}
	h.Base = newBase
	upstream := cmp.Or(h.ParentHead, parent.head)
	if upstream == "" {
		h.EscalatedBy = TriggerConflict
		m.escalate(ctx, h, "stack parent merged, but its last head is unknown, so the rebase has no old base point")
		return true, nil
	}
	spec := restackSpec{onto: newBase, upstream: upstream, head: s.Head, unstack: true}
	if m.launch(ctx, h, TriggerConflict, func(ctx context.Context, snap Handoff) outcome { return m.restack(ctx, snap, spec) }) {
		m.setState(h, StateCIRunning, "rebasing onto "+newBase)
	}
	return true, nil
}

// nativeRestacked follows GitHub's own restack of a native stack after the PR below
// merged: TLM neither retargets nor rebases (that would race GitHub's force-push). Once
// GitHub retargeted the PR, the worktree moves to the head GitHub pushed.
func (m *Manager) nativeRestacked(ctx context.Context, h *Handoff, p github.PRSnapshot, parent below) error {
	if p.BaseRef == "" || p.BaseRef == h.Base {
		m.setState(h, h.State, "waiting for GitHub to restack after "+parent.name+" merged")
		return nil
	}
	if err := m.syncWorktree(ctx, *h, p.HeadSHA); err != nil {
		h.EscalatedBy = TriggerConflict
		m.escalate(ctx, h, "the worktree can't follow GitHub's restack: "+err.Error())
		return nil
	}
	logEvent(h, m.now(), "stack", fmt.Sprintf("%s merged: GitHub restacked onto %s; worktree synced to %s", parent.name, p.BaseRef, short(p.HeadSHA)))
	h.Base = p.BaseRef
	h.StackParent, h.ParentHead, h.ParentPR, h.StackNumber = "", "", 0, 0
	return nil
}

// syncWorktree moves h's worktree to head, a push TLM didn't make, when the worktree
// holds nothing of its own: clean, and at what the branch last had on origin.
func (m *Manager) syncWorktree(ctx context.Context, h Handoff, head string) error {
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		return err
	}
	defer unlock()
	git := func(args ...string) (string, error) { return proc.Run(ctx, h.Worktree, "git", args...) }
	if st, err := status(ctx, h); err != nil || st != "" {
		return errors.New("it has uncommitted changes")
	}
	local, err := git("rev-parse", "HEAD")
	if err != nil || local == head {
		return err
	}
	if pushed, _ := git("rev-parse", "--verify", "--quiet", "refs/remotes/origin/"+h.Branch); pushed != local {
		return fmt.Errorf("it has commits that aren't on origin/%s", h.Branch)
	}
	if _, err := git("fetch", "--quiet", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", h.Branch, h.Branch)); err != nil {
		return err
	}
	_, err = git("reset", "--quiet", "--hard", head)
	return err
}

// restack is the background work of a rebase: it takes the worktree lock, rebases on
// the host, resolves conflicts with the agent, and force-pushes with a lease.
func (m *Manager) restack(ctx context.Context, h Handoff, r restackSpec) outcome {
	var fx effects
	reason := m.restackRun(ctx, h, r, &fx)
	fx.unstack = reason == "" && r.unstack
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
		if err := m.reset(c, h, pre); err != nil {
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
	bumped, err := bumpDownrev(ctx, dir, r.onto, m.cfg.LastMile.MigrationPaths)
	if err != nil {
		rollBack()
		return "down_revision bump: " + err.Error()
	}
	if len(bumped) > 0 {
		fx.log("rebase", "pointed %s at the base's migration head", strings.Join(bumped, ", "))
	}
	// CI reads labels off the push's event, so the label goes on before the push.
	label, labelled := "", false
	if stops == 0 && r.green && len(bumped) > 0 {
		label = m.cfg.LastMile.DownrevLabels[h.Repo]
	}
	if label != "" {
		if err := m.setDownrevLabel(ctx, h, label, true); err != nil {
			fx.log("label", "adding %s failed, pushing without it: %v", label, err)
		} else {
			labelled = true
			fx.log("label", "%s added: the bump is the only change", label)
		}
	}
	if !labelled {
		if why := m.unlabelBeforePush(ctx, h, fx); why != "" {
			rollBack()
			return why
		}
	}
	ref := "refs/heads/" + h.Branch
	if _, err := proc.Run(ctx, dir, "git", "-c", "core.hooksPath=/dev/null", "push", "--force-with-lease="+ref+":"+pre, "origin", ref+":"+ref); err != nil {
		rollBack()                            // the remote isn't what we saw: put the worktree back with it
		if labelled && h.DownrevHead != pre { // pre itself may carry the label of an earlier bump
			if err := m.setDownrevLabel(context.WithoutCancel(ctx), h, label, false); err != nil {
				fx.log("label", "removing %s after the failed push failed: %v", label, err)
			}
		}
		return "push failed (force-with-lease): " + err.Error()
	}
	fx.log("push", "force-pushed %s with lease (was %.7s)", h.Branch, pre)
	if labelled {
		fx.downrevFrom = pre
		fx.downrevHead, _ = worktree.HeadSHA(ctx, dir)
	}
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
			prompt, err := render(conflictPrompt, conflictPromptData{PR: h.PRNumber, Notes: m.cfg.Review.Repos[h.Repo].Notes, Title: h.Title, Branch: h.Branch, Onto: "origin/" + r.onto, Replayed: replayed,
				Files: list.String(), ConflictDiff: diff, Diff: patch, DiffTruncated: truncated})
			return prompt, ctxFiles, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out AgentResult
			if why := decode("conflict agent", raw, &out); why != "" {
				return why
			}
			if why := m.checkResolution(ctx, dir, files, h.Untracked, m.scratch(h.Repo), out, before); why != "" {
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
func (m *Manager) checkResolution(ctx context.Context, dir string, files, untracked, scratch []string, res AgentResult, before map[string]stamp) string {
	ignored, err := ignoredChanges(ctx, dir, before, scratch)
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

// short is a sha cut to 12 characters for an event.
func short(sha string) string { return sha[:min(len(sha), 12)] }
