package main

import (
	"bytes"
	"encoding/json"
	"net/http"
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
	defer func(f func() string) { apiToken = f }(apiToken)
	apiToken = func() string { return apiTokenForTests }
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

func TestSendsTokenOnlyToLoopbackOnMutations(t *testing.T) {
	for _, c := range []struct {
		method, url string
		want        bool
	}{
		{"POST", "http://127.0.0.1:7465/api/handoffs", true},
		{"PUT", "http://localhost:7465/x", true},
		{"POST", "http://[::1]:7465/x", true},
		{"GET", "http://127.0.0.1:7465/x", false},
		{"POST", "http://example.com:7465/x", false},
		{"POST", "http://10.0.0.5:7465/x", false},
	} {
		req, _ := http.NewRequest(c.method, c.url, nil)
		if got := sendsToken(req); got != c.want {
			t.Errorf("%s %s = %v, want %v", c.method, c.url, got, c.want)
		}
	}
}

func TestTLMInstallSkill(t *testing.T) {
	claude, codex := t.TempDir(), t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	path := func(dir string) string { return filepath.Join(dir, "skills", skillName, "SKILL.md") }

	if code, out, stderr := tlm(t, "install-skill"); code != 0 || strings.Count(out, "installed") != 2 {
		t.Fatalf("first install = %d %q %q", code, out, stderr)
	}
	for _, dir := range []string{claude, codex} {
		if b, err := os.ReadFile(path(dir)); err != nil || !bytes.Equal(b, handoffSkill) {
			t.Errorf("%s: %v", path(dir), err)
		}
	}
	os.WriteFile(path(codex), []byte("old"), 0o644)
	if code, out, _ := tlm(t, "install-skill"); code != 0 || !strings.Contains(out, "claude: up to date") || !strings.Contains(out, "codex: updated") {
		t.Errorf("second install = %d %q", code, out)
	}
	if code, _, stderr := tlm(t, "install-skill", "--for", "gemini"); code != 1 || !strings.Contains(stderr, `unknown agent "gemini"`) {
		t.Errorf("unknown agent = %d %q", code, stderr)
	}
	if !bytes.HasPrefix(handoffSkill, []byte("---\nname: "+skillName+"\n")) {
		t.Error("embedded skill lost its frontmatter")
	}
}
