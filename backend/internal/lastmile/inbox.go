package lastmile

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/github"
)

// Cursor is how far the review agent has read: ids only ever grow on GitHub, so
// "newer than" is an id comparison. It moves over the items an agent was handed.
type Cursor struct {
	Threads  map[string]int64 `json:"threads,omitempty"`  // thread id -> last comment id handed over
	Reviews  int64            `json:"reviews,omitempty"`  // last review id handed over
	Comments int64            `json:"comments,omitempty"` // last conversation comment id handed over
}

// adoptCursor is where an adopted PR's inbox starts: past everything already there
// except the threads still waiting on an answer from me (unresolved, not outdated,
// last comment someone else's). Review bodies and conversation comments have no
// resolved state, so they all count as answered.
func adoptCursor(in github.Inbox, me string) Cursor {
	var c Cursor
	for _, t := range in.Threads {
		if n := len(t.Comments); n > 0 && !t.Resolved && !t.Outdated && !strings.EqualFold(t.Comments[n-1].Author, me) {
			continue
		}
		for _, x := range t.Comments {
			if c.Threads == nil {
				c.Threads = map[string]int64{}
			}
			c.Threads[t.ID] = max(c.Threads[t.ID], x.ID)
		}
	}
	for _, x := range in.Reviews {
		c.Reviews = max(c.Reviews, x.ID)
	}
	for _, x := range in.Issue {
		c.Comments = max(c.Comments, x.ID)
	}
	return c
}

// Item kinds.
const (
	KindThread  = "thread"
	KindReview  = "review"
	KindComment = "comment"
)

// Comment is one comment of an Item as the review agent sees it.
type Comment struct {
	ID      int64     `json:"id"`
	Author  string    `json:"author"`
	Bot     bool      `json:"bot"`
	TLM     bool      `json:"tlm,omitempty"` // posted by TLM itself (as the user's login)
	New     bool      `json:"new"`           // not yet handed to an agent
	Body    string    `json:"body"`
	Created time.Time `json:"created"`
	URL     string    `json:"url,omitempty"`
}

// Item is one thing to answer: an inline thread with its full history, a
// review's body, or a conversation comment. Its ID is what an action names.
type Item struct {
	ID       string    `json:"id"` // the thread's node id, "review-<n>" or "comment-<n>"
	Kind     string    `json:"kind"`
	Path     string    `json:"path,omitempty"`
	Line     int       `json:"line,omitempty"`
	Outdated bool      `json:"outdated,omitempty"`
	Comments []Comment `json:"comments"`
}

func (i Item) newComments() []Comment {
	return slices.DeleteFunc(slices.Clone(i.Comments), func(c Comment) bool { return !c.New })
}

// Human says the item has a new comment from a person.
func (i Item) Human() bool {
	return slices.ContainsFunc(i.newComments(), func(c Comment) bool { return !c.Bot })
}

// BotThread says a bot started the conversation.
func (i Item) BotThread() bool { return len(i.Comments) > 0 && i.Comments[0].Bot }

// first is the comment an inline reply goes under.
func (i Item) first() Comment { return i.Comments[0] }

// last is the newest comment id of the item.
func (i Item) last() int64 {
	var n int64
	for _, c := range i.Comments {
		n = max(n, c.ID)
	}
	return n
}

// FeedbackRef is the snapshot's short form of a new item.
type FeedbackRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Author string `json:"author"` // of the newest new comment
	Bot    bool   `json:"bot"`
}

// isBot says login is a bot: GitHub's own mark, or a user account the config names.
func isBot(gh bool, login string, logins []string) bool {
	return gh || strings.HasSuffix(login, "[bot]") ||
		slices.ContainsFunc(logins, func(b string) bool { return strings.EqualFold(b, login) })
}

// Inbox is what people said on the PR, read against the handoff's cursor.
type Inbox struct {
	New        []Item         // items with something new, threads first
	Unresolved int            // review threads not resolved
	Unanswered int            // unresolved threads whose last comment is neither the PR author's nor TLM's
	BotReplies map[string]int // TLM's replies in each bot-started thread
}

// buildInbox marks authors, drops what TLM posted itself from "new", and picks
// the items with something new since the cursor. Resolved threads are history.
func buildInbox(in github.Inbox, h Handoff, botLogins []string, author string) Inbox {
	own := func(id int64) bool { return slices.Contains(h.Posted, id) }
	conv := func(c github.InboxComment, cursor int64) Comment {
		tlm := own(c.ID)
		return Comment{ID: c.ID, Author: c.Author, Bot: isBot(c.Bot, c.Author, botLogins), TLM: tlm,
			New: !tlm && c.ID > cursor, Body: c.Body, Created: c.Created, URL: c.URL}
	}
	var out Inbox
	for _, t := range in.Threads {
		if t.Resolved || len(t.Comments) == 0 {
			continue
		}
		out.Unresolved++
		if last := t.Comments[len(t.Comments)-1]; !strings.EqualFold(last.Author, author) && !own(last.ID) {
			out.Unanswered++
		}
		it := Item{ID: t.ID, Kind: KindThread, Path: t.Path, Line: t.Line, Outdated: t.Outdated}
		for _, c := range t.Comments {
			it.Comments = append(it.Comments, conv(c, h.Cursor.Threads[t.ID]))
		}
		if it.BotThread() {
			for _, c := range it.Comments {
				if c.TLM {
					if out.BotReplies == nil {
						out.BotReplies = map[string]int{}
					}
					out.BotReplies[t.ID]++
				}
			}
		}
		if len(it.newComments()) > 0 {
			out.New = append(out.New, it)
		}
	}
	for _, src := range []struct {
		kind   string
		cs     []github.InboxComment
		cursor int64
	}{{KindReview, in.Reviews, h.Cursor.Reviews}, {KindComment, in.Issue, h.Cursor.Comments}} {
		for _, c := range src.cs {
			if cc := conv(c, src.cursor); cc.New {
				out.New = append(out.New, Item{ID: fmt.Sprintf("%s-%d", src.kind, c.ID), Kind: src.kind, Comments: []Comment{cc}})
			}
		}
	}
	return out
}

// Refs is the snapshot's list of the new items.
func (b Inbox) Refs() []FeedbackRef {
	var refs []FeedbackRef
	for _, it := range b.New {
		n := it.newComments()
		refs = append(refs, FeedbackRef{ID: it.ID, Kind: it.Kind, Author: n[len(n)-1].Author, Bot: !it.Human()})
	}
	return refs
}

// humanCount counts the new comments written by people.
func (b Inbox) humanCount() int {
	n := 0
	for _, it := range b.New {
		for _, c := range it.newComments() {
			if !c.Bot {
				n++
			}
		}
	}
	return n
}

// advanceCursor moves c over items. It copies the thread map: handoffs are shared by copy.
func advanceCursor(c Cursor, items []Item) Cursor {
	c.Threads = maps.Clone(c.Threads)
	for _, it := range items {
		switch it.Kind {
		case KindThread:
			if c.Threads == nil {
				c.Threads = map[string]int64{}
			}
			c.Threads[it.ID] = max(c.Threads[it.ID], it.last())
		case KindReview:
			c.Reviews = max(c.Reviews, it.last())
		case KindComment:
			c.Comments = max(c.Comments, it.last())
		}
	}
	return c
}

// ReviewAction is the review agent's decision about one item.
type ReviewAction struct {
	ThreadID string `json:"thread_id"` // an Item.ID
	Action   string `json:"action"`    // fix | reply | defer | skip
	Reply    string `json:"reply"`
	Reason   string `json:"reason"`
}

// ReviewResult is /out/result.json of the review agent: the shared contract plus the actions.
type ReviewResult struct {
	AgentResult
	Actions []ReviewAction `json:"actions"`
}

// validActions keeps one well-formed action per item that was handed over. The
// ids the agent invents, repeats or gives a bad verb are dropped, and named in dropped.
func validActions(actions []ReviewAction, items []Item) (ok []ReviewAction, dropped []string) {
	for _, a := range actions {
		known := slices.ContainsFunc(items, func(it Item) bool { return it.ID == a.ThreadID })
		verb := slices.Contains([]string{"fix", "reply", "defer", "skip"}, a.Action)
		if !known || !verb || slices.ContainsFunc(ok, func(o ReviewAction) bool { return o.ThreadID == a.ThreadID }) {
			dropped = append(dropped, a.ThreadID+" ("+a.Action+")")
			continue
		}
		ok = append(ok, a)
	}
	return ok, dropped
}

// changesRequested lists the users whose latest standing review is CHANGES_REQUESTED.
func changesRequested(reviews []github.SnapReview) []string {
	latest := map[string]string{}
	for _, r := range slices.SortedStableFunc(slices.Values(reviews), func(a, b github.SnapReview) int { return a.SubmittedAt.Compare(b.SubmittedAt) }) {
		switch r.State {
		case "APPROVED", "CHANGES_REQUESTED":
			latest[r.User] = r.State
		case "DISMISSED":
			delete(latest, r.User)
		}
	}
	var users []string
	for u, st := range latest {
		if st == "CHANGES_REQUESTED" {
			users = append(users, u)
		}
	}
	slices.Sort(users)
	return users
}
