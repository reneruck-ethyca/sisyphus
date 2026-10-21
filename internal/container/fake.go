package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"sync"
)

// Fake is an in-memory Runtime for tests. Containers run until Finish or Stop.
type Fake struct {
	mu          sync.Mutex
	next        int
	cs          map[string]*fakeContainer
	unavailable error
	started     []Spec
	available   int // Available calls

	// OnStart, if set, runs inside Start after the container exists (as if the
	// daemon had started it); an error makes Start fail anyway.
	OnStart func(ctx context.Context, id string) error
}

// AvailableCalls counts Available calls.
func (f *Fake) AvailableCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.available
}

// FailWait makes Wait on id return an error while the container keeps running.
func (f *Fake) FailWait(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.cs[id]; c != nil {
		close(c.waitFail)
	}
}

// SetUnavailable makes Available fail with err (nil = available again).
func (f *Fake) SetUnavailable(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unavailable = err
}

// Started returns every Spec passed to Start, in order.
func (f *Fake) Started() []Spec {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Spec(nil), f.started...)
}

type fakeContainer struct {
	labels   map[string]string
	done     chan struct{}
	waitFail chan struct{}
	code     int
	out      string
}

func (f *Fake) Available(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.available++
	return f.unavailable
}

func (f *Fake) Start(ctx context.Context, s Spec) (string, error) {
	f.mu.Lock()
	if f.unavailable != nil {
		f.mu.Unlock()
		return "", f.unavailable
	}
	f.started = append(f.started, s)
	id := f.add(s.Labels)
	hook := f.OnStart
	f.mu.Unlock()
	if hook != nil {
		if err := hook(ctx, id); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (f *Fake) add(labels map[string]string) string {
	if f.cs == nil {
		f.cs = map[string]*fakeContainer{}
	}
	f.next++
	id := fmt.Sprintf("c%d", f.next)
	f.cs[id] = &fakeContainer{labels: labels, done: make(chan struct{}), waitFail: make(chan struct{})}
	return id
}

// Seed adds a container as if a previous process had started it.
func (f *Fake) Seed(labels map[string]string, running bool, code int, out string) string {
	f.mu.Lock()
	id := f.add(labels)
	f.mu.Unlock()
	if !running {
		f.Finish(id, code, out)
	}
	return id
}

// Finish makes a running container exit with code after printing out.
func (f *Fake) Finish(id string, code int, out string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cs[id]
	if c == nil {
		return
	}
	select {
	case <-c.done:
	default:
		c.code, c.out = code, out
		close(c.done)
	}
}

func (f *Fake) get(id string) (*fakeContainer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if c := f.cs[id]; c != nil {
		return c, nil
	}
	return nil, errors.New("no such container: " + id)
}

func (f *Fake) Logs(ctx context.Context, id string, w io.Writer) error {
	c, err := f.get(id)
	if err != nil {
		return err
	}
	select {
	case <-c.done:
		_, err := io.WriteString(w, c.out)
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Fake) Wait(ctx context.Context, id string) (int, error) {
	c, err := f.get(id)
	if err != nil {
		return 0, err
	}
	select {
	case <-c.done:
		return c.code, nil
	case <-c.waitFail:
		return 0, errors.New("docker wait: connection reset")
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

func (f *Fake) Stop(_ context.Context, id string) error {
	f.Finish(id, 137, "")
	return nil
}

func (f *Fake) Remove(_ context.Context, id string) error {
	f.Finish(id, 137, "")
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.cs, id)
	return nil
}

func (f *Fake) List(_ context.Context, labels map[string]string) ([]Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.unavailable != nil {
		return nil, f.unavailable
	}
	var out []Info
	for id, c := range f.cs {
		match := true
		for k, v := range labels {
			match = match && c.labels[k] == v
		}
		if !match {
			continue
		}
		info := Info{ID: id, Labels: maps.Clone(c.labels), Running: true}
		select {
		case <-c.done:
			info.Running, info.ExitCode = false, c.code
		default:
		}
		out = append(out, info)
	}
	return out, nil
}
