package lastmile

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/reneruck/sisyphus/internal/github"
)

// maxBotReplies caps TLM's replies per bot-started thread (design §5).
const maxBotReplies = 3

// feedback reads the review inbox, puts it on the snapshot and decides what new
// feedback does: start the review agent, or hand over to a human. It returns the
// state to show (the caller's, when there is nothing new).
func (m *Manager) feedback(ctx context.Context, h *Handoff, p github.PRSnapshot, s *Snapshot, state, reason string) (string, string, error) {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	in, err := m.GH.Inbox(ctx, ref)
	if err != nil {
		return state, reason, fmt.Errorf("reading reviews: %w", err)
	}
	box := buildInbox(in, *h, m.cfg.LastMile.BotLogins, s.Author)
	s.Unresolved, s.Unanswered, s.NewFeedback, s.BotReplies = box.Unresolved, box.Unanswered, box.Refs(), box.BotReplies
	if len(box.New) == 0 {
		return state, reason, nil
	}
	// stop hands the handoff to a human. The cursor moves on when the human is expected to
	// deal with these items; otherwise they stay new for the round after a resume.
	stop := func(advance bool, why string) (string, string, error) {
		h.EscalatedBy = TriggerReview
		if advance {
			h.Cursor = advanceCursor(h.Cursor, box.New)
		}
		return StateNeedsHuman, why, nil
	}
	for _, it := range box.New {
		if it.Kind == KindThread && slices.Contains(h.FixedThreads, it.ID) && it.Human() {
			return stop(true, "reviewer re-raised a fixed thread: "+it.Path)
		}
	}
	if !h.Autonomy.AddressReviews {
		// Bot-only feedback is just shown in the snapshot.
		if n := box.humanCount(); n > 0 {
			return stop(true, fmt.Sprintf("review feedback: %d new comments", n))
		}
		return state, reason, nil
	}
	if m.Tasks == nil {
		return stop(false, "agents need reviews enabled")
	}
	if n := h.FixRounds[TriggerReview]; n >= h.MaxFixRounds {
		return stop(false, fmt.Sprintf("review feedback keeps coming after %d rounds", n))
	}
	h.FixRounds = bump(h.FixRounds, TriggerReview)
	logEvent(h, m.now(), "agent", fmt.Sprintf("review agent started on %d items", len(box.New)))
	snap := *s
	snap.Failures = nil
	changes := changesRequested(p.Reviews)
	m.launch(ctx, h, TriggerReview, func(ctx context.Context, hh Handoff) outcome {
		return m.reviewAgent(ctx, hh, snap, box.New, p.Body, changes)
	})
	return StateFeedback, "review agent running", nil
}

// reviewAgent runs the review-response agent over items and applies its result on
// the host: edits first (one commit, one push), then the replies in one batch.
func (m *Manager) reviewAgent(ctx context.Context, h Handoff, s Snapshot, items []Item, body string, changes []string) outcome {
	var fx effects
	itemsJSON, _ := json.MarshalIndent(items, "", "  ")
	snapJSON, _ := json.MarshalIndent(s, "", "  ")
	reason := m.runAgent(ctx, h, agentJob{kind: "review agent", head: s.Head, base: s.Base,
		build: func(diff string, truncated bool) (string, map[string]string, error) {
			prompt, err := render(reviewPrompt, reviewPromptData{Title: h.Title, Body: body, Branch: h.Branch, Base: s.Base,
				Items: string(itemsJSON), Snapshot: string(snapJSON), Diff: diff, DiffTruncated: truncated})
			return prompt, map[string]string{"snapshot.json": string(snapJSON), "feedback.json": string(itemsJSON)}, err
		}},
		func(raw []byte, before map[string]stamp) string {
			var out ReviewResult
			if r := decode("review agent", raw, &out); r != "" {
				return r
			}
			actions, dropped := validActions(out.Actions, items)
			if len(dropped) > 0 {
				slog.Warn("lastmile review agent named unknown items", "id", h.ID, "dropped", dropped)
				fx.log("agent", "dropped actions for unknown items or verbs: %s", strings.Join(dropped, ", "))
			}
			reason, err := m.commitAgentWork(ctx, h, out.AgentResult, before, &fx)
			if err != nil {
				slog.Warn("lastmile write path failed", "id", h.ID, "err", err)
				return "writing the fix failed: " + err.Error()
			}
			sha := ""
			switch reason {
			case "": // pushed
				sha = m.headShort(ctx, h)
				fx.log("commit", "pushed %s: %s", sha, out.Summary)
			case reasonNoChanges: // answers only
			default:
				return reason // rejected: nothing is posted, the items stay new
			}
			fx.log("agent", "review agent: %s (confidence %s)", out.Summary, out.Confidence)
			fx.handed = items
			postErr := m.postReplies(ctx, h, items, actions, sha, &fx)
			if out.Body != "" {
				m.applyBody(ctx, h, out.Body, out.BodyReason, &fx)
			}
			if sha != "" {
				m.reRequest(ctx, h, s.Author, items, actions, changes, &fx)
			}
			if postErr != nil {
				return "posting replies failed: " + postErr.Error()
			}
			return ""
		})
	return outcome{reason: reason, fx: fx}
}

// postReplies posts the answers in one batch, in the order the agent gave them.
// Inline threads get a reply under their first comment; review bodies and
// conversation comments share one PR comment that links what it answers. It
// carries on after a failed post and returns the first error.
func (m *Manager) postReplies(ctx context.Context, h Handoff, items []Item, actions []ReviewAction, sha string, fx *effects) error {
	ref := github.PRRef{Repo: h.Repo, Number: h.PRNumber}
	var firstErr error
	fail := func(err error) {
		if firstErr == nil {
			firstErr = err
		}
	}
	var general []string
	handled := map[string]bool{}
	for _, a := range actions {
		handled[a.ThreadID] = true
		it := items[slices.IndexFunc(items, func(it Item) bool { return it.ID == a.ThreadID })]
		fixed := a.Action == "fix" && sha != ""
		text := strings.TrimSpace(a.Reply)
		switch {
		case a.Action == "skip":
			fx.log("skip", "%s: %s", it.ID, a.Reason)
			continue
		case fixed:
			text = strings.TrimSpace("Fixed in " + sha + ". " + text)
		case a.Action == "fix" && text == "":
			fx.log("skip", "%s: the agent claimed a fix but changed nothing: %s", it.ID, a.Reason)
			continue
		case text == "":
			fx.log("skip", "%s: %s without a reply text: %s", it.ID, a.Action, a.Reason)
			continue
		}
		if it.Kind != KindThread {
			c := it.Comments[0]
			general = append(general, fmt.Sprintf("Re %s (%s):\n\n%s", c.URL, c.Author, text))
			continue
		}
		// ponytail: the cap only counts threads that a bot started, not top-level bot comments.
		if it.BotThread() && countTLM(it) >= maxBotReplies {
			fx.log("bot-cap", "%s: already replied %d times in this bot thread, not posting", it.ID, maxBotReplies)
			continue
		}
		id, err := m.GH.Reply(ctx, ref, it.first().ID, text)
		if err != nil {
			fail(err)
			continue
		}
		fx.posted = append(fx.posted, id)
		if fixed {
			fx.fixed = append(fx.fixed, it.ID)
		}
		fx.log("reply", "%s (%s): %s", it.ID, a.Action, firstLine(text))
	}
	for _, it := range items {
		if !handled[it.ID] {
			fx.log("skip", "%s: the agent returned no action", it.ID)
		}
	}
	if len(general) > 0 {
		id, err := m.GH.Comment(ctx, ref, strings.Join(general, "\n\n---\n\n"))
		if err != nil {
			fail(err)
		} else {
			fx.posted = append(fx.posted, id)
			fx.log("reply", "PR comment answering %d items", len(general))
		}
	}
	return firstErr
}

func countTLM(it Item) int {
	n := 0
	for _, c := range it.Comments {
		if c.TLM {
			n++
		}
	}
	return n
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	if r := []rune(l); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return l
}

// reRequest asks the reviewers whose CHANGES_REQUESTED a pushed fix answered to
// look again (design §6), unless a human took them off the PR.
func (m *Manager) reRequest(ctx context.Context, h Handoff, author string, items []Item, actions []ReviewAction, changes []string, fx *effects) {
	var who []string
	for _, a := range actions {
		if a.Action != "fix" {
			continue
		}
		for _, it := range items {
			if it.ID != a.ThreadID {
				continue
			}
			for _, c := range it.newComments() {
				if slices.Contains(changes, c.Author) && !slices.Contains(h.Removed, c.Author) && !strings.EqualFold(c.Author, author) {
					who = union(who, c.Author)
				}
			}
		}
	}
	if len(who) == 0 {
		return
	}
	if err := m.GH.EditPR(ctx, github.PRRef{Repo: h.Repo, Number: h.PRNumber}, github.PREdit{AddReviewers: who}); err != nil {
		fx.log("reviewer", "re-requesting %s failed: %v", strings.Join(who, ", "), err)
		return
	}
	fx.requested = who
	fx.log("reviewer", "re-requested %s", strings.Join(who, ", "))
}
