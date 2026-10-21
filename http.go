package main

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/reneruck/waterboy/internal/app"
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
	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
		svc.RunNow()
		w.WriteHeader(http.StatusAccepted)
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
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		render(w, "index.html", svc.Status())
	})
	mux.HandleFunc("GET /prs/{owner}/{repo}/{number}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		if !ok {
			http.NotFound(w, r)
			return
		}
		render(w, "pr.html", pr)
	})
	return localOnly(http.NewCrossOriginProtection().Handler(mux))
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
	if name == "log" {
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
