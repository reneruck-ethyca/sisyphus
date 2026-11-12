package tracker

import (
	"context"
	"sync"
)

// Events is how the tracker tells the review layer what happened. Calls come
// from the ticking goroutine and must not block. Tracked and Updated fire only
// once the worktree is synced to tr.HeadSHA, so work started from them never
// sees stale code.
type Events interface {
	Tracked(tr Tracked)                               // first time this PR is ready
	Updated(tr Tracked, newCommits bool, replies int) // ready again with something new
	Reclaim(tr Tracked)                               // the tracker needs the (locked) worktree back
	Dropped(tr Tracked, reason string)                // no longer tracked; its worktree may be gone
}

type nopEvents struct{}

func (nopEvents) Tracked(Tracked)            {}
func (nopEvents) Updated(Tracked, bool, int) {}
func (nopEvents) Reclaim(Tracked)            {}
func (nopEvents) Dropped(Tracked, string)    {}

// Locks serialises use of a PR's worktree between the tracker (sync, removal)
// and whatever runs in it (reviews). Keyed by PR ID.
// ponytail: one channel per PR ever seen, never freed; fine at review-queue scale.
type Locks struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

func (l *Locks) ch(id string) chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.m == nil {
		l.m = map[string]chan struct{}{}
	}
	c := l.m[id]
	if c == nil {
		c = make(chan struct{}, 1)
		l.m[id] = c
	}
	return c
}

// TryLock takes the lock if it's free.
func (l *Locks) TryLock(id string) (unlock func(), ok bool) {
	c := l.ch(id)
	select {
	case c <- struct{}{}:
		return func() { <-c }, true
	default:
		return nil, false
	}
}

// Lock waits for the lock until ctx ends.
func (l *Locks) Lock(ctx context.Context, id string) (unlock func(), err error) {
	c := l.ch(id)
	select {
	case c <- struct{}{}:
		return func() { <-c }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
