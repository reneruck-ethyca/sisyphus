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
	"strings"
	"testing"
	"time"

	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/config"
	"github.com/reneruck/waterboy/internal/container"
	"github.com/reneruck/waterboy/internal/github"
	"github.com/reneruck/waterboy/internal/gittest"
	"github.com/reneruck/waterboy/internal/review"
	"github.com/reneruck/waterboy/internal/testkit"
	"github.com/reneruck/waterboy/internal/tracker"
)

func newTestServer(t *testing.T) (*httptest.Server, *app.Service, *app.LogBuffer) {
	t.Helper()
	cfg, tr := testkit.Tracker(t, &github.Fake{},
		&tracker.Tracked{PRRef: testkit.RefA, Title: "Fix <things>", URL: "https://github.com/ethyca/a/pull/1", HeadSHA: "x"})
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

func TestHTTPPages(t *testing.T) {
	srv, _, _ := newTestServer(t)
	for path, want := range map[string][]string{
		"/":               {`href="/prs/ethyca/a/1"`, "Fix &lt;things&gt;", "1 tracked", "Last run never"},
		"/prs/ethyca/a/1": {"ethyca/a#1", "Fix &lt;things&gt;", "https://github.com/ethyca/a/pull/1", `class="health ok"`},
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
	t.Helper()
	_, clone := gittest.Setup(t)
	head := gittest.Git(t, clone, "rev-parse", "HEAD")
	gh := &github.Fake{Cands: []github.PRRef{testkit.RefA}, PRs: map[string]github.PR{
		testkit.RefA.Key(): {PRRef: testkit.RefA, State: "open", HeadSHA: head, RequestedReviewers: []string{"me"}}}}
	cfg, tr := testkit.Tracker(t, gh, &tracker.Tracked{PRRef: testkit.RefA, HeadSHA: head, SyncedSHA: head, Worktree: clone})
	rt := &container.Fake{}
	rev, err := review.New(config.Review{Image: "img", Command: []string{"sh"}, Timeout: config.Duration{Duration: time.Minute}, MaxConcurrent: 1},
		rt, tr.Locks, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	svc := app.NewService(cfg, tr, rev, app.NewLogBuffer(10))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go rev.Run(ctx)
	svc.RunOnce(ctx)
	var run review.Run
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("review never finished")
		}
		if cs, _ := rt.List(ctx, nil); len(cs) == 1 && cs[0].Running {
			rt.Finish(cs[0].ID, 0, "review <log>\n")
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
		"/prs/ethyca/a/1": `href="/prs/ethyca/a/1/runs/` + run.ID + `/log"`,
		"/":               `class="health succeeded"`,
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
