package lastmile

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
)

// ErrNotFound is a handoff id nobody knows.
var ErrNotFound = errors.New("no such handoff")

// pick takes count logins from the roster round-robin, starting at cursor and
// skipping those in skip. It returns them and the cursor to continue from.
func pick(roster []string, count, cursor int, skip []string) ([]string, int) {
	var out []string
	next := cursor
	for i := 0; i < len(roster) && len(out) < count; i++ {
		login := roster[(cursor+i)%len(roster)]
		if slices.ContainsFunc(skip, func(s string) bool { return strings.EqualFold(s, login) }) || slices.Contains(out, login) {
			continue
		}
		out = append(out, login)
		next = (cursor + i + 1) % len(roster)
	}
	return out, next
}

// without is list minus drop, as a new slice (handoffs are shared by copy).
func without(list []string, drop ...string) []string {
	var out []string
	for _, l := range list {
		if !slices.Contains(drop, l) {
			out = append(out, l)
		}
	}
	return out
}

// union is a followed by what b adds, as a new slice.
func union(a []string, b ...string) []string {
	out := slices.Clone(a)
	for _, l := range b {
		if !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

// humansOn is who already reviews the PR: requested, or with a standing
// review, other than its author and the bots.
func (m *Manager) humansOn(s Snapshot) []string {
	have := slices.Clone(s.Requested)
	for _, a := range s.Approvals {
		have = union(have, a.User)
	}
	return slices.DeleteFunc(have, func(u string) bool {
		return strings.EqualFold(u, s.Author) || strings.EqualFold(u, m.cfg.LastMile.BotReviewer) ||
			isBot(false, u, m.cfg.LastMile.BotLogins)
	})
}

// rosterPick tops the PR up from its repo's roster to the roster's count,
// counting the humans it already has, and moves the repo's cursor.
func (m *Manager) rosterPick(h Handoff, author string, have []string) []string {
	r := m.cfg.LastMile.ReviewersFor(h.Repo)
	if len(r.Roster) == 0 || len(have) >= r.Count {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	picked, next := pick(r.Roster, r.Count-len(have), m.cursors[h.Repo], append([]string{author}, have...))
	if m.cursors == nil {
		m.cursors = map[string]int{}
	}
	m.cursors[h.Repo] = next
	return picked
}

// noteRemoved records whom a human took off the PR: someone TLM requested who
// is neither requested now nor has reviewed. They are never requested again.
func noteRemoved(h *Handoff, p github.PRSnapshot) {
	for _, u := range h.Requested {
		is := func(v string) bool { return strings.EqualFold(v, u) } // logins are case-insensitive
		if slices.ContainsFunc(p.RequestedUsers, is) || slices.ContainsFunc(h.Removed, is) {
			continue
		}
		if slices.ContainsFunc(p.Reviews, func(r github.SnapReview) bool { return is(r.User) }) {
			continue // a review ends a request; nobody removed them
		}
		h.Removed = append(slices.Clone(h.Removed), u)
	}
}

// requestReviewers asks for the bot reviewer at once, draft or not, and for the
// humans once the PR is ready for them (design §6), each once. Edits to the human
// list made after that are applied as a diff. It records what it did on h.
func (m *Manager) requestReviewers(ctx context.Context, h *Handoff, ref github.PRRef, s Snapshot) error {
	if !h.BotRequested {
		if bot := m.bot(s); bot != "" {
			if err := m.askBot(ctx, h, ref, bot); err != nil {
				return err
			}
		}
		h.BotRequested = true
	}
	switch {
	case !h.HumansRequested && s.Ready():
		humans, have := h.Reviewers, m.humansOn(s)
		if !h.ReviewersSet && len(humans) == 0 {
			humans = m.rosterPick(*h, s.Author, have)
		}
		switch {
		case len(humans) == 0 && len(have) == 0:
			h.Warning = "no human reviewers: pass --reviewers or set lastmile.reviewers.roster"
		case len(humans) > 0:
			if err := m.GH.EditPR(ctx, ref, github.PREdit{AddReviewers: humans}); err != nil {
				return fmt.Errorf("requesting %s: %w", strings.Join(humans, ", "), err)
			}
		}
		h.Requested = union(h.Requested, humans...)
		h.HumansRequested = true
		h.ReviewersPending = false
	case h.HumansRequested && h.ReviewersPending:
		// The edit's diff against GitHub's current requests. TLM's own earlier picks that
		// aren't wanted any more come off; whoever a human added is left alone, and
		// whoever a human removed stays removed unless the edit named them anew.
		want := h.Reviewers
		add := without(want, append(slices.Clone(s.Requested), h.Removed...)...)
		var drop []string
		for _, u := range h.Requested {
			if !strings.EqualFold(u, m.cfg.LastMile.BotReviewer) && !slices.Contains(want, u) && slices.Contains(s.Requested, u) {
				drop = append(drop, u)
			}
		}
		if len(add)+len(drop) > 0 {
			if err := m.GH.EditPR(ctx, ref, github.PREdit{AddReviewers: add, RemoveReviewers: drop}); err != nil {
				return fmt.Errorf("updating reviewers: %w", err)
			}
		}
		h.Requested = union(without(h.Requested, drop...), add...)
		h.ReviewersPending = false
	}
	return nil
}

// maxBotRounds caps the review agent rounds spent on the bot's feedback alone;
// botWait is how long a draft waits for the bot to review its head.
const (
	maxBotRounds = 3
	botWait      = 30 * time.Minute
)

// bot is the bot reviewer for s's PR, or "" when there is none (none set, or it wrote the PR).
func (m *Manager) bot(s Snapshot) string {
	if bot := m.cfg.LastMile.BotReviewer; bot != "" && !strings.EqualFold(bot, s.Author) {
		return bot
	}
	return ""
}

// askBot requests a review from bot and starts the wait for it.
func (m *Manager) askBot(ctx context.Context, h *Handoff, ref github.PRRef, bot string) error {
	if err := m.GH.EditPR(ctx, ref, github.PREdit{AddReviewers: []string{bot}}); err != nil {
		return fmt.Errorf("requesting %s: %w", bot, err)
	}
	h.Requested = union(h.Requested, bot)
	h.BotAsked = m.now()
	return nil
}

// draftStep moves a draft PR towards ready (design §6): a ready PR means ready for
// human review, so the bot reviews first, its feedback is fixed (maxBotRounds at
// most), and the PR goes ready once CI is green and no agent is at work. A head the
// bot hasn't seen is put to it again; a bot that stays quiet for botWait is passed
// over. state is what the tick computed so far.
func (m *Manager) draftStep(ctx context.Context, h *Handoff, ref github.PRRef, p github.PRSnapshot, s *Snapshot, state string) error {
	bot := m.bot(*s)
	if bot != "" && !h.BotRequested {
		return nil // requestReviewers asks for it this tick
	}
	reviewed := bot != "" && slices.ContainsFunc(p.Reviews, func(r github.SnapReview) bool {
		return strings.EqualFold(r.User, bot) && r.CommitSHA == s.Head
	})
	if reviewed {
		h.BotAsked = time.Time{}
	}
	quiet := !h.BotAsked.IsZero() && m.now().Sub(h.BotAsked) >= botWait
	done := bot == "" || reviewed || quiet || h.BotRounds >= maxBotRounds
	if !done && h.BotAsked.IsZero() { // the head moved on since the bot's last review
		if err := m.askBot(ctx, h, ref, bot); err != nil {
			return err
		}
		logEvent(h, m.now(), "reviewer", "asked "+bot+" to review the new head")
		return nil
	}
	if !done || s.CI != CIGreen || state == StateFeedback || state == StateNeedsHuman {
		return nil
	}
	if err := m.GH.MarkReady(ctx, ref); err != nil {
		return err
	}
	s.Draft, h.ReadyAt = false, m.now()
	why := "CI green"
	switch {
	case bot == "":
	case reviewed:
		why += ", " + bot + " reviewed"
	case h.BotRounds >= maxBotRounds:
		why += fmt.Sprintf(", %d rounds on %s's feedback", h.BotRounds, bot)
	default:
		why += fmt.Sprintf(", no review from %s in %s", bot, botWait)
	}
	logEvent(h, m.now(), "ready", "marked ready for review: "+why)
	return nil
}

// SetReviewers replaces the human reviewers of a handoff. Before they are first
// requested this changes the plan; afterwards the next tick applies the
// difference on GitHub. Naming someone who was removed on GitHub asks for them
// again: it's the user's explicit choice.
func (m *Manager) SetReviewers(id string, logins []string) (Handoff, error) {
	var want []string
	for _, l := range logins {
		if l = strings.TrimSpace(l); !reviewer.MatchString(l) {
			return Handoff{}, invalid("reviewer %q isn't a GitHub login", l)
		}
		want = union(want, l)
	}
	h, err := m.setReviewers(id, want)
	if err != nil {
		return Handoff{}, err
	}
	slog.Info("lastmile reviewers set", "id", id, "reviewers", want)
	m.changed()
	m.Kick()
	return h, nil
}

// setReviewers stores want as the handoff's reviewers. Unlike update it leaves
// Gen alone: commit merges the list into a tick that is running.
func (m *Manager) setReviewers(id string, want []string) (Handoff, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.handoffs, func(h Handoff) bool { return h.ID == id })
	if i < 0 {
		return Handoff{}, ErrNotFound
	}
	h := m.handoffs[i]
	if Terminal(h.State) {
		return Handoff{}, invalid("handoff %s is %s", id, h.State)
	}
	// Someone newly named is asked for afresh: not removed any more, and not an earlier request
	// that the next tick would find missing and call removed again.
	newly := without(want, h.Reviewers...)
	h.Removed, h.Requested = without(h.Removed, newly...), without(h.Requested, newly...)
	h.Reviewers, h.ReviewersSet = want, true
	h.ReviewersPending = h.HumansRequested
	prev := m.handoffs[i]
	m.handoffs[i] = h
	if err := m.saveLocked(); err != nil {
		m.handoffs[i] = prev
		return Handoff{}, err
	}
	return h, nil
}
