package main

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/review"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

var styleCSS, _ = webFS.ReadFile("web/style.css")

// openInBrowser opens a URL with the system handler; a var so tests don't.
var openInBrowser = func(url string) error { return exec.Command("open", url).Run() }

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"when": func(t time.Time) string {
		if t.IsZero() {
			return "never"
		}
		return t.Local().Format("2006-01-02 15:04:05")
	},
	"stamp": func(t time.Time) string { return t.Local().Format("2 Jan, 15:04") },
	// ago is a <time> the page's script keeps current ("2 min ago"); the exact time is its tooltip.
	"ago": func(t time.Time) template.HTML {
		if t.IsZero() {
			return "never"
		}
		return template.HTML(fmt.Sprintf(`<time data-rel datetime="%s" title="%s">%s</time>`,
			t.UTC().Format(time.RFC3339), t.Local().Format("2 Jan 2006, 15:04:05"), rel(t)))
	},
	// runPath links a review run's page.
	"runPath": func(prID, run string) string {
		repo, n, _ := strings.Cut(prID, "#")
		return "/prs/" + repo + "/" + n + "/runs/" + run
	},
	"outcome": func(r review.Run) string { l, _ := app.Outcome(r); return l },
	"tone":    func(r review.Run) string { _, t := app.Outcome(r); return t },
	"reason":  reasonLabel,
	"dur":     runDuration,
	"short":   func(sha string) string { return sha[:min(len(sha), 8)] },
	"pct":     func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"warnAt":  func(share float64) float64 { return share * 0.75 }, // the budget meter turns red past this
	"base":    path.Base,
	"top":     func(page string) topbar { return topbar{Page: page} },
	"md":      inlineMarkdown,
	// checked is a bool setting's checkbox state; Invert fields show the other way round.
	"checked": func(f config.Field) bool { v, _ := f.Value.(bool); return v != f.Invert },
	"outputs": func(evs []review.Event) (n int) {
		for _, e := range evs {
			if e.Kind == "output" {
				n++
			}
		}
		return n
	},
}).ParseFS(webFS, "web/*.html"))

var (
	mdCode = regexp.MustCompile("`([^`\n]+)`")
	mdBold = regexp.MustCompile(`\*\*([^*\n]+)\*\*`)
)

// inlineMarkdown escapes a review's text, then renders its `code` and **bold**;
// the rest of the markdown stays as written (the page keeps its line breaks).
func inlineMarkdown(s string) template.HTML {
	h := mdCode.ReplaceAllString(template.HTMLEscapeString(s), "<code>$1</code>")
	return template.HTML(mdBold.ReplaceAllString(h, "<b>$1</b>"))
}

// rel says how long ago (or from now) t is, like the pages' rel() script does.
func rel(t time.Time) string {
	d := time.Since(t)
	a, v := d.Abs(), ""
	switch {
	case a < time.Minute:
		if d < 0 {
			return "in a moment"
		}
		return "just now"
	case a < time.Hour:
		v = fmt.Sprintf("%d min", int(a.Minutes()+.5))
	case a < 24*time.Hour:
		v = fmt.Sprintf("%d h", int(a.Hours()+.5))
	default:
		v = fmt.Sprintf("%d d", int(a.Hours()/24+.5))
	}
	if d < 0 {
		return "in " + v
	}
	return v + " ago"
}

var reasons = map[string]string{"new": "PR first seen", "commits": "new commits", "replies": "new replies", "manual": "run by you"}

func reasonLabel(r string) string { return cmp.Or(reasons[r], r) }

// runDuration is how long a run took, or has been running.
func runDuration(r review.Run) string {
	end := r.Finished
	if r.Status == review.Running || end.IsZero() {
		end = time.Now()
	}
	d := end.Sub(r.Started).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min %d s", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

// startHTTP serves the dashboard and JSON API on addr, or on a free loopback
// port when addr is taken: the window needs it. Only app.Service is used, so
// the handlers see the same snapshots as the tray.
func startHTTP(addr string, svc *app.Service) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("dashboard address in use, picking a free port", "addr", addr, "err", err)
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return "", err
		}
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
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(styleCSS)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.Status())
	})
	mux.HandleFunc("GET /api/status/stream", func(w http.ResponseWriter, r *http.Request) {
		streamStatus(w, r, svc)
	})
	// The window can't navigate away from the dashboard, so its pages send
	// external links here; only GitHub, where everything sisyphus links to lives.
	mux.HandleFunc("POST /api/open", func(w http.ResponseWriter, r *http.Request) {
		u := r.FormValue("url")
		if !strings.HasPrefix(u, "https://github.com/") {
			http.Error(w, "only https://github.com/ links are opened", http.StatusForbidden)
			return
		}
		if err := openInBrowser(u); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/prs/{owner}/{repo}/{number}/runs/{run}/open", func(w http.ResponseWriter, r *http.Request) {
		dir, ok := svc.RunDir(prID(r), r.PathValue("run"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		if err := openInBrowser(dir); err != nil { // Finder, for a directory
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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
		d := dashboard{Status: svc.Status(), PR: &pr, Run: run, Events: evs, Next: next}
		if d.ReviewFile, err = svc.ReviewFile(pr.ID, run.ID); err != nil {
			slog.Warn("review file", "pr", pr.ID, "run", run.ID, "err", err)
		}
		render(w, "index.html", d)
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
		render(w, "logs.html", logsPage{Top: topbar{Page: "logs"}, Entries: svc.Logs()})
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
		if last := pr.Review.Latest(); last != nil {
			var err error
			if d.ReviewFile, err = svc.ReviewFile(pr.ID, last.ID); err != nil {
				slog.Warn("review file", "pr", pr.ID, "run", last.ID, "err", err)
			}
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

	ReviewFile *review.ReviewFile // what the shown (or latest) run's agent wrote; nil = nothing
}

// topbar is the header: the dashboard shows Status in it, other pages don't.
type topbar struct {
	Page   string
	Status *app.Status
}

func (d dashboard) Top() topbar { return topbar{Status: &d.Status} }

type prGroup struct {
	Label string
	PRs   []app.PRView
}

var groupLabels = []struct{ key, label string }{
	{app.GroupAttention, "Needs you"}, {app.GroupReviewing, "Reviewing"}, {app.GroupReviewed, "Reviewed"}, {app.GroupWaiting, "Not reviewed yet"},
}

// Groups is the PR list by what the user has to do, empty groups left out.
func (d dashboard) Groups() []prGroup {
	var out []prGroup
	for _, g := range groupLabels {
		if prs := d.InGroup(g.key); len(prs) > 0 {
			out = append(out, prGroup{g.label, prs})
		}
	}
	return out
}

func (d dashboard) InGroup(group string) []app.PRView {
	var out []app.PRView
	for _, pr := range d.PRs {
		if pr.Group == group {
			out = append(out, pr)
		}
	}
	return out
}

type logsPage struct {
	Top     topbar
	Entries []app.LogEntry
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

func (settingsPage) Top() topbar { return topbar{Page: "settings"} }

type fieldGroup struct {
	Name   string
	Fields []config.Field
}

// Groups is the fields by section, in config.Groups order.
func (s settingsPage) Groups() []fieldGroup {
	var out []fieldGroup
	for _, g := range config.Groups {
		fg := fieldGroup{Name: g}
		for _, f := range s.Fields {
			if f.Group == g {
				fg.Fields = append(fg.Fields, f)
			}
		}
		out = append(out, fg)
	}
	return out
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

// streamStatus sends the status as JSON now and after every change, at most
// every statusEvery per client, as server-sent events. Pages reload when it differs.
func streamStatus(w http.ResponseWriter, r *http.Request, svc *app.Service) {
	changed, cancel := svc.SubscribeStatus()
	defer cancel()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	rc := http.NewResponseController(w)
	send := func() bool {
		b, _ := json.Marshal(svc.Status())
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	for send() {
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
		select { // coalesce a burst of changes (a run's log lines) into one send
		case <-time.After(statusEvery):
		case <-r.Context().Done():
			return
		}
	}
}

var statusEvery = 2 * time.Second

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
