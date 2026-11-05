package lastmile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/reneruck/sisyphus/internal/proc"
	"github.com/reneruck/sisyphus/internal/review"
)

// TaskRunner runs an agent on a worktree; *review.Reviewer does.
type TaskRunner interface {
	RunTask(ctx context.Context, t review.Task) (review.TaskResult, error)
}

// maxDiffPrompt bounds the diff in the fix prompt.
const maxDiffPrompt = 60 << 10

// agentRun is an agent run in flight for a handoff, or, once finished, what it
// found for the next tick to apply: the tick is the only writer of handoffs, so
// a run never races with it.
type agentRun struct {
	cancel context.CancelFunc
	done   bool
	reason string // "" = the work was pushed
}

// runs tracks the agent runs by handoff id, in memory: after a restart a run is
// lost, and the next tick sees CI still red.
type runs struct {
	mu sync.Mutex
	m  map[string]*agentRun
}

// get returns a copy of the run: done and reason are written by its goroutine.
func (r *runs) get(id string) (agentRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a := r.m[id]; a != nil {
		return agentRun{done: a.done, reason: a.reason}, true
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

// agentFinished applies the outcome of a run that ended since the last tick.
func (m *Manager) agentFinished(ctx context.Context, h *Handoff) {
	a, _ := m.running.get(h.ID)
	m.running.drop(h.ID)
	if a.reason != "" {
		h.EscalatedBy = TriggerCI
		m.escalate(ctx, h, a.reason)
	} else {
		m.setState(h, StateCIRunning, "")
	}
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
	if h.FixRounds == nil {
		h.FixRounds = map[string]int{}
	}
	h.FixRounds[TriggerCI]++
	for _, f := range s.Failures {
		h.FixedFailures = append(h.FixedFailures, f.Fingerprint)
	}
	runCtx, cancel := context.WithCancel(ctx)
	run := &agentRun{cancel: cancel}
	if !m.running.put(h.ID, run) {
		cancel()
		return StateCIRed, "fix agent running", nil
	}
	snap := *h // the goroutine works from a copy
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()
		reason := m.fixCI(runCtx, snap, s)
		if ctx.Err() != nil || runCtx.Err() != nil { // shutdown or cancel: nothing to apply
			m.running.drop(snap.ID)
			return
		}
		m.running.mu.Lock()
		run.done, run.reason = true, reason
		m.running.mu.Unlock()
		m.Kick()
	}()
	return StateCIRed, "fix agent running", nil
}

// fixCI runs the fix agent and, if it produced a change, the host write path.
// It returns "" when the work was pushed, else why a human is needed.
func (m *Manager) fixCI(ctx context.Context, h Handoff, s Snapshot) string {
	unlock, err := m.locks.Lock(ctx, "lastmile:"+h.ID)
	if err != nil {
		return "fix agent: " + err.Error()
	}
	defer unlock()

	if st, err := proc.Run(ctx, h.Worktree, "git", "status", "--porcelain"); err != nil || st != "" {
		return "worktree has uncommitted changes; the fix agent would mix them in"
	}
	if head, err := proc.Run(ctx, h.Worktree, "git", "rev-parse", "HEAD"); err != nil || (s.Head != "" && head != s.Head) {
		return fmt.Sprintf("worktree is at %.12s, the PR at %.12s", head, s.Head)
	}
	diff, _ := proc.Run(ctx, h.Worktree, "git", "diff", "origin/"+s.Base+"...HEAD")
	truncated := len(diff) > maxDiffPrompt
	if truncated {
		diff = strings.ToValidUTF8(diff[:maxDiffPrompt], "")
	}
	view := s
	view.Failures = slices.Clone(s.Failures)
	for i := range view.Failures {
		view.Failures[i].Log = "" // in the prompt below, and in failures.json
	}
	snapJSON, _ := json.MarshalIndent(view, "", "  ")
	failJSON, _ := json.MarshalIndent(s.Failures, "", "  ")
	prompt, err := renderFixPrompt(fixPromptData{Title: h.Title, Body: h.Body, Branch: h.Branch, Base: s.Base, Failures: s.Failures,
		Snapshot: string(snapJSON), Diff: diff, DiffTruncated: truncated})
	if err != nil {
		return "fix prompt: " + err.Error()
	}
	before, err := ignoredFiles(ctx, h.Worktree)
	if err != nil {
		return "listing ignored files: " + err.Error()
	}
	res, err := m.Tasks.RunTask(ctx, review.Task{ID: h.ID, Repo: h.Repo, Worktree: h.Worktree, Agent: h.Agent, Prompt: prompt,
		Context: map[string]string{"snapshot.json": string(snapJSON), "failures.json": string(failJSON)}})
	if err != nil {
		slog.Warn("lastmile fix agent failed", "id", h.ID, "err", err)
		return "fix agent failed: " + err.Error()
	}
	var out AgentResult
	if err := json.Unmarshal(res.Result, &out); err != nil {
		return "fix agent's result.json is not valid: " + err.Error()
	}
	reason, err := m.commitAgentWork(ctx, h, out, before)
	if err != nil {
		slog.Warn("lastmile write path failed", "id", h.ID, "err", err)
		return "writing the fix failed: " + err.Error()
	}
	if reason == "" {
		slog.Info("lastmile fix pushed", "id", h.ID, "summary", out.Summary, "confidence", out.Confidence)
	}
	return reason
}
