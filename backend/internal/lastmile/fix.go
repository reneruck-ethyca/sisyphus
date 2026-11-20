package lastmile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// TaskRunner runs an agent on a worktree; *review.Reviewer does.
type TaskRunner interface {
	RunTask(ctx context.Context, t review.Task) (review.TaskResult, error)
}

// maxDiffPrompt bounds the diff in the fix prompt.
const maxDiffPrompt = 60 << 10

// effects is what an agent run leaves for the tick to write into the handoff:
// the tick is the only writer, so a run never races with it.
type effects struct {
	events    []Event
	posted    []int64  // comment ids TLM posted
	fixed     []string // threads answered "Fixed in …"
	handed    []Item   // items the cursor moves over
	body      *string  // the PR body TLM wrote
	requested []string // reviewers TLM re-requested
	rounds    int      // conflict agent runs, counted in FixRounds
	failures  []string // fingerprints a fix round was spent on
	unstack   bool     // the child now sits on the parent's old base: it is a plain handoff
	rebase    string   // the agent that asked for a rebase onto the base

	downrevFrom, downrevHead string // a downrev-only restack (design §7.4): the head before it, and the one it pushed
	downrevDropped           bool   // the downrev label came off before a push that didn't qualify for it
}

func (e *effects) log(kind, format string, a ...any) {
	e.events = append(e.events, Event{Time: time.Now(), Kind: kind, Text: fmt.Sprintf(format, a...)})
}

// apply writes the effects into h.
func (e effects) apply(h *Handoff) {
	for _, ev := range e.events {
		logEvent(h, ev.Time, ev.Kind, ev.Text)
	}
	if len(e.posted) > 0 {
		h.Posted = append(slices.Clone(h.Posted), e.posted...)
		if len(h.Posted) > maxPosted {
			h.Posted = h.Posted[len(h.Posted)-maxPosted:]
		}
	}
	h.FixedThreads = union(h.FixedThreads, e.fixed...)
	if len(e.handed) > 0 {
		h.Cursor = advanceCursor(h.Cursor, e.handed)
	}
	if e.body != nil {
		h.LastBodyWritten = *e.body
	}
	h.Requested = union(h.Requested, e.requested...)
	h.FixedFailures = append(slices.Clone(h.FixedFailures), e.failures...)
	for range e.rounds {
		h.FixRounds = bump(h.FixRounds, TriggerConflict)
	}
	if e.rebase != "" {
		h.RebaseAsked = e.rebase
	}
	if e.unstack {
		h.StackParent, h.ParentHead = "", ""
	}
	if e.downrevDropped {
		h.DownrevHead, h.DownrevFrom, h.DownrevRoot = "", "", ""
	}
	if e.downrevHead != "" {
		if h.DownrevHead != e.downrevFrom || h.DownrevRoot == "" { // not a bump on top of a labelled bump
			h.DownrevRoot = e.downrevFrom
		}
		h.DownrevFrom, h.DownrevHead = e.downrevFrom, e.downrevHead
	}
}

// outcome is what an agent run found: reason "" = the work was pushed (or, for a
// review round, answered).
type outcome struct {
	reason string
	fx     effects
}

// agentRun is an agent run in flight for a handoff, or, once finished, what it
// found for the next tick to apply.
type agentRun struct {
	cancel  context.CancelFunc
	trigger string // Trigger*
	started time.Time
	done    bool
	outcome
}

// runs tracks the agent runs by handoff id, in memory: after a restart a run is
// lost, and the next tick starts over from what GitHub shows.
type runs struct {
	mu sync.Mutex
	m  map[string]*agentRun
}

// get returns a copy of the run: its outcome is written by its goroutine.
func (r *runs) get(id string) (agentRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.m[id]; a != nil {
		return agentRun{done: a.done, trigger: a.trigger, started: a.started, outcome: a.outcome}, true
	}
	return agentRun{}, false
}

func (r *runs) put(id string, a *agentRun) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = map[string]*agentRun{}
	}
	if r.m[id] != nil {
		return false
	}
	r.m[id] = a
	return true
}

func (r *runs) drop(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, id)
}

// cancel stops the run of id, if any.
func (r *runs) cancel(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.m[id]; a != nil {
		a.cancel()
	}
}

// Wait blocks until the agent runs started so far are over; for tests and shutdown.
func (m *Manager) Wait() { m.wg.Wait() }

// launch runs work on a copy of h beside the tick loop, for trigger, and marks h
// (and the stored handoff) with the run. It returns false when a run is already
// in flight for h.
func (m *Manager) launch(ctx context.Context, h *Handoff, trigger string, work func(context.Context, Handoff) outcome) bool {
	runCtx, cancel := context.WithCancel(ctx)
	run := &agentRun{cancel: cancel, trigger: trigger, started: m.now()}
	if !m.running.put(h.ID, run) {
		cancel()
		return false
	}
	head, _ := worktree.HeadSHA(ctx, h.Worktree)
	h.Run = &RunMark{Trigger: trigger, Started: run.started, Head: head}
	m.markRun(h.ID, h.Run)
	snap := *h
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		out := work(runCtx, snap)
		if ctx.Err() != nil || runCtx.Err() != nil { // shutdown or cancel: nothing to apply
			m.running.drop(snap.ID)
			return
		}
		m.running.mu.Lock()
		run.done, run.outcome = true, out
		m.running.mu.Unlock()
		m.Kick()
	}()
	return true
}

// markRun stores r on the handoff id before its run starts, without waiting for the
// tick's commit: a crash mid-run must find it. No Gen bump, so the tick isn't discarded.
func (m *Manager) markRun(id string, r *RunMark) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i := slices.IndexFunc(m.handoffs, func(h Handoff) bool { return h.ID == id }); i >= 0 {
		m.handoffs[i].Run = r
		if err := m.saveLocked(); err != nil {
			slog.Error("lastmile save failed", "err", err)
		}
	}
}

// agentFinished applies the outcome of a, a run that ended since the last tick.
func (m *Manager) agentFinished(ctx context.Context, h *Handoff, a agentRun) {
	h.Run = nil
	a.fx.apply(h)
	if a.trigger == TriggerDirty {
		m.dirtyAgentFinished(ctx, h, a.reason)
		return
	}
	if a.reason != "" {
		h.EscalatedBy = a.trigger
		m.escalate(ctx, h, a.reason)
	} else {
		m.setState(h, StateCIRunning, "")
	}
}

// lostRun cleans up after a run a restart cut short, as cleanup does after a failed
// one: a conflict run's rebase is aborted, the tree goes back to the run's starting
// head unless a commit moved it on, and a dirty run's WIP commit is unwrapped. Ignored
// files changed since the run started are named, not removed: with no stamps from
// before it, a planted file can't be told from a changed one.
func (m *Manager) lostRun(ctx context.Context, h *Handoff) {
	r := *h.Run
	if r.Trigger == TriggerConflict {
		m.abortStaleRebase(ctx, h)
	}
	unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
	if err != nil {
		return // the mark stays: the next tick tries again
	}
	changed, ierr := ignoredSince(ctx, h.Worktree, r, h.Untracked, m.scratch(h.Repo))
	if head, err := worktree.HeadSHA(ctx, h.Worktree); err == nil && head == r.Head {
		if err := reset(ctx, *h, head); err != nil {
			slog.Warn("lastmile lost run: reset", "id", h.ID, "err", err)
		}
	}
	unlock()
	h.Run = nil
	reason := "agent run lost to a restart"
	if names := pathList(changed); names != "" {
		reason += "; gitignored files created or changed since it started (not removed): " + names
	}
	if ierr != nil { // fail closed: unchecked is not clean
		reason += "; checking gitignored files failed, look at them: " + ierr.Error()
	}
	switch {
	case r.Trigger == TriggerDirty:
		if m.unwrapWIP(ctx, *h, r.Head) {
			reason += "; your changes are uncommitted again"
		}
		m.dirtyAgentFinished(ctx, h, reason)
	case len(changed) > 0 || ierr != nil:
		h.EscalatedBy = r.Trigger
		m.escalate(ctx, h, reason)
	case r.Trigger == TriggerQueue:
		logEvent(h, m.now(), "agent", "queue investigation lost to a restart; re-queueing")
	default:
		logEvent(h, m.now(), "agent", reason+"; starting over")
	}
}

// ignoredSince names the gitignored files whose ctime is past r's start, less what
// setup wrote under node_modules before r.SetupDone (see runAgent). Unknown setup
// end = nothing is setup's. Files under the scratch globs are removed, not named:
// build output, and whatever was planted there goes with it.
func ignoredSince(ctx context.Context, dir string, r RunMark, extra, scratch []string) ([]string, error) {
	files, err := ignoredFiles(ctx, dir, extra...)
	if err != nil {
		return nil, err
	}
	since := r.Started.Add(-time.Second).UnixNano() // a second's slack: the kernel's file clock can lag time.Now
	var changed []string
	for f, st := range files {
		setup := !r.SetupDone.IsZero() && st.ctime <= r.SetupDone.UnixNano() && slices.Contains(strings.Split(f, "/"), "node_modules")
		switch {
		case st.ctime <= since || setup:
		case inScratch(scratch, f):
			_ = os.Remove(filepath.Join(dir, f))
		default:
			changed = append(changed, f)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

// redState decides what a red PR with fix-ci on does: rerun flakes, stop on a
// rule, or start the fix agent. It returns the state to show, and whether the
// handoff goes to needs-human.
func (m *Manager) redState(ctx context.Context, h *Handoff, s Snapshot) (state, reason string, err error) {
	if rerun, err := m.rerunFlakes(ctx, h, s); err != nil || rerun {
		return StateCIRunning, "", err
	}
	stop := func(why string) (string, string, error) { return StateNeedsHuman, why, nil }
	if m.Tasks == nil {
		return stop("agents need reviews enabled")
	}
	if n := h.FixRounds[TriggerCI]; n >= h.MaxFixRounds {
		return stop(fmt.Sprintf("CI still failing after %d fix rounds: %s", n, failedChecks(s.Checks)))
	}
	for _, f := range s.Failures {
		if slices.Contains(h.FixedFailures, f.Fingerprint) {
			return stop("same failure twice: " + f.Name)
		}
	}
	h.FixRounds = bump(h.FixRounds, TriggerCI)
	m.launch(ctx, h, TriggerCI, func(ctx context.Context, snap Handoff) outcome { return m.fixCI(ctx, snap, s) })
	return StateCIRed, "fix agent running", nil
}

// bump returns counts with key one higher, as a new map.
func bump(counts map[string]int, key string) map[string]int {
	out := maps.Clone(counts)
	if out == nil {
		out = map[string]int{}
	}
	out[key]++
	return out
}

// agentJob is what the shared agent helper needs to know about one kind of run.
type agentJob struct {
	kind  string // "fix agent", "review agent": names the run in reasons
	head  string // the head the PR shows; the worktree must be at it ("" = any)
	held  bool   // the caller holds the worktree lock and has a rebase stopped: no lock, no clean-tree or head check
	base  string // for the diff against origin/<base>
	build func(diff string, truncated bool) (prompt string, files map[string]string, err error)
}

// runAgent is the part every agent kind shares (design §5): it takes the
// worktree lock, refuses a dirty or out-of-date worktree, snapshots the ignored
// files, runs the agent and hands its result.json to apply, still under the lock.
// apply decodes the result and goes through commitAgentWork; it returns "" when
// it's done, else why a human is needed. On every other exit after the agent ran
// the worktree is put back (see cleanup).
func (m *Manager) runAgent(ctx context.Context, h Handoff, j agentJob, apply func(raw []byte, before map[string]stamp) string) (reason string) {
	if !j.held {
		unlock, err := m.lockWorktree(ctx, h.ID, h.Worktree)
		if err != nil {
			return j.kind + ": " + err.Error()
		}
		defer unlock()
		if st, err := status(ctx, h); err != nil || st != "" {
			return "worktree has uncommitted changes; the " + j.kind + " would mix them in"
		}
	}
	start, err := worktree.HeadSHA(ctx, h.Worktree)
	if err != nil || (j.head != "" && !j.held && start != j.head) {
		return fmt.Sprintf("worktree is at %.12s, the PR at %.12s", start, j.head)
	}
	diff, _ := proc.Run(ctx, h.Worktree, "git", "diff", "origin/"+j.base+"...HEAD")
	truncated := len(diff) > maxDiffPrompt
	diff = clip(diff, maxDiffPrompt, "")
	prompt, files, err := j.build(diff, truncated)
	if err != nil {
		return j.kind + " prompt: " + err.Error()
	}
	before, err := ignoredFiles(ctx, h.Worktree, h.Untracked...)
	if err != nil {
		return "listing ignored files: " + err.Error()
	}
	// From here the agent may have touched the worktree.
	defer func() {
		if reason != "" {
			reason = m.cleanup(ctx, h, start, before, reason)
		}
	}()
	// The container's setup (npm install into workspace node_modules) rewrites
	// ignored files too, so what it leaves under node_modules joins the baseline.
	// Only there: setup runs the branch's install scripts, which an earlier agent
	// round may have changed, so anything else it writes (.envrc, .venv/bin) is
	// still held against the run.
	// ponytail: node_modules/.bin is still trusted; protect package.json scripts if that matters.
	afterSetup := func() error {
		b, err := ignoredFiles(ctx, h.Worktree)
		if err != nil {
			return err
		}
		for f, st := range b {
			if slices.Contains(strings.Split(f, "/"), "node_modules") {
				before[f] = st
			}
		}
		if h.Run != nil { // for a lost run's check (lostRun)
			r := *h.Run
			r.SetupDone = m.now()
			m.markRun(h.ID, &r)
		}
		return nil
	}
	res, err := m.Tasks.RunTask(ctx, review.Task{ID: h.ID, Repo: h.Repo, Worktree: h.Worktree, Agent: h.Agent, Prompt: prompt, Context: files,
		AfterSetup: afterSetup})
	if err != nil {
		slog.Warn("lastmile agent failed", "kind", j.kind, "id", h.ID, "err", err)
		return j.kind + " failed: " + err.Error()
	}
	return apply(res.Result, before)
}

// cleanup puts the worktree back after a run that didn't end in a push: new
// ignored files are removed and changed ones named in the reason (the agent
// could have touched what the host runs later), and the tree goes back to the
// starting head unless a commit moved it on. It runs on a context that survives
// the cancel that may have ended the run.
func (m *Manager) cleanup(ctx context.Context, h Handoff, start string, before map[string]stamp, reason string) string {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	changed, err := ignoredChanges(ctx, h.Worktree, before, m.scratch(h.Repo))
	if err != nil {
		slog.Warn("lastmile cleanup: ignored files", "id", h.ID, "err", err)
	}
	if head, err := worktree.HeadSHA(ctx, h.Worktree); err == nil && head == start {
		if err := reset(ctx, h, start); err != nil {
			slog.Warn("lastmile cleanup: reset", "id", h.ID, "err", err)
		}
	}
	if names := pathList(changed); names != "" && !strings.Contains(reason, names) {
		reason += "; gitignored files created or changed (new ones removed): " + names
	}
	return reason
}

// decode reads an agent's result.json into v.
func decode(kind string, raw []byte, v any) string {
	if err := json.Unmarshal(raw, v); err != nil {
		return kind + "'s result.json is not valid: " + err.Error()
	}
	return ""
}

// applyBody writes the PR body an agent proposed, unless a human edited it since
// TLM last did or it lost one of the template's sections; then the proposal goes
// out as a PR comment instead (design §5). Failures are logged, never fatal: the
// work is pushed already.
func (m *Manager) applyBody(ctx context.Context, h Handoff, body, why string, fx *effects) {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	p, err := m.GH.PRView(ctx, ref)
	if err != nil {
		fx.log("body", "body update skipped, reading the PR failed: %v", err)
		return
	}
	var problem string
	switch {
	case !sameBody(p.Body, h.LastBodyWritten):
		problem = "someone edited the description since TLM last wrote it"
	case !hasHeadings(body, p.Body):
		problem = "the proposal drops a section of the current description"
	}
	if problem == "" {
		if err := m.GH.EditPR(ctx, ref, github.PREdit{Body: &body}); err != nil {
			fx.log("body", "updating the description failed: %v", err)
			return
		}
		fx.body = &body
		fx.log("body", "description updated: %s", why)
		return
	}
	text := fmt.Sprintf("TLM would change the PR description (%s), but %s, so here is the proposal instead.\n\n<details><summary>Proposed description</summary>\n\n%s\n\n</details>", why, problem, body)
	id, err := m.GH.Comment(ctx, ref, text)
	if err != nil {
		fx.log("body", "posting the proposed description failed: %v", err)
		return
	}
	if id != 0 {
		fx.posted = append(fx.posted, id)
	}
	fx.log("body", "description not changed (%s); proposal posted as a comment: %s", problem, why)
}

// sameBody compares bodies the way GitHub round-trips them: line endings and
// surrounding space don't count.
func sameBody(a, b string) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(a) == norm(b)
}

// hasHeadings says every "## " heading of old is still in next.
func hasHeadings(next, old string) bool {
	have := map[string]bool{}
	for _, l := range strings.Split(next, "\n") {
		have[strings.TrimSpace(l)] = true
	}
	for _, l := range strings.Split(old, "\n") {
		if l = strings.TrimSpace(l); strings.HasPrefix(l, "## ") && !have[l] {
			return false
		}
	}
	return true
}

// fixCI runs the fix agent and, if it produced a change, the host write path.
func (m *Manager) fixCI(ctx context.Context, h Handoff, s Snapshot) outcome {
	var fx effects
	for _, f := range s.Failures { // counted when the round ends: a run lost to a restart spent nothing
		fx.failures = append(fx.failures, f.Fingerprint)
	}
	view := s
	view.Failures = slices.Clone(s.Failures)
	for i := range view.Failures {
		view.Failures[i].Log = "" // in the prompt below, and in failures.json
	}
	snapJSON, _ := json.MarshalIndent(view, "", "  ")
	failJSON, _ := json.MarshalIndent(s.Failures, "", "  ")
	reason := m.runAgent(ctx, h, agentJob{kind: "fix agent", head: s.Head, base: s.Base,
		build: func(diff string, truncated bool) (string, map[string]string, error) {
			prompt, err := render(fixPrompt, fixPromptData{Title: h.Title, Body: h.Body, Branch: h.Branch, Base: s.Base, Failures: s.Failures,
				Snapshot: string(snapJSON), Diff: diff, DiffTruncated: truncated})
			return prompt, map[string]string{"snapshot.json": string(snapJSON), "failures.json": string(failJSON)}, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out AgentResult
			if r := decode("fix agent", raw, &out); r != "" {
				return r
			}
			reason, err := m.commitAgentWork(ctx, h, out, before, &fx)
			if err != nil {
				slog.Warn("lastmile write path failed", "id", h.ID, "err", err)
				return "writing the fix failed: " + err.Error()
			}
			if reason == "" || reason == reasonNoChanges {
				fx.askRebase("fix agent", out)
			}
			if reason == reasonNoChanges && out.Rebase { // the fix is on the base already: the rebase is the fix
				fx.log("agent", "fix agent: %s (confidence %s)", out.Summary, out.Confidence)
				return ""
			}
			if reason != "" {
				return reason
			}
			slog.Info("lastmile fix pushed", "id", h.ID, "summary", out.Summary, "confidence", out.Confidence)
			fx.log("agent", "fix agent: %s (confidence %s)", out.Summary, out.Confidence)
			fx.log("commit", "pushed %s", m.headShort(ctx, h))
			if out.Body != "" {
				m.applyBody(ctx, h, out.Body, out.BodyReason, &fx)
			}
			return ""
		})
	return outcome{reason: reason, fx: fx}
}

// askRebase notes an agent's request to rebase after its run (drift does it).
func (fx *effects) askRebase(kind string, res AgentResult) {
	if res.Rebase {
		fx.rebase = kind
		fx.log("rebase", "%s asked for a rebase onto the base: %s", kind, res.RebaseReason)
	}
}

// headShort is the worktree's head, abbreviated.
func (m *Manager) headShort(ctx context.Context, h Handoff) string {
	sha, _ := worktree.HeadSHA(ctx, h.Worktree)
	return sha[:min(7, len(sha))]
}

// queueAgent runs the queue agent on a PR GitHub took out of the merge queue for
// no cause TLM could tell. "requeue" with no edits is a fine answer: mergeStep
// re-queues; edits go out through the host write path first.
func (m *Manager) queueAgent(ctx context.Context, h Handoff, s Snapshot, removal string) outcome {
	var fx effects
	snapJSON, _ := json.MarshalIndent(s, "", "  ")
	reason := m.runAgent(ctx, h, agentJob{kind: "queue agent", head: s.Head, base: s.Base,
		build: func(diff string, truncated bool) (string, map[string]string, error) {
			prompt, err := render(queuePrompt, queuePromptData{Title: h.Title, Body: h.Body, Branch: h.Branch, Base: s.Base,
				Removal: removal, Snapshot: string(snapJSON), Diff: diff, DiffTruncated: truncated})
			return prompt, map[string]string{"snapshot.json": string(snapJSON)}, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out struct{ Action, Summary string }
			if r := decode("queue agent", raw, &out); r != "" {
				return r
			}
			res := AgentResult{Summary: out.Summary, Escalate: out.Action != "requeue", EscalateReason: out.Summary}
			reason, err := m.commitAgentWork(ctx, h, res, before, &fx)
			if err != nil {
				slog.Warn("lastmile write path failed", "id", h.ID, "err", err)
				return "writing the queue agent's change failed: " + err.Error()
			}
			switch reason {
			case reasonNoChanges:
				fx.log("agent", "queue agent: %s; re-queueing", out.Summary)
				return ""
			case "":
				fx.log("agent", "queue agent: %s", out.Summary)
				fx.log("commit", "pushed %s", m.headShort(ctx, h))
			}
			return reason
		})
	return outcome{reason: reason, fx: fx}
}
