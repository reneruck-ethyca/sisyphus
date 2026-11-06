package lastmile

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/proc"
)

// Run ticks every lastmile.interval and on Kick until ctx is done.
func (m *Manager) Run(ctx context.Context) {
	t := time.NewTicker(m.cfg.LastMile.Interval.Duration)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-m.kick:
		case <-ctx.Done():
			return
		}
		m.tick(ctx)
	}
}

// tick moves each open handoff along, one at a time. A failure stays with its
// handoff (Error) and never stops the others.
func (m *Manager) tick(ctx context.Context) {
	for _, h := range m.List() {
		if Terminal(h.State) {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		next := h
		next.Error = ""
		if err := m.advance(ctx, &next); err != nil {
			slog.Warn("lastmile tick failed", "id", h.ID, "err", err)
			next.Error = err.Error()
		}
		m.judge(&next)
		if reflect.DeepEqual(h, next) {
			continue
		}
		if m.commit(h, next) {
			m.afterCommit(ctx, h, next)
		}
	}
}

// advance does one handoff's tick on h: open the PR, or read it and move it on.
func (m *Manager) advance(ctx context.Context, h *Handoff) error {
	if h.PRNumber == 0 {
		return m.openPR(ctx, h)
	}
	a, running := m.running.get(h.ID)
	if !running {
		m.abortStaleRebase(ctx, h)
	}
	if running {
		if !a.done {
			return nil // the agent holds the worktree; the handoff shows "fix agent running"
		}
		// A run just ended. Don't read the PR in the same tick: right after a push GitHub
		// may still show the old head's red checks. The next tick reads the new head.
		m.agentFinished(ctx, h)
		return nil
	}
	return m.follow(ctx, h)
}

// lockedGit runs git in the handoff's worktree while holding its lock.
func (m *Manager) lockedGit(ctx context.Context, h Handoff, args ...string) (string, error) {
	unlock, err := m.locks.Lock(ctx, "lastmile:"+h.ID)
	if err != nil {
		return "", err
	}
	defer unlock()
	return proc.Run(ctx, h.Worktree, "git", args...)
}

// prTitle puts the Jira key in front unless the title has it already.
func prTitle(title, jira string) string {
	if jira == "" || strings.Contains(title, jira) {
		return title
	}
	return jira + ": " + title
}

// prBody is the body the handoff gave, else the repo's PR template, else nothing.
// The template comes from git, not the file system: git shows a symlink as its
// target path, where reading the file would publish whatever host file it points at.
func prBody(ctx context.Context, h Handoff) string {
	if h.Body != "" {
		return h.Body
	}
	b, err := proc.Run(ctx, h.Worktree, "git", "show", "HEAD:.github/pull_request_template.md")
	if err != nil || len(b) > 64<<10 {
		return ""
	}
	return b
}

// openPR pushes the branch and opens the PR, or adopts the one already open
// for the branch: a crash between the two must not open a second PR.
func (m *Manager) openPR(ctx context.Context, h *Handoff) error {
	if _, err := m.lockedGit(ctx, *h, "push", "-u", "origin", h.Branch); err != nil {
		return err
	}
	ref, found, err := m.GH.FindPR(ctx, h.Repo, h.Branch)
	if err != nil {
		return err
	}
	if !found {
		body := prBody(ctx, *h)
		if ref, err = m.GH.CreatePR(ctx, h.Repo, github.NewPR{Base: h.Base, Head: h.Branch, Title: prTitle(h.Title, h.Jira),
			Body: body, Draft: h.Draft, Labels: h.Labels}); err != nil {
			return err
		}
		h.LastBodyWritten = body
	} else {
		// An adopted PR's past discussion was had before the handoff: only what comes after is feedback.
		in, err := m.GH.Inbox(ctx, ref)
		if err != nil {
			return err
		}
		h.Cursor = cursorAtEnd(in)
	}
	h.PRNumber, h.PROpened = ref.Number, m.now()
	h.PRURL = fmt.Sprintf("https://github.com/%s/pull/%d", ref.Repo, ref.Number)
	h.State, h.Updated = StatePROpen, m.now()
	logEvent(h, m.now(), "pr-opened", h.PRURL)
	return nil
}

// follow reads the PR and applies the state machine: merged or closed ends the
// handoff, otherwise reviewers are requested and the state follows CI and reviews.
func (m *Manager) follow(ctx context.Context, h *Handoff) error {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	p, err := m.GH.PRView(ctx, ref)
	if err != nil {
		return err
	}
	switch p.State {
	case "MERGED":
		m.outcome(*h, p.MergedBy, p.HeadSHA, "merged", p.MergedAt)
		m.finish(h, StateMerged)
		return nil
	case "CLOSED":
		m.outcome(*h, "", p.HeadSHA, "closed", p.ClosedAt)
		m.finish(h, StateClosed)
		return nil
	}
	s := buildSnapshot(p, h.PROpened, m.now(), m.cfg.LastMile.BotLogins)
	base := cmp.Or(p.BaseRef, h.Base)
	s.Behind = m.behind(ctx, *h, base)
	s.BodyEdited = !sameBody(p.Body, h.LastBodyWritten)
	h.Snapshot = &s
	if !s.Draft && h.ReadyAt.IsZero() {
		h.ReadyAt = m.now()
	}
	if h.State == StateNeedsHuman { // a person has it; only merged and closed are watched
		return nil
	}

	noteRemoved(h, p)
	if s.Draft && !h.Draft { // asked for a ready PR, so make it one
		if err := m.GH.MarkReady(ctx, ref); err != nil {
			return err
		}
		s.Draft = false
	}
	if done, err := m.stack(ctx, h, p, s); done || err != nil {
		return err
	}
	if done, err := m.drift(ctx, h, s, base); done || err != nil {
		return err
	}
	state, reason := nextState(*h, s)
	if s.CI == CIRed {
		s.Failures = m.failures(ctx, *h, s.Checks)
		if h.Autonomy.FixCI {
			var err error
			if state, reason, err = m.redState(ctx, h, s); err != nil {
				return err
			}
		} else if rerun, err := m.rerunFlakes(ctx, h, s); err != nil {
			return err
		} else if rerun {
			state, reason = StateCIRunning, ""
		}
	}
	if s.CI != CIRed && state != StateNeedsHuman {
		var err error
		if state, reason, err = m.feedback(ctx, h, p, &s, state, reason); err != nil {
			return err
		}
	}
	// Requests follow the state computed before them: humans are asked at the first green,
	// and the handoff shows ci-green until the next tick.
	if err := m.requestReviewers(ctx, h, ref, s); err != nil {
		return err
	}
	if state == StateNeedsHuman {
		if s.CI == CIRed {
			h.EscalatedBy = TriggerCI
		}
		m.escalate(ctx, h, reason)
		return nil
	}
	m.setState(h, state, reason)
	return nil
}

// behind counts how many commits of origin/<base> the branch lacks. A failed
// fetch keeps the last count: it's information, not a reason to stop.
func (m *Manager) behind(ctx context.Context, h Handoff, base string) int {
	last := 0
	if h.Snapshot != nil {
		last = h.Snapshot.Behind
	}
	if _, err := m.lockedGit(ctx, h, "fetch", "--quiet", "origin", fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", base, base)); err != nil {
		slog.Warn("lastmile fetch failed", "id", h.ID, "err", err)
		return last
	}
	out, err := m.lockedGit(ctx, h, "rev-list", "--count", "HEAD..origin/"+base)
	if err != nil {
		return last
	}
	n, _ := strconv.Atoi(out)
	return n
}

// setState moves h to state; Updated changes only when it does.
func (m *Manager) setState(h *Handoff, state, reason string) {
	if h.State != state {
		h.State, h.Updated = state, m.now()
		logEvent(h, h.Updated, "state", strings.TrimSuffix(state+": "+reason, ": "))
	}
	h.Reason = reason
}

// finish ends the handoff: merged or closed. The branch is free again.
func (m *Manager) finish(h *Handoff, state string) {
	h.State, h.Updated, h.Reason = state, m.now(), ""
	logEvent(h, h.Updated, "state", state)
}

// escalate puts h in needs-human: the label on the PR (created if the repo lacks
// it), and the state, which keeps where it was in PrevState. Label failures are
// logged; the state is what matters.
func (m *Manager) escalate(ctx context.Context, h *Handoff, reason string) {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	label := m.cfg.LastMile.NeedsHumanLabel
	err := m.GH.CreateLabel(ctx, h.Repo, label, "d93f0b", "Waiting for a human")
	if err == nil {
		err = m.GH.EditPR(ctx, ref, github.PREdit{AddLabels: []string{label}})
	}
	if err != nil {
		slog.Warn("lastmile needs-human label failed", "id", h.ID, "err", err)
	}
	h.PrevState = h.State
	m.setState(h, StateNeedsHuman, reason)
}

// afterCommit sends the notifications a tick's change calls for.
func (m *Manager) afterCommit(ctx context.Context, before, after Handoff) {
	if m.OnChange != nil {
		m.OnChange()
	}
	if !before.ParentMerged && after.ParentMerged {
		m.Notify(ctx, after, EventStackParentMerged)
	}
	switch {
	case before.PRNumber == 0 && after.PRNumber != 0:
		m.Notify(ctx, after, EventPROpened)
	case before.State == after.State:
	case after.State == StateMerged:
		m.Notify(ctx, after, EventMerged)
	case after.State == StateClosed:
		m.Notify(ctx, after, EventClosed)
	case after.State == StateNeedsHuman:
		m.Notify(ctx, after, EventNeedsHuman)
	}
}

// commit stores what a tick did to h. A reviewer edit that arrived while the
// tick ran wins over the tick's copy of the list.
func (m *Manager) commit(before, after Handoff) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.handoffs, func(o Handoff) bool { return o.ID == after.ID })
	if i < 0 || m.handoffs[i].Gen != before.Gen {
		return false // gone, or resumed or cancelled while the tick ran: its view is stale
	}
	if cur := m.handoffs[i]; !slices.Equal(cur.Reviewers, before.Reviewers) || cur.ReviewersSet != before.ReviewersSet {
		after.Reviewers, after.ReviewersSet, after.ReviewersPending = cur.Reviewers, cur.ReviewersSet, cur.ReviewersPending
		after.Removed = cur.Removed
	}
	m.handoffs[i] = after
	if err := m.saveLocked(); err != nil {
		slog.Error("lastmile save failed", "err", err)
	}
	return true
}
