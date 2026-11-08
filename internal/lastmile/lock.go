package lastmile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// flockPoll is how often a run waiting for another process's worktree lock looks again.
var flockPoll = time.Second

// lockWorktree guards a handoff's worktree: the in-process lock "lastmile:<id>",
// then an flock under the state dir keyed by the worktree path, so a second
// sisyphus on the same state (a smoke-test binary next to the LaunchAgent) can't
// run an agent or git in the same worktree at the same time.
func (m *Manager) lockWorktree(ctx context.Context, id, dir string) (unlock func(), err error) {
	unlockMem, err := m.locks.Lock(ctx, "lastmile:"+id)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(dir))
	path := filepath.Join(filepath.Dir(m.path), "locks", hex.EncodeToString(sum[:8])+".lock")
	f, err := openLockFile(path)
	if err != nil {
		unlockMem()
		return nil, err
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			unlockMem()
			return nil, err
		}
		select {
		case <-time.After(flockPoll):
		case <-ctx.Done():
			f.Close()
			unlockMem()
			return nil, ctx.Err()
		}
	}
	return func() {
		f.Close() // releases the flock
		unlockMem()
	}, nil
}

func openLockFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
}
