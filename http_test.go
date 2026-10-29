package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/github"
	"github.com/reneruck/sisyphus/internal/gittest"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/testkit"
	"github.com/reneruck/sisyphus/internal/tracker"
)

func newTestServer(t *testing.T) (*httptest.Server, *app.Service, *app.LogBuffer) {
	t.Helper()
	cfg, tr := testkit.Tracker(t, &github.Fake{},
		&tracker.Tracked{PRRef: testkit.RefA, Title: "Fix <things>", Author: "octocat", URL: "https://github.com/ethyca/a/pull/1", HeadSHA: "x"})
	logs := app.NewLogBuffer(10)
	svc := app.NewService(cfg, tr, nil, logs)
	srv := httptest.NewServer(newHandler(svc))
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
	for _, path := range []string{"/api/prs/ethyca/a/2", "/prs/ethyca/a/2"} {
		if code, _ := get(t, srv.URL+path); code != 404 {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
}

func TestHTTPRun(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	resp, err := http.Post(srv.URL+"/api/run", "", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%v %v", resp, err)
	}
	if !svc.RunQueued() {
		t.Fatal("tick not queued")
	}
}

func TestHTTPSettings(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/settings")
	if code != 200 || !strings.Contains(body, `name="review.model" value="opus"`) || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("/settings = %d: %s", code, body)
	}
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
	st, err := svc.Settings()
	if err != nil {
		t.Fatal(err)
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

func TestHTTPPages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for path, want := range map[string][]string{
		"/": {`href="/prs/ethyca/a/1"`, "Fix &lt;things&gt;", "Starting…", "<h1>Overview</h1>", `class="chip neutral">Not reviewed`},
		"/prs/ethyca/a/1": {"ethyca/a #1", "Fix &lt;things&gt;", "https://github.com/ethyca/a/pull/1", `aria-current="page"`,
			`href="https://github.com/octocat" target="_blank" rel="noopener"`, `href="https://github.com/ethyca/a/pull/1" target="_blank" rel="noopener"`},
	} {
		code, body := get(t, srv.URL+path)
		if code != 200 {
			t.Fatalf("%s = %d: %s", path, code, body)
		}
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("%s missing %q", path, w)
			}
		}
	}
}

func TestHTTPRejectsForeignHostAndOrigin(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Host = "evil.example:7465" // DNS rebinding
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %v %v", resp, err)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/run", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden || svc.RunQueued() {
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
	code, body = get(t, srv.URL+"/logs")
	if code != 200 || !strings.Contains(body, "fetching &lt;PR&gt;") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("logs page %d: %s", code, body)
	}
}

func TestHTTPHistory(t *testing.T) {
	srv, svc, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/history")
	if code != 200 || strings.Contains(body, "Fix &lt;things&gt;") { // still tracked
		t.Fatalf("history page %d: %s", code, body)
	}
	if err := svc.Untrack(context.Background(), testkit.RefA.Key()); err != nil {
		t.Fatal(err)
	}
	code, body = get(t, srv.URL+"/api/history")
	var rows []app.HistoryRow
	if err := json.Unmarshal([]byte(body), &rows); code != 200 || err != nil || len(rows) != 1 ||
		rows[0].Reason != "untracked by hand" || rows[0].Title != "Fix <things>" {
		t.Fatalf("%d %v %s", code, err, body)
	}
	code, body = get(t, srv.URL+"/history/ethyca/a/1")
	if code != 200 || !strings.Contains(body, "untracked by hand") || !strings.Contains(body, "archive-banner") {
		t.Fatalf("history PR page %d: %s", code, body)
	}
	if code, _ = get(t, srv.URL+"/history/ethyca/a/2"); code != 404 {
		t.Fatalf("unknown history PR = %d", code)
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
	rev, err := review.New(config.Review{Model: "opus", MaxTurns: 5, Timeout: config.Duration{Duration: time.Minute}, MaxConcurrent: 1,
		Repos: map[string]config.RepoReview{testkit.RefA.Repo: {Image: "img"}}},
		rt, tr.Locks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(cfg, tr, rev, app.NewLogBuffer(10))
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
			srv := httptest.NewServer(newHandler(svc))
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
	srv := httptest.NewServer(newHandler(svc))
	t.Cleanup(srv.Close)
	return srv, svc, run
}

func TestHTTPReviewRuns(t *testing.T) {
	srv, _, run := newReviewServer(t)
	base := srv.URL + "/prs/ethyca/a/1/runs/" + run.ID
	for path, want := range map[string]string{
		"/prs/ethyca/a/1":                `href="/prs/ethyca/a/1/runs/` + run.ID + `"`,
		"/prs/ethyca/a/1/runs/" + run.ID: `Found &lt;b&gt;one&lt;/b&gt; bug.`,
		"/":                              `class="chip neutral">Not posted`,
		"/prs/ethyca/a/1/runs/" + run.ID + "/log":           "review <log>\n",
		"/prs/ethyca/a/1/runs/" + run.ID + "/out/review.md": "# hi",
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

func TestHTTPRunPageRendersEvents(t *testing.T) {
	srv, _, run := newReviewServer(t)
	_, body := get(t, srv.URL+"/prs/ethyca/a/1/runs/"+run.ID)
	for _, want := range []string{`id="setup">review &lt;log&gt;`, `class="ev text"`, `class="ev result"`,
		"success · 3 turns · 1m1s · $0.50", `data-live="false"`} {
		if !strings.Contains(body, want) {
			t.Errorf("run page missing %q", want)
		}
	}
	if strings.Contains(body, `http-equiv="refresh"`) {
		t.Error("run page must not auto-reload")
	}
	if code, _ := get(t, srv.URL+"/prs/ethyca/a/1/runs/nope"); code != 404 {
		t.Errorf("unknown run = %d", code)
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
	srv, svc, run := reviewServer(t, false)
	st := svc.Status()
	if len(st.Reviewing) != 1 {
		t.Fatalf("reviewing = %+v", st.Reviewing)
	}
	a := st.Reviewing[0]
	if a.PR != testkit.RefA.Key() || a.Run != run.ID || a.Container != run.Container || !strings.HasPrefix(a.Name, "sisyphus-ethyca-a-1-") {
		t.Fatalf("active review = %+v", a)
	}
	_, body := get(t, srv.URL+"/")
	for _, want := range []string{`class="chip live">Reviewing`, `href="/prs/ethyca/a/1/runs/` + run.ID + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	if _, body := get(t, srv.URL+"/prs/ethyca/a/1/runs/"+run.ID); !strings.Contains(body, a.Name) {
		t.Errorf("run page missing container %q", a.Name)
	}
}

func TestHTTPShowsNoContainerWhenIdle(t *testing.T) {
	srv, svc, _ := newReviewServer(t)
	if r := svc.Status().Reviewing; len(r) != 0 {
		t.Fatalf("reviewing = %+v", r)
	}
	if _, body := get(t, srv.URL+"/"); !strings.Contains(body, "No review running.") {
		t.Error("idle overview should say no review is running")
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
	_, body := get(t, srv.URL+"/")
	if !strings.Contains(body, `data-action="track"`) {
		t.Fatal("dashboard doesn't offer to track the PR again")
	}
	if code := post("track"); code != http.StatusNoContent || len(svc.Status().Ignored) != 0 {
		t.Fatalf("track: %d, ignored %+v", code, svc.Status().Ignored)
	}
}

func TestHTTPStatusStream(t *testing.T) {
	statusEvery = 10 * time.Millisecond
	cfg, tr := testkit.Tracker(t, &github.Fake{}, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: "x"})
	svc := app.NewService(cfg, tr, nil, app.NewLogBuffer(10))
	srv := httptest.NewServer(newHandler(svc))
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
