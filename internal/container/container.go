// Package container runs review workloads in isolated containers. Runtime is
// stateless and ID-based, so a restarted waterboy can find and re-attach to
// containers it started earlier (by label).
package container

import (
	"context"
	"io"
)

// Mount bind-mounts Source (host) at Target (container).
type Mount struct {
	Source, Target string
	ReadOnly       bool
}

type Spec struct {
	Name     string
	Image    string
	Command  []string          // full argv; replaces the image's entrypoint
	Env      map[string]string // never put on the command line (ps would show secrets)
	EnvFiles []string          // KEY=value files; later ones win
	Mounts   []Mount
	Workdir  string
	Labels   map[string]string
}

type Info struct {
	ID       string
	Labels   map[string]string
	Running  bool
	ExitCode int // valid once !Running
}

type Runtime interface {
	// Available reports whether the runtime can start containers right now.
	Available(ctx context.Context) error
	Start(ctx context.Context, s Spec) (id string, err error)
	// Logs copies the container's output (from the beginning) to w until it exits or ctx ends.
	Logs(ctx context.Context, id string, w io.Writer) error
	// Wait blocks until the container exits and returns its exit code.
	Wait(ctx context.Context, id string) (exitCode int, err error)
	Stop(ctx context.Context, id string) error
	Remove(ctx context.Context, id string) error
	// List returns containers (running or not) carrying all of labels.
	List(ctx context.Context, labels map[string]string) ([]Info, error)
}
