package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/lastmile"
)

func tlm(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := runTLM(args, &out, &errb)
	return code, strings.TrimSpace(out.String()), strings.TrimSpace(errb.String())
}

func TestTLMHandoffStatusWait(t *testing.T) {
	waitPoll = 10 * time.Millisecond
	srv, clone := handoffServer(t)
	u, _ := url.Parse(srv.URL)
	addr := "--addr=" + u.Host
	body := filepath.Join(t.TempDir(), "pr.md")
	os.WriteFile(body, []byte("the body"), 0o644)

	code, id, stderr := tlm(t, "handoff", addr, "--worktree", clone, "--base", "main", "--body-file", body,
		"--labels", "a, b", "--autonomy", "fix-ci,merge=shadow", "--notify", "true", "--requester", "me")
	if code != 0 || !strings.HasPrefix(id, "fidesplus-feature-x-") {
		t.Fatalf("handoff = %d %q %q", code, id, stderr)
	}

	code, out, _ := tlm(t, "status", id, addr, "--json")
	var h lastmile.Handoff
	if err := json.Unmarshal([]byte(out), &h); code != 0 || err != nil || h.Body != "the body" || len(h.Labels) != 2 || h.Autonomy.Merge != "shadow" || h.Requester != "me" {
		t.Fatalf("status --json = %d %v %s", code, err, out)
	}
	if code, out, _ := tlm(t, "status", addr, id); code != 0 || !strings.Contains(out, "handed-off") || !strings.Contains(out, "feature/x") {
		t.Errorf("status = %d %q", code, out)
	}

	if code, out, _ := tlm(t, "wait", id, addr, "--until", "handed-off,merged"); code != 0 || out != "handed-off" {
		t.Errorf("wait = %d %q", code, out)
	}

	// The same worktree again: the API's reason reaches the user, with a non-zero exit.
	if code, _, stderr := tlm(t, "handoff", addr, "--worktree", clone, "--base", "main"); code != 1 || !strings.Contains(stderr, "already owned") {
		t.Errorf("second handoff = %d %q", code, stderr)
	}
	if code, _, stderr := tlm(t, "status", "nope", addr); code != 1 || !strings.Contains(stderr, "404") {
		t.Errorf("unknown id = %d %q", code, stderr)
	}
	if code, _, _ := tlm(t, "status", addr); code != 1 {
		t.Errorf("status without id = %d, want 1", code)
	}
	if code, _, _ := tlm(t); code != 2 {
		t.Errorf("no command = %d, want 2", code)
	}
	if code, _, _ := tlm(t, "frobnicate"); code != 2 {
		t.Errorf("unknown command = %d, want 2", code)
	}
}
