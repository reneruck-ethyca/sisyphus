package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/testkit"
	"github.com/reneruck/sisyphus/internal/tracker"
)

func newTestServer(t *testing.T) (*httptest.Server, *app.Service, *app.LogBuffer) {
	t.Helper()
	cfg, tr := testkit.Tracker(t, &github.Fake{},
		&tracker.Tracked{PRRef: testkit.RefA, Title: "Fix <things>", Author: "octocat", URL: "https://github.com/ethyca/a/pull/1", HeadSHA: "x"})
	logs := app.NewLogBuffer(10)
	hs := newServer(cfg, tr, nil, logs)
	svc := hs.svc
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	return srv, svc, logs
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHTTPStatus(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/api/status")
	var st app.Status
	if err := json.Unmarshal([]byte(body), &st); code != 200 || err != nil {
		t.Fatalf("%d %v %s", code, err, body)
	}
	if st.User != "me" || len(st.PRs) != 1 || st.PRs[0].ID != testkit.RefA.Key() || st.PRs[0].Health != "ok" {
		t.Fatalf("status = %+v", st)
	}
}

func TestHTTPPR(t *testing.T) {
	srv, _, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/api/prs/ethyca/a/1")
	var pr app.PRView
	if err := json.Unmarshal([]byte(body), &pr); code != 200 || err != nil || pr.ID != testkit.RefA.Key() {
		t.Fatalf("%d %v %s", code, err, body)
	}
	for _, path := range []string{"/api/prs/ethyca/a/2", "/api/prs/ethyca/a/2/stack"} {
		if code, _ := get(t, srv.URL+path); code != 404 {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
}

func TestHTTPPRAgentOverride(t *testing.T) {
	srv, _, _ := newTestServer(t)
	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/prs/ethyca/a/1/agent", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := put(`{"agent":{"vendor":"codex","model":"gpt-5-codex","effort":"high"}}`); got != http.StatusNoContent {
		t.Fatalf("set override = %d", got)
	}
	_, body := get(t, srv.URL+"/api/prs/ethyca/a/1")
	var pr app.PRView
	if err := json.Unmarshal([]byte(body), &pr); err != nil || !pr.OverridesAgent || pr.Agent.Vendor != "codex" || pr.Agent.Effort != "high" {
		t.Fatalf("override = %+v, err=%v", pr, err)
	}
	if got := put(`{"inherit":true}`); got != http.StatusNoContent {
		t.Fatalf("inherit = %d", got)
	}
	_, body = get(t, srv.URL+"/api/prs/ethyca/a/1")
	if err := json.Unmarshal([]byte(body), &pr); err != nil || pr.OverridesAgent || pr.Agent.Vendor != "claude" {
		t.Fatalf("inherit = %+v, err=%v", pr, err)
	}
}

func TestHTTPRun(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	go svc.Run(t.Context())
	waitTick := func(after time.Time) time.Time {
		t.Helper()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
			if st := svc.Status(); !st.Running && st.LastRun.After(after) {
				return st.LastRun
			}
			if time.Now().After(deadline) {
				t.Fatal("tick never happened")
			}
		}
	}
	first := waitTick(time.Time{}) // the loop's own; the next one only comes from the request, the interval is an hour
	resp, err := http.Post(srv.URL+"/api/run", "", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%v %v", resp, err)
	}
	waitTick(first)
}

func TestHTTPSettings(t *testing.T) {
	srv, _, _ := newTestServer(t)
	put := func(body string) int {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+"/api/settings", strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := put(`{"org": "acme", "review.max_turns": 5}`); c != http.StatusNoContent {
		t.Fatalf("save = %d", c)
	}
	if c := put(`{"interval": "soon"}`); c != http.StatusBadRequest {
		t.Fatalf("bad save = %d", c)
	}
	var st settings
	if _, body := get(t, srv.URL+"/api/settings"); json.Unmarshal([]byte(body), &st) != nil || !slices.Equal(st.Groups, config.Groups) {
		t.Fatalf("/api/settings: %s", body)
	}
	for _, f := range st.Fields {
		if f.Key == "org" && f.Value != "acme" || f.Key == "review.max_turns" && f.Value != 5.0 || f.Key == "interval" && f.Value != "10m" {
			t.Errorf("%s = %v", f.Key, f.Value)
		}
	}

	restarted := make(chan struct{})
	defer func(r func()) { restart = r }(restart)
	restart = func() { close(restarted) }
	resp, err := http.Post(srv.URL+"/api/restart", "", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%v %v", resp, err)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("didn't restart")
	}
}

func TestHTTPRejectsForeignHostAndOrigin(t *testing.T) {
	srv, _, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Host = "evil.example:7465" // DNS rebinding
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %v %v", resp, err)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/run", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin run: %v %v", resp, err)
	}
}

func TestHTTPLogs(t *testing.T) {
	srv, _, logs := newTestServer(t)
	log := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil)))
	log.Warn("fetching <PR>", "pr", testkit.RefA.Key())

	code, body := get(t, srv.URL+"/api/logs")
	var entries []app.LogEntry
	if err := json.Unmarshal([]byte(body), &entries); code != 200 || err != nil || len(entries) != 1 || entries[0].Msg != "fetching <PR>" {
		t.Fatalf("%d %v %s", code, err, body)
	}
}

func TestHTTPHistory(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	if code, body := get(t, srv.URL+"/api/history"); code != 200 || body != "[]\n" { // still tracked
		t.Fatalf("history %d: %s", code, body)
	}
	if err := svc.Untrack(context.Background(), testkit.RefA.Key()); err != nil {
		t.Fatal(err)
	}
	code, body := get(t, srv.URL+"/api/history")
	var rows []app.HistoryRow
	if err := json.Unmarshal([]byte(body), &rows); code != 200 || err != nil || len(rows) != 1 ||
		rows[0].Reason != "untracked by hand" || rows[0].Title != "Fix <things>" {
		t.Fatalf("%d %v %s", code, err, body)
	}
}

func TestHTTPLogStream(t *testing.T) {
	srv, _, logs := newTestServer(t)
	log := slog.New(logs.Handler(slog.NewTextHandler(io.Discard, nil)))
	log.Info("backlog")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/logs/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	next := func() app.LogEntry {
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e app.LogEntry
				if err := json.Unmarshal([]byte(data), &e); err != nil {
					t.Fatal(err)
				}
				return e
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return app.LogEntry{}
	}
	if e := next(); e.Msg != "backlog" {
		t.Fatalf("first event %+v", e)
	}
	log.Info("live")
	if e := next(); e.Msg != "live" || e.Seq != 2 {
		t.Fatalf("second event %+v", e)
	}
}

// newReviewServer runs one placeholder review to completion against a real git
// worktree and a fake container runtime, then serves the dashboard.
func newReviewServer(t *testing.T) (*httptest.Server, *app.Service, review.Run) {
	return reviewServer(t, true)
}

// reviewServer starts one review against a real git worktree and a fake
// container runtime; finish lets it complete, otherwise it's left running.
func reviewServer(t *testing.T, finish bool) (*httptest.Server, *app.Service, review.Run) {
	t.Helper()
	_, clone := gittest.Setup(t)
	head := gittest.Git(t, clone, "rev-parse", "HEAD")
	gh := &github.Fake{Cands: []github.PRRef{testkit.RefA}, PRs: map[string]github.PR{
		testkit.RefA.Key(): {PRRef: testkit.RefA, State: "open", HeadSHA: head, RequestedReviewers: []string{"me"}}}}
	cfg, tr := testkit.Tracker(t, gh, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: head, SyncedSHA: head, Worktree: clone})
	rt := &container.Fake{}
	rev, err := review.New(config.Review{Agent: config.Agent{Vendor: "claude", Model: "opus"}, MaxTurns: 5, Timeout: config.Duration{Duration: time.Minute}, MaxConcurrent: 1,
		Repos: map[string]config.RepoReview{testkit.RefA.Repo: {Image: "img"}}},
		rt, tr.Locks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	hs := newServer(cfg, tr, rev, app.NewLogBuffer(10))
	svc := hs.svc
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); rev.Wait() }) // before the temp dirs go
	go rev.Run(ctx)
	svc.RunOnce(ctx)
	var run review.Run
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("review never finished")
		}
		if pr, _ := svc.PR(testkit.RefA.Key()); !finish && pr.Review.State == "running" && pr.Review.Latest().Container != "" {
			srv := httptest.NewServer(newHandler(hs))
			t.Cleanup(srv.Close)
			return srv, svc, *pr.Review.Latest()
		}
		if cs, _ := rt.List(ctx, nil); finish && len(cs) == 1 && cs[0].Running {
			rt.Finish(cs[0].ID, 0, "review <log>\n"+`{"type":"assistant","message":{"content":[{"type":"text","text":"Found <b>one</b> bug."}]}}`+"\n"+
				`{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":61000,"total_cost_usd":0.5,"session_id":"s1"}`+"\n")
		}
		if pr, _ := svc.PR(testkit.RefA.Key()); pr.Review.State == "" && pr.Review.Latest() != nil {
			run = *pr.Review.Latest()
			break
		}
	}
	os.WriteFile(filepath.Join(run.Dir, "out", "review.md"), []byte("# hi"), 0o644)
	os.Symlink("/etc/hosts", filepath.Join(run.Dir, "out", "escape"))
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	return srv, svc, run
}

func TestHTTPReviewRuns(t *testing.T) {
	srv, _, run := newReviewServer(t)
	api := "/api/prs/ethyca/a/1/runs/" + run.ID
	base := srv.URL + api + "/files"
	for path, want := range map[string]string{
		api + "/review":              "null", // the agent wrote no review.json
		api + "/files/log":           "review <log>\n",
		api + "/files/out/review.md": "# hi",
	} {
		code, body := get(t, srv.URL+path)
		if code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s = %d, want %q in:\n%s", path, code, want, body)
		}
	}
	if resp, _ := http.Get(base + "/log"); resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("log content type %q", resp.Header.Get("Content-Type"))
	}
	// Files a reviewer wrote must not run script on the dashboard's origin.
	if resp, _ := http.Get(base + "/out/review.md"); resp.Header.Get("Content-Security-Policy") != "sandbox" ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("run file headers: %v", resp.Header)
	}
	for _, path := range []string{"/out/escape", "/../../../../../../etc/hosts", "-nope/log"} {
		if code, body := get(t, base+path); code == 200 {
			t.Errorf("%s served: %.80s", path, body)
		}
	}
}

// A finished run's stream replays from ?from= and then says it's done.
func TestHTTPRunEventStream(t *testing.T) {
	srv, _, run := newReviewServer(t)
	resp, err := http.Get(srv.URL + "/api/prs/ethyca/a/1/runs/" + run.ID + "/events?from=1")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var kinds []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		if sc.Text() == "event: done" {
			break
		}
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var e review.Event
			json.Unmarshal([]byte(data), &e)
			kinds = append(kinds, e.Kind)
		}
	}
	if !slices.Equal(kinds, []string{"text", "result", "output"}) { // output: what came of posting
		t.Fatalf("streamed %q (line 0 should be skipped by from=1)", kinds)
	}
}

func TestHTTPShowsRunningContainer(t *testing.T) {
	_, svc, run := reviewServer(t, false)
	st := svc.Status()
	if len(st.Reviewing) != 1 {
		t.Fatalf("reviewing = %+v", st.Reviewing)
	}
	a := st.Reviewing[0]
	if a.PR != testkit.RefA.Key() || a.Run != run.ID || a.Container != run.Container || !strings.HasPrefix(a.Name, "sisyphus-ethyca-a-1-") {
		t.Fatalf("active review = %+v", a)
	}
	if pr, _ := svc.PR(testkit.RefA.Key()); pr.Verdict != "Reviewing" || pr.Review.Latest().ID != run.ID {
		t.Errorf("PR = %s, latest run %+v", pr.Verdict, pr.Review.Latest())
	}
}

func TestHTTPShowsNoContainerWhenIdle(t *testing.T) {
	_, svc, _ := newReviewServer(t)
	if r := svc.Status().Reviewing; len(r) != 0 {
		t.Fatalf("reviewing = %+v", r)
	}
}

func TestHTTPReviewNow(t *testing.T) {
	srv, svc, _ := newReviewServer(t)
	post := func(path string) *http.Response {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := post("/api/prs/ethyca/a/1/review"); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if pr, _ := svc.PR(testkit.RefA.Key()); len(pr.Review.Runs) == 2 && pr.Review.Runs[0].Job.Reason == "manual" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("manual review never started")
		}
	}
	if resp := post("/api/prs/ethyca/a/99/review"); resp.StatusCode != http.StatusConflict {
		t.Fatalf("untracked PR: status %d", resp.StatusCode)
	}
}

func TestHTTPWipe(t *testing.T) {
	srv, svc, run := newReviewServer(t)
	resp, err := http.Post(srv.URL+"/api/prs/ethyca/a/1/wipe", "", nil)
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Fatalf("wipe: %v %v", resp.StatusCode, err)
	}
	// The history is gone; a fresh review may already be queued or starting (Ensure).
	pr, _ := svc.PR(testkit.RefA.Key())
	if pr.ID == "" || slices.ContainsFunc(pr.Review.Runs, func(r review.Run) bool { return r.ID == run.ID }) {
		t.Fatalf("after wipe: %+v (want tracked, old run gone)", pr.Review)
	}
	if resp, _ := http.Post(srv.URL+"/api/prs/ethyca/a/99/wipe", "", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("untracked: %d", resp.StatusCode)
	}
}

func TestHTTPPRActions(t *testing.T) {
	srv, svc, _ := newReviewServer(t)
	post := func(action string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/prs/ethyca/a/1/"+action, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("reset"); code != http.StatusNoContent {
		t.Fatalf("reset: %d", code)
	}
	if code := post("nope"); code != http.StatusNotFound {
		t.Fatalf("unknown action: %d", code)
	}
	if code := post("untrack"); code != http.StatusNoContent {
		t.Fatalf("untrack: %d", code)
	}
	if st := svc.Status(); len(st.PRs) != 0 || len(st.Ignored) != 1 {
		t.Fatalf("after untrack: PRs %d, ignored %+v", len(st.PRs), st.Ignored)
	}
	if code := post("track"); code != http.StatusNoContent || len(svc.Status().Ignored) != 0 {
		t.Fatalf("track: %d, ignored %+v", code, svc.Status().Ignored)
	}
}

func TestHTTPStatusStream(t *testing.T) {
	statusEvery = 10 * time.Millisecond
	cfg, tr := testkit.Tracker(t, &github.Fake{}, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x"})
	hs := newServer(cfg, tr, nil, app.NewLogBuffer(10))
	svc := hs.svc
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/api/status/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	next := func() string {
		t.Helper()
		var line string
		for line == "" { // skip the blank line between events
			b := make([]byte, 1)
			for {
				if _, err := resp.Body.Read(b); err != nil {
					t.Fatal(err)
				}
				if b[0] == '\n' {
					break
				}
				line += string(b)
			}
		}
		return strings.TrimPrefix(line, "data: ")
	}
	if first := next(); !strings.Contains(first, `"prs":[{`) || strings.Contains(first, `"running":true`) {
		t.Fatalf("first event = %s", first)
	}
	svc.RunOnce(ctx) // changes LastRun: a second event follows
	if second := next(); !strings.Contains(second, `"last_run":"2`) {
		t.Fatalf("second event = %s", second)
	}
}

func TestHTTPOpen(t *testing.T) {
	var opened []string
	orig := openInBrowser
	openInBrowser = func(u string) error { opened = append(opened, u); return nil }
	t.Cleanup(func() { openInBrowser = orig })
	srv, _, run := newReviewServer(t)
	post := func(path, body string) int {
		t.Helper()
		resp, err := http.Post(srv.URL+path, "application/x-www-form-urlencoded", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if code := post("/api/open", "url=https://github.com/ethyca/a/pull/1"); code != http.StatusNoContent {
		t.Fatalf("github link: %d", code)
	}
	if code := post("/api/open", "url=https://evil.example/x"); code != http.StatusForbidden {
		t.Fatalf("other link: %d", code)
	}
	if code := post("/api/open", "url=file:///etc/hosts"); code != http.StatusForbidden {
		t.Fatalf("file link: %d", code)
	}
	if code := post("/api/prs/ethyca/a/1/runs/"+run.ID+"/open", ""); code != http.StatusNoContent {
		t.Fatalf("run dir: %d", code)
	}
	if code := post("/api/prs/ethyca/a/1/runs/nope/open", ""); code != http.StatusNotFound {
		t.Fatalf("unknown run: %d", code)
	}
	if want := []string{"https://github.com/ethyca/a/pull/1", run.Dir}; !slices.Equal(opened, want) {
		t.Fatalf("opened %v, want %v", opened, want)
	}
}

// handoffServer serves an app whose last mile can take a handoff of the returned clone (feature/x, one commit ahead of main).
func handoffServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	upstream, clone := gittest.Setup(t)
	gittest.Git(t, clone, "config", "url."+upstream+".insteadOf", "https://github.com/ethyca/fidesplus.git")
	gittest.Git(t, clone, "remote", "set-url", "origin", "https://github.com/ethyca/fidesplus.git")
	cfg, tr := testkit.Tracker(t, &github.Fake{})
	cfg.Review.Repos = map[string]config.RepoReview{"ethyca/fidesplus": {Image: "i"}}
	hs := newServer(cfg, tr, nil, app.NewLogBuffer(10))
	svc := hs.svc
	var err error
	dir := t.TempDir()
	lm, err := lastmile.New(cfg, &lastmile.FakeGitHub{}, tr.Locks, dir)
	if err != nil {
		t.Fatal(err)
	}
	svc.UseLastMile(lm)
	b, err := os.ReadFile(lastmile.TokenFile(dir))
	if err != nil {
		t.Fatal(err)
	}
	apiTokenForTests = strings.TrimSpace(string(b))
	hs.token, testBearer = apiTokenForTests, apiTokenForTests // in a real install they're the same file
	t.Cleanup(func() { apiTokenForTests, testBearer = "", testToken })
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	return srv, clone
}

// apiTokenForTests is what post sends as the handoff API token.
var apiTokenForTests string

func post(t *testing.T, url, body string) (int, string) {
	t.Helper()
	return postToken(t, url, body, apiTokenForTests)
}

func postToken(t *testing.T, url, body, token string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token) // "" too: a request that sends no valid token
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHTTPHandoffs(t *testing.T) {
	srv, clone := handoffServer(t)
	if code, body := get(t, srv.URL+"/api/handoffs"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Fatalf("empty list = %d %s", code, body)
	}
	code, body := post(t, srv.URL+"/api/handoffs", `{"worktree": `+strconv.Quote(clone)+`, "base": "main", "autonomy": "fix-ci", "requester": "orca"}`)
	var h lastmile.Handoff
	if err := json.Unmarshal([]byte(body), &h); code != 201 || err != nil || h.ID == "" || h.State != lastmile.StateHandedOff || h.Repo != "ethyca/fidesplus" {
		t.Fatalf("create = %d %v %s", code, err, body)
	}
	if code, body := get(t, srv.URL+"/api/handoffs/"+h.ID); code != 200 || !strings.Contains(body, h.ID) {
		t.Errorf("get = %d %s", code, body)
	}
	if code, body := get(t, srv.URL+"/api/handoffs"); code != 200 || !strings.Contains(body, h.ID) {
		t.Errorf("list = %d %s", code, body)
	}
	if code, _ := get(t, srv.URL+"/api/handoffs/nope"); code != 404 {
		t.Errorf("unknown id = %d, want 404", code)
	}

	for name, c := range map[string]struct{ body, want string }{
		"owned":      {`{"worktree": ` + strconv.Quote(clone) + `, "base": "main"}`, "already owned"},
		"merge auto": {`{"worktree": ` + strconv.Quote(clone) + `, "base": "main", "autonomy": "merge=auto"}`, "not supported yet"},
		"no base":    {`{"worktree": ` + strconv.Quote(clone) + `}`, "base"},
	} {
		if code, body := post(t, srv.URL+"/api/handoffs", c.body); code != 400 || !strings.Contains(body, c.want) {
			t.Errorf("%s = %d %s, want 400 containing %q", name, code, body, c.want)
		}
	}
	for _, tok := range []string{"", "wrong"} {
		if code, _ := postToken(t, srv.URL+"/api/handoffs", `{"worktree": "/x", "base": "main"}`, tok); code != 401 {
			t.Errorf("token %q = %d, want 401", tok, code)
		}
	}
	for _, bad := range []string{`nope`, `{"worktree": "/x", "base": "main", "surprise": 1}`} {
		if code, _ := post(t, srv.URL+"/api/handoffs", bad); code != 400 {
			t.Errorf("%s = %d, want 400", bad, code)
		}
	}
}

func TestHTTPHandoffsDisabled(t *testing.T) {
	srv, _, _ := newTestServer(t)
	if code, body := get(t, srv.URL+"/api/handoffs"); code != 200 || strings.TrimSpace(body) != "[]" {
		t.Errorf("list = %d %s", code, body)
	}
	if code, _ := get(t, srv.URL+"/api/handoffs/x"); code != 404 {
		t.Errorf("get = %d, want 404", code)
	}
	if code, _ := postToken(t, srv.URL+"/api/handoffs", `{"worktree": "/x", "base": "main"}`, testToken); code != 503 {
		t.Errorf("create = %d, want 503", code)
	}
}

func TestHTTPHandoffReviewers(t *testing.T) {
	srv, clone := handoffServer(t)
	_, body := post(t, srv.URL+"/api/handoffs", `{"worktree": `+strconv.Quote(clone)+`, "base": "main"}`)
	var h lastmile.Handoff
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	put := func(path, body string) (int, string) {
		req, _ := http.NewRequest(http.MethodPut, srv.URL+path, strings.NewReader(body))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	for _, in := range []string{`["alice","bob"]`, `{"reviewers": ["alice","bob"]}`} {
		code, body := put("/api/handoffs/"+h.ID+"/reviewers", in)
		var got lastmile.Handoff
		if err := json.Unmarshal([]byte(body), &got); code != 200 || err != nil || strings.Join(got.Reviewers, ",") != "alice,bob" || !got.ReviewersSet {
			t.Errorf("%s = %d %s", in, code, body)
		}
	}
	if code, body := put("/api/handoffs/"+h.ID+"/reviewers", `["not a login"]`); code != 400 || !strings.Contains(body, "GitHub login") {
		t.Errorf("bad login = %d %s", code, body)
	}
	if code, _ := put("/api/handoffs/"+h.ID+"/reviewers", `"x"`); code != 400 {
		t.Errorf("bad body = %d", code)
	}
	if code, _ := put("/api/handoffs/nope/reviewers", `[]`); code != 404 {
		t.Errorf("unknown = %d", code)
	}
	if code, body := get(t, srv.URL+"/api/status"); code != 200 || !strings.Contains(body, `"handoffs"`) || !strings.Contains(body, h.ID) {
		t.Errorf("status = %d", code)
	}
}

func TestHTTPHandoffResumeCancel(t *testing.T) {
	srv, clone := handoffServer(t)
	_, body := post(t, srv.URL+"/api/handoffs", `{"worktree": `+strconv.Quote(clone)+`, "base": "main"}`)
	var h lastmile.Handoff
	if err := json.Unmarshal([]byte(body), &h); err != nil {
		t.Fatal(err)
	}
	if code, body := post(t, srv.URL+"/api/handoffs/"+h.ID+"/resume", ""); code != 400 || !strings.Contains(body, "not needs-human") {
		t.Errorf("resume of a handed-off = %d %s", code, body)
	}
	if code, _ := post(t, srv.URL+"/api/handoffs/nope/resume", ""); code != 404 {
		t.Errorf("unknown resume = %d", code)
	}
	code, body := post(t, srv.URL+"/api/handoffs/"+h.ID+"/cancel", "")
	if code != 200 || !strings.Contains(body, `"state":"cancelled"`) {
		t.Errorf("cancel = %d %s", code, body)
	}
	if code, _ := post(t, srv.URL+"/api/handoffs/"+h.ID+"/cancel", ""); code != 400 {
		t.Errorf("second cancel = %d", code)
	}
}

func TestHTTPShadowReport(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{})
	hs := newServer(cfg, tr, nil, app.NewLogBuffer(10))
	svc := hs.svc
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	if code, _ := get(t, srv.URL+"/api/lastmile/shadow"); code != 404 {
		t.Errorf("disabled = %d", code)
	}
	dir := t.TempDir()
	var err error
	lm, err := lastmile.New(cfg, &lastmile.FakeGitHub{}, tr.Locks, dir)
	if err != nil {
		t.Fatal(err)
	}
	svc.UseLastMile(lm)
	if code, body := get(t, srv.URL+"/api/lastmile/shadow"); code != 200 || !strings.Contains(body, `"observed":0`) || !strings.Contains(body, `"prs":[]`) {
		t.Errorf("empty = %d %s", code, body)
	}
	log := `{"time":"2026-11-05T10:00:00Z","handoff_id":"h1","repo":"o/r","pr":1,"head":"a","would_merge":true,"blockers":[],"signals":{}}
garbage
{"time":"2026-11-05T11:00:00Z","handoff_id":"h1","repo":"o/r","pr":1,"outcome":"merged","merged_by":"carol","last_verdict":null,"verdict_head_matches":true}
`
	os.WriteFile(filepath.Join(dir, "lastmile-shadow.jsonl"), []byte(log), 0o600)
	code, body := get(t, srv.URL+"/api/lastmile/shadow")
	if code != 200 || !strings.Contains(body, `"merged":1`) || !strings.Contains(body, `"would_merge_yes":{"merged":1`) || strings.Contains(body, `"history"`) {
		t.Errorf("report = %d %s", code, body)
	}
	if code, body = get(t, srv.URL+"/api/lastmile/shadow?pr=o/r%231"); code != 200 || !strings.Contains(body, `"history":[`) {
		t.Errorf("by pr = %d %s", code, body)
	}
	if code, _ = get(t, srv.URL+"/api/lastmile/shadow?pr=bad"); code != 400 {
		t.Errorf("bad pr = %d", code)
	}
}

func TestHTTPLastMilePagesDisabled(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{})
	srv := httptest.NewServer(newHandler(newServer(cfg, tr, nil, app.NewLogBuffer(10))))
	t.Cleanup(srv.Close)
	for _, p := range []string{"/api/lastmile/shadow", "/api/handoffs/x"} {
		if code, _ := get(t, srv.URL+p); code != 404 {
			t.Errorf("%s = %d", p, code)
		}
	}
}

func TestHTTPSkill(t *testing.T) {
	claude, codex := t.TempDir(), t.TempDir() // both dirs exist: both agents count as found
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	path := func(dir string) string { return filepath.Join(dir, "skills", skillName, "SKILL.md") }
	os.MkdirAll(filepath.Dir(path(claude)), 0o755)
	os.WriteFile(path(claude), []byte("old"), 0o644)
	srv, _, _ := newTestServer(t) // checks the skills at startup

	act := func(agent, action string) skillStatus {
		t.Helper()
		resp, err := http.Post(srv.URL+"/api/skill/"+agent+"/"+action, "", nil)
		if err != nil || resp.StatusCode != http.StatusAccepted {
			t.Fatalf("%s %s = %v %v", agent, action, resp, err)
		}
		resp.Body.Close()
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			_, body := get(t, srv.URL+"/api/skill")
			var st skillStatus
			json.Unmarshal([]byte(body), &st)
			if st.Busy == "" {
				if st.Error != "" {
					t.Fatalf("%s %s: %s", agent, action, st.Error)
				}
				return st
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s %s never finished", agent, action)
			}
		}
	}
	states := func(st skillStatus) string {
		var out []string
		for _, a := range st.Agents {
			out = append(out, a.ID+"="+a.State)
		}
		return strings.Join(out, " ")
	}
	if got := states(skillsStatus()); got != "claude=stale codex=absent" {
		t.Fatalf("at startup: %s", got)
	}
	if got := states(act("claude", "install")); got != "claude=current codex=absent" {
		t.Errorf("after the update: %s", got)
	}
	if b, _ := os.ReadFile(path(claude)); !bytes.Equal(b, handoffSkill) {
		t.Error("skill not updated")
	}
	if got := states(act("codex", "install")); got != "claude=current codex=current" {
		t.Errorf("after installing for codex: %s", got)
	}
	if got := states(act("claude", "uninstall")); got != "claude=absent codex=current" {
		t.Errorf("after uninstalling for claude: %s", got)
	}
	if _, err := os.Stat(filepath.Dir(path(claude))); !os.IsNotExist(err) {
		t.Errorf("claude's skill dir is still there: %v", err)
	}
	if _, body := get(t, srv.URL+"/settings"); strings.Contains(body, "skill update available") {
		t.Error("dot still shown")
	}
	for _, p := range []string{"gemini/install", "claude/delete"} {
		if resp, _ := http.Post(srv.URL+"/api/skill/"+p, "", nil); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", p, resp.StatusCode)
		}
	}
}

// A finished run's log: its events so far, and where a live stream would carry on.
func TestHTTPRunLog(t *testing.T) {
	srv, _, run := newReviewServer(t)
	code, body := get(t, srv.URL+"/api/prs/ethyca/a/1/runs/"+run.ID+"/log")
	var l runLog
	if err := json.Unmarshal([]byte(body), &l); code != 200 || err != nil || l.Next == 0 {
		t.Fatalf("%d %v %s", code, err, body)
	}
	var kinds []string
	for _, e := range l.Events {
		kinds = append(kinds, e.Kind)
	}
	if !slices.Contains(kinds, "output") || !slices.Contains(kinds, "text") || !slices.Contains(kinds, "result") {
		t.Errorf("kinds = %q", kinds)
	}
	if code, _ := get(t, srv.URL+"/api/prs/ethyca/a/1/runs/nope/log"); code != 404 {
		t.Errorf("unknown run = %d", code)
	}
}

// The UI's files are served as they are; any other page path gets index.html for the UI to route.
func TestHTTPServesUI(t *testing.T) {
	dist := fstest.MapFS{"index.html": {Data: []byte("<!doctype html>ui")}, "assets/app.js": {Data: []byte("js()")}}
	srv := httptest.NewServer(spa(dist))
	t.Cleanup(srv.Close)
	for path, want := range map[string]string{"/": "ui", "/prs/o/r/1": "ui", "/lastmile/h1": "ui", "/assets/app.js": "js()"} {
		if code, body := get(t, srv.URL+path); code != 200 || !strings.Contains(body, want) {
			t.Errorf("%s = %d %q", path, code, body)
		}
	}
	if code, _ := get(t, srv.URL+"/api/nope"); code != 404 {
		t.Errorf("unknown API path = %d", code)
	}
	empty := httptest.NewServer(spa(fstest.MapFS{}))
	t.Cleanup(empty.Close)
	if code, body := get(t, empty.URL+"/"); code != http.StatusServiceUnavailable || !strings.Contains(body, "make build") {
		t.Errorf("unbuilt UI = %d %q", code, body)
	}
}

const testToken = "test-token"

// testBearer is the token every test request carries unless it sets its own Authorization.
var testBearer = testToken

type bearerTransport struct{ next http.RoundTripper }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Header.Get("Authorization") == "" {
		r = r.Clone(r.Context())
		r.Header.Set("Authorization", "Bearer "+testBearer)
	}
	return b.next.RoundTrip(r)
}

func TestMain(m *testing.M) {
	loadAPIToken = func(string) (string, error) { return testToken, nil }
	http.DefaultTransport = bearerTransport{http.DefaultTransport}
	os.Exit(m.Run())
}

// Every /api call needs the token, as a Bearer token or the session cookie the page gets
// for it; loopback isn't enough, since review containers reach the port with any Host.
func TestHTTPRequiresToken(t *testing.T) {
	cfg, tr := testkit.Tracker(t, &github.Fake{})
	hs := newServer(cfg, tr, nil, app.NewLogBuffer(10))
	srv := httptest.NewServer(newHandler(hs))
	t.Cleanup(srv.Close)
	raw := &http.Client{Transport: http.DefaultTransport.(bearerTransport).next, CheckRedirect: http.DefaultClient.CheckRedirect}
	do := func(method, path, auth string, cookie *http.Cookie) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"org": "evil"}`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		resp, err := raw.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	for _, c := range []struct{ method, path string }{{"GET", "/api/status"}, {"GET", "/api/settings"}, {"PUT", "/api/settings"}, {"POST", "/api/restart"},
		{"POST", "/api/handoffs/x/dirty/agent"}, {"GET", "/api/logs/stream"}} {
		if r := do(c.method, c.path, "", nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s without token = %d", c.method, c.path, r.StatusCode)
		}
		if r := do(c.method, c.path, "Bearer wrong", nil); r.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s with a wrong token = %d", c.method, c.path, r.StatusCode)
		}
	}
	if r := do("GET", "/api/status", "Bearer "+testToken, nil); r.StatusCode != 200 {
		t.Errorf("bearer = %d", r.StatusCode)
	}
	// The page itself isn't secret; it trades a one-time code for a cookie that isn't the token.
	if r := do("GET", "/prs/o/r/1", "", nil); r.StatusCode == http.StatusUnauthorized {
		t.Errorf("page = %d", r.StatusCode)
	}
	if r := do("GET", "/api/session?code="+testToken, "", nil); r.StatusCode != http.StatusUnauthorized || len(r.Cookies()) != 0 {
		t.Errorf("session for the token instead of a code = %d %v", r.StatusCode, r.Cookies())
	}
	code := strings.TrimPrefix(hs.dashboardURL(""), "?code=")
	r := do("GET", "/api/session?code="+code, "", nil)
	if r.StatusCode != http.StatusNoContent || len(r.Cookies()) != 1 || !r.Cookies()[0].HttpOnly || r.Cookies()[0].SameSite != http.SameSiteStrictMode ||
		r.Cookies()[0].Value == testToken {
		t.Fatalf("session = %d %+v", r.StatusCode, r.Cookies())
	}
	if again := do("GET", "/api/session?code="+code, "", nil); again.StatusCode != http.StatusUnauthorized {
		t.Errorf("a used code = %d", again.StatusCode)
	}
	if r := do("GET", "/api/status", "Bearer "+r.Cookies()[0].Value, nil); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("the cookie's value as a Bearer token = %d", r.StatusCode)
	}
	if r := do("GET", "/api/status", "", r.Cookies()[0]); r.StatusCode != 200 {
		t.Errorf("cookie = %d", r.StatusCode)
	}
}
