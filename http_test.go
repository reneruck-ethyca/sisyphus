package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*httptest.Server, *Service) {
	t.Helper()
	tr := newTestTracker(t, &fakeGitHub{},
		&Tracked{PRRef: refA, Title: "Fix <things>", URL: "https://github.com/ethyca/a/pull/1", HeadSHA: "x", PendingUpdates: true})
	svc := NewService(tr.cfg, tr, NewLogBuffer(10))
	srv := httptest.NewServer(newHandler(svc))
	t.Cleanup(srv.Close)
	return srv, svc
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
	srv, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/api/status")
	var st Status
	if err := json.Unmarshal([]byte(body), &st); code != 200 || err != nil {
		t.Fatalf("%d %v %s", code, err, body)
	}
	if st.User != "me" || len(st.PRs) != 1 || st.PRs[0].ID != refA.Key() || st.PRs[0].Health != "pending" {
		t.Fatalf("status = %+v", st)
	}
}

func TestHTTPPR(t *testing.T) {
	srv, _ := newTestServer(t)
	code, body := get(t, srv.URL+"/api/prs/ethyca/a/1")
	var pr PRView
	if err := json.Unmarshal([]byte(body), &pr); code != 200 || err != nil || pr.ID != refA.Key() {
		t.Fatalf("%d %v %s", code, err, body)
	}
	for _, path := range []string{"/api/prs/ethyca/a/2", "/prs/ethyca/a/2"} {
		if code, _ := get(t, srv.URL+path); code != 404 {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
}

func TestHTTPRun(t *testing.T) {
	srv, svc := newTestServer(t)
	resp, err := http.Post(srv.URL+"/api/run", "", nil)
	if err != nil || resp.StatusCode != http.StatusAccepted {
		t.Fatalf("%v %v", resp, err)
	}
	if len(svc.runNow) != 1 {
		t.Fatal("tick not queued")
	}
}

func TestHTTPPages(t *testing.T) {
	srv, _ := newTestServer(t)
	for path, want := range map[string][]string{
		"/":               {`href="/prs/ethyca/a/1"`, "Fix &lt;things&gt;", "1 tracked · 1 pending", "Last run never"},
		"/prs/ethyca/a/1": {"ethyca/a#1", "Fix &lt;things&gt;", "https://github.com/ethyca/a/pull/1", `class="health pending"`},
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
	srv, svc := newTestServer(t)
	req, _ := http.NewRequest("GET", srv.URL+"/api/status", nil)
	req.Host = "evil.example:7465" // DNS rebinding
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign host: %v %v", resp, err)
	}
	req, _ = http.NewRequest("POST", srv.URL+"/api/run", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusForbidden || len(svc.runNow) != 0 {
		t.Fatalf("cross-origin run: %v %v", resp, err)
	}
}

func TestHTTPLogs(t *testing.T) {
	srv, svc := newTestServer(t)
	log := slog.New(svc.logs.Handler(slog.NewTextHandler(io.Discard, nil)))
	log.Warn("fetching <PR>", "pr", refA.Key())

	code, body := get(t, srv.URL+"/api/logs")
	var entries []LogEntry
	if err := json.Unmarshal([]byte(body), &entries); code != 200 || err != nil || len(entries) != 1 || entries[0].Msg != "fetching <PR>" {
		t.Fatalf("%d %v %s", code, err, body)
	}
	code, body = get(t, srv.URL+"/logs")
	if code != 200 || !strings.Contains(body, "fetching &lt;PR&gt;") || strings.Contains(body, `http-equiv="refresh"`) {
		t.Fatalf("logs page %d: %s", code, body)
	}
}

func TestHTTPLogStream(t *testing.T) {
	srv, svc := newTestServer(t)
	log := slog.New(svc.logs.Handler(slog.NewTextHandler(io.Discard, nil)))
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
	next := func() LogEntry {
		for sc.Scan() {
			if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				var e LogEntry
				if err := json.Unmarshal([]byte(data), &e); err != nil {
					t.Fatal(err)
				}
				return e
			}
		}
		t.Fatalf("stream ended: %v", sc.Err())
		return LogEntry{}
	}
	if e := next(); e.Msg != "backlog" {
		t.Fatalf("first event %+v", e)
	}
	log.Info("live")
	if e := next(); e.Msg != "live" || e.Seq != 2 {
		t.Fatalf("second event %+v", e)
	}
}
