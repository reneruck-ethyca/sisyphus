package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// lockState makes this process the only sisyphus on dir: every instance keeps its
// state (state.json, reviews.json, lastmile.json) in memory and writes it back
// whole, so a second one would overwrite the first's and run agents next to it.
// The flock goes with the process (its fd is close-on-exec, so a restart's
// re-exec takes it again); the pid in the file only names the holder in the error.
func lockState(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "sisyphus.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holder := "another process"
			if pid := strings.TrimSpace(string(b)); pid != "" {
				holder = "pid " + pid
			}
			return fmt.Errorf("another sisyphus (%s) is already running on %s; stop it first (make stop)", holder, dir)
		}
		return err
	}
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	}
	stateLock = f // held until the process exits
	return nil
}

var stateLock *os.File
