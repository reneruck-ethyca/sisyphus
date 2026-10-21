package main

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LogEntry is one ops log line as UIs see it.
type LogEntry struct {
	Seq   uint64    `json:"seq"` // increasing; lets clients merge a backlog with a live stream
	Time  time.Time `json:"time"`
	Level string    `json:"level"`
	Msg   string    `json:"msg"`
	Attrs string    `json:"attrs,omitempty"` // "key=value …"
}

// LogBuffer keeps the most recent log entries and fans new ones out to subscribers.
// ponytail: in-memory ring only; history before the last restart lives in the log file.
type LogBuffer struct {
	mu   sync.Mutex
	ring []LogEntry
	next int // ring write position once full
	seq  uint64
	subs map[chan LogEntry]struct{}
}

func NewLogBuffer(size int) *LogBuffer {
	return &LogBuffer{ring: make([]LogEntry, 0, size), subs: map[chan LogEntry]struct{}{}}
}

// Recent returns the buffered entries, oldest first.
func (b *LogBuffer) Recent() []LogEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append(append([]LogEntry(nil), b.ring[b.next:]...), b.ring[:b.next]...)
}

// Subscribe delivers every new entry until cancel is called. A subscriber that
// falls more than 256 entries behind misses lines rather than stalling logging.
func (b *LogBuffer) Subscribe() (<-chan LogEntry, func()) {
	ch := make(chan LogEntry, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
	}
}

func (b *LogBuffer) add(e LogEntry) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seq++
	e.Seq = b.seq
	if len(b.ring) < cap(b.ring) {
		b.ring = append(b.ring, e)
	} else {
		b.ring[b.next] = e
		b.next = (b.next + 1) % len(b.ring)
	}
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// Handler tees records into b, then passes them on to next.
func (b *LogBuffer) Handler(next slog.Handler) slog.Handler {
	return &teeHandler{buf: b, next: next}
}

type teeHandler struct {
	buf   *LogBuffer
	next  slog.Handler
	attrs string // from With(); ponytail: groups aren't prefixed, nothing here uses WithGroup
}

func (h *teeHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	r.Attrs(func(a slog.Attr) bool { appendAttr(&sb, a); return true })
	h.buf.add(LogEntry{Time: r.Time, Level: r.Level.String(), Msg: r.Message, Attrs: sb.String()})
	return h.next.Handle(ctx, r)
}

func (h *teeHandler) WithAttrs(as []slog.Attr) slog.Handler {
	var sb strings.Builder
	sb.WriteString(h.attrs)
	for _, a := range as {
		appendAttr(&sb, a)
	}
	return &teeHandler{buf: h.buf, next: h.next.WithAttrs(as), attrs: sb.String()}
}

func (h *teeHandler) WithGroup(name string) slog.Handler {
	return &teeHandler{buf: h.buf, next: h.next.WithGroup(name), attrs: h.attrs}
}

func appendAttr(sb *strings.Builder, a slog.Attr) {
	if sb.Len() > 0 {
		sb.WriteByte(' ')
	}
	v := a.Value.Resolve().String()
	if strings.ContainsAny(v, " \"=") || v == "" {
		v = strconv.Quote(v)
	}
	sb.WriteString(a.Key)
	sb.WriteByte('=')
	sb.WriteString(v)
}
