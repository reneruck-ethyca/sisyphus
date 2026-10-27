package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/waterboy/internal/app"
	"github.com/reneruck/waterboy/internal/review"
)

//go:embed web/*.html
var webFS embed.FS

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
	// runPath links an active review to its live run page.
	"runPath": func(prID, run string) string {
		repo, n, _ := strings.Cut(prID, "#")
		return "/prs/" + repo + "/" + n + "/runs/" + run
	},
	"since": func(t time.Time) string { return time.Since(t).Round(time.Second).String() },
	// live pages update themselves and Settings holds a form; the rest refresh every 15s.
	"live": func(title string) bool {
		return title == "Logs" || title == "Settings" || strings.HasPrefix(title, "Run ")
	},
}).ParseFS(webFS, "web/*.html"))

// startHTTP serves the dashboard and JSON API on addr. Only app.Service is used, so
// the handlers see the same snapshots as the tray.
func startHTTP(addr string, svc *app.Service) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	go func() {
		if err := http.Serve(ln, newHandler(svc)); err != nil {
			slog.Error("http server stopped", "err", err)
		}
	}()
	return "http://" + ln.Addr().String() + "/", nil
}

func newHandler(svc *app.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Status())
	})
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, pr)
	})
	mux.HandleFunc("POST /api/prs/{owner}/{repo}/{number}/review", func(w http.ResponseWriter, r *http.Request) {
		if err := svc.ReviewNow(prID(r)); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusAccepted)
	})
	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
		svc.RunNow()
		w.WriteHeader(http.StatusAccepted)
	})

	mux.HandleFunc("GET /prs/{owner}/{repo}/{number}/runs/{run}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		run := findRun(pr, r.PathValue("run"))
		if !ok || run == nil {
			http.NotFound(w, r)
			return
		}
		evs, next, err := svc.RunEvents(pr.ID, run.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		render(w, "index.html", dashboard{Status: svc.Status(), PR: &pr, Run: run, Events: evs, Next: next})
	})
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/runs/{run}/events", func(w http.ResponseWriter, r *http.Request) {
		streamRun(w, r, svc)
	})
	mux.HandleFunc("GET /prs/{owner}/{repo}/{number}/runs/{run}/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serveRunFile(w, r, svc)
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Logs())
	})
	mux.HandleFunc("GET /api/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		streamLogs(w, r, svc)
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		render(w, "logs.html", svc.Logs())
	})
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		st, err := svc.Settings()
		render(w, "settings.html", settingsPage{st, err})
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		st, err := svc.Settings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var values map[string]any
		if err := json.NewDecoder(r.Body).Decode(&values); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := svc.SaveSettings(values); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		restartSoon()
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "index.html", dashboard{Status: svc.Status()})
	})
	mux.HandleFunc("GET /prs/{owner}/{repo}/{number}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		if !ok {
			http.NotFound(w, r)
			return
		}
		d := dashboard{Status: svc.Status(), PR: &pr}
		if d.Stack, d.StackErr = svc.Stack(r.Context(), pr.ID); d.StackErr != nil {
			slog.Warn("stack status", "pr", pr.ID, "err", d.StackErr)
		}
		render(w, "index.html", d)
	})
	// The PR page's … menu: reset (reviews), wipe (+ stack), untrack (+ tracking); track undoes untrack.
	actions := map[string]func(context.Context, string) error{
		"reset": svc.ResetReviews, "wipe": svc.Wipe, "untrack": svc.Untrack, "track": svc.Retrack}
	mux.HandleFunc("POST /api/prs/{owner}/{repo}/{number}/{action}", func(w http.ResponseWriter, r *http.Request) {
		act, ok := actions[r.PathValue("action")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := act(r.Context(), prID(r)); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	return localOnly(http.NewCrossOriginProtection().Handler(mux))
}

// dashboard is the two-column page: every PR on the left, PR (if any) on the
// right, and one of its review runs (if any) streaming below.
type dashboard struct {
	app.Status
	PR     *app.PRView
	Run    *review.Run
	Events []review.Event
	Next   int // first log line the live stream should send

	Stack    *review.StackStatus // the PR page's local stack; nil = none configured
	StackErr error
}

func (d dashboard) Title() string {
	switch {
	case d.Run != nil:
		return "Run " + d.Run.ID
	case d.PR != nil:
		return d.PR.ID
	}
	return "Status"
}

type settingsPage struct {
	app.Settings
	Err error
}

func findRun(pr app.PRView, id string) *review.Run {
	for i := range pr.Review.Runs {
		if pr.Review.Runs[i].ID == id {
			return &pr.Review.Runs[i]
		}
	}
	return nil
}

// streamRun sends a run's events from line ?from= on as server-sent events: the
// backlog, then live ones. Once the run is over it sends a "done" event and ends.
func streamRun(w http.ResponseWriter, r *http.Request, svc *app.Service) {
	id, runID := prID(r), r.PathValue("run")
	live, cancel := svc.SubscribeRunEvents() // before the backlog, so nothing falls in between
	defer cancel()
	backlog, next, err := svc.RunEvents(id, runID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	send := func(event string, v any) bool {
		b, _ := json.Marshal(v)
		if event != "" {
			fmt.Fprintf(w, "event: %s\n", event)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for _, e := range backlog {
		if e.Line >= from && !send("", e) {
			return
		}
	}
	rc.Flush()
	running := func() bool {
		pr, _ := svc.PR(id)
		run := findRun(pr, runID)
		return run != nil && run.Status == review.Running
	}
	check := time.NewTicker(time.Second)
	defer check.Stop()
	for {
		select {
		case e := <-live:
			if e.PR == id && e.Run == runID && e.Line >= max(next, from) && !send("", e.Event) {
				return
			}
		case <-check.C:
			if !running() {
				// Drain what the run printed last, then tell the page to settle.
				for drained := false; !drained; {
					select {
					case e := <-live:
						if e.PR == id && e.Run == runID && e.Line >= max(next, from) {
							send("", e.Event)
						}
					default:
						drained = true
					}
				}
				send("done", struct{}{})
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// streamLogs sends the backlog, then every new entry, as server-sent events
// until the client goes away.
func streamLogs(w http.ResponseWriter, r *http.Request, svc *app.Service) {
	live, cancel := svc.SubscribeLogs() // before the backlog, so nothing falls in between
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	var last uint64
	send := func(e app.LogEntry) bool {
		if e.Seq <= last {
			return true // already sent with the backlog
		}
		last = e.Seq
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for _, e := range svc.Logs() {
		if !send(e) {
			return
		}
	}
	rc.Flush() // send headers even with an empty backlog
	for {
		select {
		case e := <-live:
			if !send(e) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

// serveRunFile serves a review run's files (log, run.json, out/…). os.Root keeps
// paths and symlinks a reviewer left in out/ from escaping the run directory.
func serveRunFile(w http.ResponseWriter, r *http.Request, svc *app.Service) {
	dir, ok := svc.RunDir(prID(r), r.PathValue("run"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer root.Close()
	name := r.PathValue("path")
	if name == "" {
		name = "."
	}
	// Whatever a reviewer wrote must not run script on the dashboard's origin.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if name == "log" || name == "stack.log" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	}
	http.ServeFileFS(w, r, root.FS(), name)
}

// prID rebuilds the app.Service ID ("owner/repo#n") from the path.
func prID(r *http.Request) string {
	return r.PathValue("owner") + "/" + r.PathValue("repo") + "#" + r.PathValue("number")
}

// localOnly rejects requests whose Host isn't loopback, so a DNS-rebinding page
// can't read PR data. Cross-origin POSTs are already refused by CrossOriginProtection.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

// render buffers so a template error becomes a 500, not half a page.
func render(w http.ResponseWriter, name string, data any) {
	var buf bytes.Buffer
	if err := pages.ExecuteTemplate(&buf, name, data); err != nil {
		slog.Error("render", "page", name, "err", err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	buf.WriteTo(w)
}
