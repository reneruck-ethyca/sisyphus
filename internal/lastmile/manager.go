package lastmile

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/jsonfile"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// Manager owns the handoffs: accepts them, keeps them in lastmile.json, and
// drives each through its lifecycle.
type Manager struct {
	GH GitHub

	cfg     config.Config
	locks   *tracker.Locks // shared with the tracker and reviews; "lastmile:<id>" guards a handoff's worktree
	path    string
	runHook func(ctx context.Context, command string, timeout time.Duration, dir string, env map[string]string) error
	kick    chan struct{}
	now     func() time.Time
	token   string // required to create handoffs (Authorized)

	// OnChange is called after a handoff changed; the app uses it to refresh the dashboard.
	OnChange func()

	mu       sync.Mutex     // guards handoffs, cursors and the file
	handoffs []Handoff      // oldest first
	cursors  map[string]int // by repo: where the reviewer roster's round-robin continues
}

type storeFile struct {
	Handoffs []Handoff      `json:"handoffs"`
	Cursors  map[string]int `json:"cursors,omitempty"`
}

// New loads the handoffs saved in stateDir/lastmile.json.
func New(cfg config.Config, gh GitHub, locks *tracker.Locks, stateDir string) (*Manager, error) {
	m := &Manager{GH: gh, cfg: cfg, locks: locks, path: filepath.Join(stateDir, "lastmile.json"),
		runHook: tracker.RunHook, kick: make(chan struct{}, 1), now: time.Now}
	var f storeFile
	if err := jsonfile.Load(m.path, &f); err != nil {
		return nil, err
	}
	m.handoffs, m.cursors = f.Handoffs, f.Cursors
	var err error
	if m.token, err = loadToken(stateDir); err != nil {
		return nil, err
	}
	return m, nil
}

// saveLocked writes the handoffs; the caller holds m.mu.
func (m *Manager) saveLocked() error {
	return jsonfile.Save(m.path, storeFile{Handoffs: m.handoffs, Cursors: m.cursors})
}

// List returns the handoffs, oldest first; never nil, so it encodes as [].
func (m *Manager) List() []Handoff {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Handoff{}, m.handoffs...)
}

// Get returns the handoff with this id.
func (m *Manager) Get(id string) (Handoff, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i := slices.IndexFunc(m.handoffs, func(h Handoff) bool { return h.ID == id })
	if i < 0 {
		return Handoff{}, false
	}
	return m.handoffs[i], true
}

// Kick asks Run for a tick now.
func (m *Manager) Kick() {
	select {
	case m.kick <- struct{}{}:
	default:
	}
}
