package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
	"github.com/reneruck/sisyphus/internal/tracker"
)

// ui is the built frontend (web/ at the repo root, built into web/dist). The
// placeholder keeps the backend building and testing without it.
//
//go:embed all:web/dist
var uiFS embed.FS

// openInBrowser opens a URL with the system handler; a var so tests don't.
var openInBrowser = func(url string) error { return exec.Command("open", url).Run() }

// server is what the handlers use: svc for status and actions that need both
// tracker and reviewer, the rest directly.
type server struct {
	svc        *app.Service
	tracker    *tracker.Tracker
	rev        *review.Reviewer // nil when reviews are disabled
	logs       *app.LogBuffer
	configPath string
	token      string // every /api call needs it: as a Bearer token, or the session cookie made from it
	codes      *loginCodes
}

// loadAPIToken is the API token in the state dir; a var for tests.
var loadAPIToken = lastmile.LoadToken

func newServer(cfg config.Config, tr *tracker.Tracker, rev *review.Reviewer, logs *app.LogBuffer) server {
	tok, err := loadAPIToken(filepath.Dir(cfg.StateFile))
	if err != nil { // no token: every API call is refused, which the dashboard says
		slog.Error("API token", "err", err)
	}
	return server{svc: app.NewService(cfg, tr, rev), tracker: tr, rev: rev, logs: logs, configPath: cfg.Path, token: tok, codes: &loginCodes{}}
}

// dashboardURL is base with a one-time code the page trades for the session cookie (GET /api/session).
// Not the token: a URL lands in the browser's history, which may sync.
func (s server) dashboardURL(base string) string { return base + "?code=" + s.codes.mint() }

// loginCodes are single-use: the window and each Open dashboard get their own.
type loginCodes struct {
	mu    sync.Mutex
	codes []string // ponytail: newest last, capped; a code nobody used is forgotten after maxCodes newer ones
}

const maxCodes = 16

func (c *loginCodes) mint() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	code := hex.EncodeToString(b)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codes = append(c.codes, code)[max(0, len(c.codes)+1-maxCodes):]
	return code
}

// use says whether code is one minted and not used yet, and uses it up.
func (c *loginCodes) use(code string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, x := range c.codes {
		if subtle.ConstantTimeCompare([]byte(x), []byte(code)) == 1 {
			c.codes = slices.Delete(c.codes, i, i+1)
			return true
		}
	}
	return false
}

func (s server) runDir(prID, runID string) (string, bool) {
	if s.rev == nil {
		return "", false
	}
	return s.rev.RunDir(prID, runID)
}

// runEvents is a run's readable output so far, and the number of the next line.
func (s server) runEvents(prID, runID string) ([]review.Event, int, error) {
	if s.rev == nil {
		return nil, 0, review.ErrNoRun
	}
	return s.rev.Events(prID, runID)
}

// reviewFile is what a run's agent wrote to out/review.json; nil if it wrote none.
func (s server) reviewFile(prID, runID string) (*review.ReviewFile, error) {
	dir, ok := s.runDir(prID, runID)
	if !ok {
		return nil, fmt.Errorf("no run %s for %s", runID, prID)
	}
	return review.ReadReviewFile(dir)
}

func (s server) stack(ctx context.Context, pr app.PRView) (*review.StackStatus, error) {
	if s.rev == nil {
		return nil, nil // no reviews, no stack
	}
	return s.rev.StackStatus(ctx, pr.Tracked)
}

// settings is the config file's editable fields, read now so it shows saved changes not yet applied.
type settings struct {
	Path   string         `json:"path"`
	Groups []string       `json:"groups"` // the sections fields fall in, in page order
	Fields []config.Field `json:"fields"`
}

func (s server) settings() (settings, error) {
	fs, err := config.Settings(s.configPath)
	return settings{Path: s.configPath, Groups: config.Groups, Fields: fs}, err
}

// startHTTP serves the dashboard and JSON API on addr, or on a free loopback
// port when addr is taken: the window needs it. Status comes from app.Service,
// so the handlers see the same snapshots as the tray.
func startHTTP(addr string, srv server) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Warn("dashboard address in use, picking a free port", "addr", addr, "err", err)
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return "", err
		}
	}
	go func() {
		if err := http.Serve(ln, newHandler(srv)); err != nil {
			slog.Error("http server stopped", "err", err)
		}
	}()
	return "http://" + ln.Addr().String() + "/", nil
}

func newHandler(srv server) http.Handler {
	svc := srv.svc
	markSkills() // once: the handler is built at startup
	mux := http.NewServeMux()
	mux.Handle("GET /", spa(uiDist()))
	// The page trades the token from its URL for an HttpOnly cookie, so the token leaves the address bar.
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		if srv.token == "" || !srv.codes.use(r.URL.Query().Get("code")) {
			http.Error(w, "unknown or used code: open the dashboard from the sisyphus menu again", http.StatusUnauthorized)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: srv.session(), Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, app.Board{Status: svc.Status()}.View())
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
		dir, ok := srv.runDir(prID(r), r.PathValue("run"))
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
	mux.HandleFunc("PUT /api/prs/{owner}/{repo}/{number}/agent", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Inherit bool         `json:"inherit"`
			Agent   config.Agent `json:"agent"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var agent *config.Agent
		if !body.Inherit {
			agent = &body.Agent
		}
		if err := srv.tracker.SetAgent(prID(r), agent); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/handoffs", func(w http.ResponseWriter, r *http.Request) {
		lm := svc.LastMile()
		if lm == nil {
			http.Error(w, errLastMileDisabled, http.StatusServiceUnavailable)
			return
		}
		if !lm.Authorized(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")) {
			http.Error(w, "missing or wrong token: send lastmile.token from sisyphus's state dir as a Bearer token (tlm does)", http.StatusUnauthorized)
			return
		}
		var req lastmile.Request
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		h, err := lm.Handoff(r.Context(), req)
		writeLastMile(w, r, http.StatusCreated, h, err)
	})
	mux.HandleFunc("GET /api/handoffs", func(w http.ResponseWriter, r *http.Request) {
		hs := []lastmile.Handoff{}
		if lm := svc.LastMile(); lm != nil {
			hs = lm.List()
		}
		writeJSON(w, http.StatusOK, hs)
	})
	mux.HandleFunc("GET /api/handoffs/{id}", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		h, ok := lm.Get(r.PathValue("id"))
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusOK, h)
	}))
	mux.HandleFunc("PUT /api/handoffs/{id}/reviewers", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		// The body is the desired human reviewers: a JSON list, or {"reviewers": [...]}.
		var raw json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var logins []string
		if err := json.Unmarshal(raw, &logins); err != nil {
			var obj struct {
				Reviewers []string `json:"reviewers"`
			}
			if err := json.Unmarshal(raw, &obj); err != nil {
				http.Error(w, "body must be a list of logins or {\"reviewers\": [...]}", http.StatusBadRequest)
				return
			}
			logins = obj.Reviewers
		}
		h, err := lm.SetReviewers(r.PathValue("id"), logins)
		writeLastMile(w, r, http.StatusOK, h, err)
	}))
	mux.HandleFunc("GET /api/lastmile/shadow", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		rep, err := lm.ShadowReport(lastmile.ShadowQuery{PR: r.URL.Query().Get("pr"), Handoff: r.URL.Query().Get("handoff")})
		writeLastMile(w, r, http.StatusOK, rep, err)
	}))
	// Installs (or updates) or uninstalls the handoff skill for one agent, in the
	// background; GET /api/skill reports how it went. It only writes or removes
	// the embedded skill in the agent's skill dir, so like the buttons below it
	// needs no token.
	mux.HandleFunc("POST /api/skill/{agent}/{action}", func(w http.ResponseWriter, r *http.Request) {
		if err := startSkillAction(r.PathValue("agent"), r.PathValue("action")); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, http.StatusAccepted, skillsStatus())
	})
	mux.HandleFunc("GET /api/skill", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, skillsStatus())
	})
	// The dashboard's buttons call these, so no token (as with the reviewers edit); the API is loopback.
	for _, verb := range []string{"resume", "cancel"} {
		mux.HandleFunc("POST /api/handoffs/{id}/"+verb, withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
			var h lastmile.Handoff
			var err error
			if verb == "resume" {
				h, err = lm.Resume(r.Context(), r.PathValue("id"))
			} else {
				h, err = lm.Cancel(r.PathValue("id"))
			}
			writeLastMile(w, r, http.StatusOK, h, err)
		}))
	}
	// The dirty-worktree panel's actions; hash is the Dirty.Hash the page was rendered for (409 if it moved on).
	mux.HandleFunc("POST /api/handoffs/{id}/dirty/{action}", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		var body struct {
			Hash    string   `json:"hash"`
			Paths   []string `json:"paths"`
			Message string   `json:"message"`
			Task    string   `json:"task"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		id := r.PathValue("id")
		var h lastmile.Handoff
		var err error
		switch r.PathValue("action") {
		case "commit":
			h, err = lm.DirtyCommit(r.Context(), id, body.Hash, body.Paths, body.Message)
		case "stash":
			h, err = lm.DirtyStash(r.Context(), id, body.Hash)
		case "agent":
			h, err = lm.DirtyAgent(r.Context(), id, body.Hash, body.Message, body.Task)
		case "leave":
			h, err = lm.DirtyLeave(r.Context(), id, body.Hash)
		default:
			http.NotFound(w, r)
			return
		}
		writeLastMile(w, r, http.StatusOK, h, err)
	}))
	mux.HandleFunc("GET /api/handoffs/{id}/dirty/diff", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		diff, err := lm.DirtyDiff(r.Context(), r.PathValue("id"), r.URL.Query().Get("path"))
		if err != nil {
			writeLastMile(w, r, http.StatusOK, nil, err)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff") // it's the worktree's content, maybe HTML
		_, _ = io.WriteString(w, diff)
	}))
	mux.HandleFunc("POST /api/run", func(w http.ResponseWriter, r *http.Request) {
		svc.RunNow()
		w.WriteHeader(http.StatusAccepted)
	})

	// A run's output so far, and Next, the line its live stream (…/events?from=Next) carries on from.
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/runs/{run}/log", func(w http.ResponseWriter, r *http.Request) {
		evs, next, err := srv.runEvents(prID(r), r.PathValue("run"))
		if errors.Is(err, review.ErrNoRun) {
			http.NotFound(w, r)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if evs == nil {
			evs = []review.Event{}
		}
		writeJSON(w, http.StatusOK, runLog{Events: evs, Next: next})
	})
	// What the run's agent wrote to out/review.json; null when it wrote none.
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/runs/{run}/review", func(w http.ResponseWriter, r *http.Request) {
		if _, ok := srv.runDir(prID(r), r.PathValue("run")); !ok {
			http.NotFound(w, r)
			return
		}
		rf, err := srv.reviewFile(prID(r), r.PathValue("run"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, rf)
	})
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/runs/{run}/events", func(w http.ResponseWriter, r *http.Request) {
		streamRun(w, r, srv)
	})
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/runs/{run}/files/{path...}", func(w http.ResponseWriter, r *http.Request) {
		serveRunFile(w, r, srv)
	})
	// The PR's local review stack; null when none is configured for its repo.
	mux.HandleFunc("GET /api/prs/{owner}/{repo}/{number}/stack", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		if !ok {
			http.NotFound(w, r)
			return
		}
		st, err := srv.stack(r.Context(), pr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		writeJSON(w, http.StatusOK, st)
	})
	mux.HandleFunc("GET /api/logs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, srv.logs.Recent())
	})
	mux.HandleFunc("GET /api/logs/stream", func(w http.ResponseWriter, r *http.Request) {
		streamLogs(w, r, srv.logs)
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		h := svc.History()
		if h == nil {
			h = []app.HistoryRow{}
		}
		writeJSON(w, http.StatusOK, h)
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		st, err := srv.settings()
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
		if err := config.SaveSettings(srv.configPath, values); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		slog.Info("settings saved, restart to apply", "file", srv.configPath, "changed", slices.Sorted(maps.Keys(values)))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /api/restart", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		restartSoon()
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
	return localOnly(http.NewCrossOriginProtection().Handler(srv.requireToken(mux)))
}

const errLastMileDisabled = "the last mile is disabled (lastmile.disabled)"

// withLastMile serves h with the last mile, or 404 when it is disabled.
func withLastMile(svc *app.Service, h func(http.ResponseWriter, *http.Request, *lastmile.Manager)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		lm := svc.LastMile()
		if lm == nil {
			http.Error(w, errLastMileDisabled, http.StatusNotFound)
			return
		}
		h(w, r, lm)
	}
}

// writeLastMile answers with v as JSON, or with the status a last-mile error calls for.
func writeLastMile(w http.ResponseWriter, r *http.Request, status int, v any, err error) {
	var invalid lastmile.InvalidError
	switch {
	case errors.Is(err, lastmile.ErrNotFound):
		http.NotFound(w, r)
	case errors.As(err, &invalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, lastmile.ErrStale):
		http.Error(w, err.Error(), http.StatusConflict)
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, status, v)
	}
}

// streamRun sends a run's events from line ?from= on as server-sent events: the
// backlog, then live ones. Once the run is over it sends a "done" event and ends.
func streamRun(w http.ResponseWriter, r *http.Request, srv server) {
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	if srv.rev == nil {
		http.NotFound(w, r)
		return
	}
	events, err := srv.rev.Follow(r.Context(), prID(r), r.PathValue("run"), from)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	sse := newSSE(w)
	for e := range events {
		if !sse.send("", e) {
			return
		}
	}
	sse.send("done", struct{}{})
}

// sse writes server-sent events. newSSE sends the headers at once, so a client
// sees the stream open before the first event.
type sse struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func newSSE(w http.ResponseWriter) sse {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	s := sse{w, http.NewResponseController(w)}
	s.rc.Flush()
	return s
}

// send writes v as JSON, as the named event if event isn't "", and reports
// whether the client is still there.
func (s sse) send(event string, v any) bool {
	b, _ := json.Marshal(v)
	if event != "" {
		fmt.Fprintf(s.w, "event: %s\n", event)
	}
	if _, err := fmt.Fprintf(s.w, "data: %s\n\n", b); err != nil {
		return false
	}
	return s.rc.Flush() == nil
}

// streamStatus sends the status as JSON now and after every change, at most
// every statusEvery per client, as server-sent events. Pages reload when it differs.
func streamStatus(w http.ResponseWriter, r *http.Request, svc *app.Service) {
	changed, cancel := svc.SubscribeStatus()
	defer cancel()
	sse := newSSE(w)
	for sse.send("", app.Board{Status: svc.Status()}.View()) {
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
func streamLogs(w http.ResponseWriter, r *http.Request, logs *app.LogBuffer) {
	live, cancel := logs.Subscribe() // before the backlog, so nothing falls in between
	defer cancel()
	sse := newSSE(w)
	var last uint64
	send := func(e app.LogEntry) bool {
		if e.Seq <= last {
			return true // already sent with the backlog
		}
		last = e.Seq
		return sse.send("", e)
	}
	for _, e := range logs.Recent() {
		if !send(e) {
			return
		}
	}
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
func serveRunFile(w http.ResponseWriter, r *http.Request, srv server) {
	dir, ok := srv.runDir(prID(r), r.PathValue("run"))
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

// runLog is a run's readable output so far; Next is where its live stream carries on.
type runLog struct {
	Events []review.Event `json:"events"`
	Next   int            `json:"next"`
}

func uiDist() fs.FS {
	dist, _ := fs.Sub(uiFS, "web/dist")
	return dist
}

// spa serves the built UI: its files as they are, and index.html for every
// other path, which the UI routes itself. Unknown /api paths stay 404s.
func spa(dist fs.FS) http.Handler {
	files := http.FileServerFS(dist)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		if st, err := fs.Stat(dist, strings.TrimPrefix(path.Clean(r.URL.Path), "/")); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		index, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, "the UI isn't built: run `make build` at the repo root", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(index)
	})
}

const sessionCookie = "sisyphus_session"

func (s server) authorized(tok string) bool {
	return s.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) == 1
}

// session is the cookie's value: derived from the token, so it outlives a restart,
// but it isn't the token tlm and the handoff API take.
func (s server) session() string {
	m := hmac.New(sha256.New, []byte(s.token))
	m.Write([]byte("sisyphus dashboard session"))
	return hex.EncodeToString(m.Sum(nil))
}

// requireToken refuses /api calls without the token. Loopback isn't enough: a review
// container reaches the port too (host.docker.internal), with any Host header it likes,
// and an agent there must not change settings, start agents or push.
func (s server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") && r.URL.Path != "/api/session" {
			tok, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			ok := bearer && s.authorized(tok)
			if c, err := r.Cookie(sessionCookie); !bearer && err == nil && s.token != "" {
				ok = subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.session())) == 1
			}
			if !ok {
				http.Error(w, "unauthorized: open the dashboard from the sisyphus menu, or send the API token (tlm does)", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
