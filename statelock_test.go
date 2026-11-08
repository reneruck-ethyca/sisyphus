package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The test binary re-runs itself as the second instance, with SISYPHUS_LOCK_DIR set.
func TestLockStateRefusesSecondInstance(t *testing.T) {
	if dir := os.Getenv("SISYPHUS_LOCK_DIR"); dir != "" {
		if err := lockState(dir); err != nil {
			os.Stdout.WriteString(err.Error())
			os.Exit(3)
		}
		os.Exit(0)
	}
	dir := t.TempDir()
	if err := lockState(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { stateLock.Close(); stateLock = nil }()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLockStateRefusesSecondInstance$")
	cmd.Env = append(os.Environ(), "SISYPHUS_LOCK_DIR="+dir)
	out, err := cmd.Output()
	if err == nil || !strings.Contains(string(out), "already running on "+dir) || !strings.Contains(string(out), "pid ") {
		t.Fatalf("second instance: err=%v out=%q", err, out)
	}
	stateLock.Close()
	cmd = exec.Command(os.Args[0], "-test.run=^TestLockStateRefusesSecondInstance$")
	cmd.Env = append(os.Environ(), "SISYPHUS_LOCK_DIR="+dir)
	if out, err := cmd.Output(); err != nil {
		t.Fatalf("after release: err=%v out=%q", err, out)
	}
}
