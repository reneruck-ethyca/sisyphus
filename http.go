package main

import (
	"bytes"
	"cmp"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/app"
	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/lastmile"
	"github.com/reneruck/sisyphus/internal/review"
)

//go:embed web/*.html web/style.css
var webFS embed.FS

var styleCSS, _ = webFS.ReadFile("web/style.css")

// openInBrowser opens a URL with the system handler; a var so tests don't.
var openInBrowser = func(url string) error { return exec.Command("open", url).Run() }

var pages = template.Must(template.New("").Funcs(template.FuncMap{
	"skill": skillsStatus,
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
	"outcome":       func(r review.Run) string { l, _ := app.Outcome(r); return l },
	"tone":          func(r review.Run) string { _, t := app.Outcome(r); return t },
	"reason":        app.ReasonLabel,
	"dur":           app.RunDuration,
	"stateTone":     app.HandoffTone,
	"short":         func(sha string) string { return sha[:min(len(sha), 8)] },
	"pct":           func(f float64) string { return fmt.Sprintf("%.0f%%", f*100) },
	"warnAt":        func(share float64) float64 { return share * 0.75 }, // the budget meter turns red past this
	"base":          path.Base,
	"gh":            githubURL,
	"secs":          humanSeconds,
	"reviewerChips": reviewerChips,
	"join":          strings.Join,
	// dict passes several values to a template: dict "Key" value ...
	"dict": func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	},
	"top": func(page string) topbar { return topbar{Page: page} },
	"md":  inlineMarkdown,
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
	markSkills() // once: the handler is built at startup
	mux := http.NewServeMux()
	mux.HandleFunc("GET /style.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Write(styleCSS)
	})
	mux.HandleFunc("GET /icon.png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write(trayIcon)
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
		if err := svc.SetAgent(prID(r), agent); err != nil {
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
		d := dashboard{Board: app.Board{Status: svc.Status()}, PR: &pr, Run: run, Events: evs, Next: next}
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
	mux.HandleFunc("GET /lastmile", func(w http.ResponseWriter, r *http.Request) {
		st := svc.Status()
		render(w, "lastmile.html", lastmilePage{Top: topbar{Page: "lastmile", Status: &st}, Board: app.Board{Status: st}})
	})
	mux.HandleFunc("GET /lastmile/shadow", withLastMile(svc, func(w http.ResponseWriter, r *http.Request, lm *lastmile.Manager) {
		shadowPage(w, r, svc, lm)
	}))
	mux.HandleFunc("GET /lastmile/{id}", func(w http.ResponseWriter, r *http.Request) {
		st := svc.Status()
		for _, v := range st.Handoffs {
			if v.ID == r.PathValue("id") {
				events := slices.Clone(v.Events)
				slices.Reverse(events) // newest first
				render(w, "handoff.html", handoffPage{Top: topbar{Page: "lastmile", Status: &st}, V: v, Events: events})
				return
			}
		}
		http.Error(w, "no such handoff", http.StatusNotFound)
	})
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		render(w, "logs.html", logsPage{Top: topbar{Page: "logs"}, Entries: svc.Logs()})
	})
	history := func(w http.ResponseWriter, r *http.Request) {
		d := dashboard{Board: app.Board{Status: svc.Status()}, History: svc.History()}
		if r.PathValue("number") != "" {
			i := slices.IndexFunc(d.History, func(h app.HistoryRow) bool { return h.ID == prID(r) })
			if i < 0 {
				http.NotFound(w, r)
				return
			}
			d.Hist = &d.History[i]
			pr := d.Hist.PRView()
			d.PR = &pr
			show := pr.Review.Latest()
			if id := r.PathValue("run"); id != "" {
				if d.Run = findRun(pr, id); d.Run == nil {
					http.NotFound(w, r)
					return
				}
				var err error
				if d.Events, d.Next, err = svc.RunEvents(pr.ID, id); err != nil {
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				show = d.Run
			}
			if show != nil {
				var err error
				if d.ReviewFile, err = svc.ReviewFile(pr.ID, show.ID); err != nil {
					slog.Warn("review file", "pr", pr.ID, "run", show.ID, "err", err)
				}
			}
		}
		render(w, "index.html", d)
	}
	mux.HandleFunc("GET /history", history)
	mux.HandleFunc("GET /history/{owner}/{repo}/{number}", history)
	mux.HandleFunc("GET /history/{owner}/{repo}/{number}/runs/{run}", history)
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, svc.History())
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
		h := homePage{dashboard: dashboard{Board: app.Board{Status: svc.Status()}}}
		if lm := svc.LastMile(); lm != nil {
			if rep, err := lm.ShadowReport(lastmile.ShadowQuery{}); err == nil { // unreadable: the card is left out
				h.Shadow = &rep
			}
		}
		render(w, "home.html", h)
	})
	mux.HandleFunc("GET /reviews", func(w http.ResponseWriter, r *http.Request) {
		render(w, "index.html", dashboard{Board: app.Board{Status: svc.Status()}})
	})
	mux.HandleFunc("GET /prs/{owner}/{repo}/{number}", func(w http.ResponseWriter, r *http.Request) {
		pr, ok := svc.PR(prID(r))
		if !ok {
			http.NotFound(w, r)
			return
		}
		d := dashboard{Board: app.Board{Status: svc.Status()}, PR: &pr}
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
	app.Board
	PR     *app.PRView
	Run    *review.Run
	Events []review.Event
	Next   int // first log line the live stream should send

	Stack    *review.StackStatus // the PR page's local stack; nil = none configured
	StackErr error

	ReviewFile *review.ReviewFile // what the shown (or latest) run's agent wrote; nil = nothing

	History []app.HistoryRow // non-nil on the history pages, which list it instead of the tracked PRs
	Hist    *app.HistoryRow  // the history entry shown, if any; PR is its view
}

func (d dashboard) Archive() bool { return d.History != nil }

// topbar is the header: the dashboard shows Status in it, other pages don't.
type topbar struct {
	Page   string
	Status *app.Status
}

// NeedsHuman feeds the Last mile tab's badge.
func (t topbar) NeedsHuman() []lastmile.View {
	if t.Status == nil {
		return nil
	}
	return app.Board{Status: *t.Status}.NeedsHuman()
}

func (d dashboard) Top() topbar {
	if d.Archive() {
		return topbar{Status: &d.Status, Page: "history"}
	}
	return topbar{Status: &d.Status, Page: "reviews"}
}

// homePage is the landing dashboard: what needs you, then reviews, the last mile, capacity and the shadow report.
type homePage struct {
	dashboard
	Shadow *lastmile.ShadowReport // nil = the last mile is off or its log unreadable
}

// Top has no tab current: the brand is the way here.
func (h homePage) Top() topbar { return topbar{Status: &h.Status} }

func (h homePage) Title() string { return "Home" }

type lastmilePage struct {
	Top topbar
	app.Board
}

// handoffPage is one handoff's page; Events are newest first.
type handoffPage struct {
	Top    topbar
	V      lastmile.View
	Events []lastmile.Event
}

// shadowData is the shadow merge report page. Focus is the report narrowed to the ?handoff= or ?pr= PR.
type shadowData struct {
	Top        topbar
	Report     lastmile.ShadowReport
	MeanSec    int64
	Focus      *lastmile.ShadowReport
	FocusID    string // the handoff the narrowed report is about, the page's #anchor
	FocusTitle string
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
	case err != nil:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	default:
		writeJSON(w, status, v)
	}
}

// shadowPage renders the report; with ?handoff=id or ?pr=owner/repo%23n it adds that PR's verdict history.
func shadowPage(w http.ResponseWriter, r *http.Request, svc *app.Service, lm *lastmile.Manager) {
	rep, err := lm.ShadowReport(lastmile.ShadowQuery{})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	st := svc.Status()
	d := shadowData{Top: topbar{Page: "lastmile", Status: &st}, Report: rep, MeanSec: int64(rep.MeanTimeInReviewSec)}
	q := lastmile.ShadowQuery{PR: r.URL.Query().Get("pr"), Handoff: r.URL.Query().Get("handoff")}
	if q != (lastmile.ShadowQuery{}) {
		f, err := lm.ShadowReport(q)
		var invalid lastmile.InvalidError
		switch {
		case errors.As(err, &invalid):
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		case err != nil:
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		d.Focus, d.FocusTitle = &f, cmp.Or(q.PR, q.Handoff)
		if len(f.PRs) > 0 {
			d.FocusID = f.PRs[0].HandoffID
			d.FocusTitle = fmt.Sprintf("%s #%d", f.PRs[0].Repo, f.PRs[0].PR)
		}
	}
	render(w, "shadow.html", d)
}

// githubURL passes a link to a GitHub page and drops any other: PR URLs come from GitHub, but the page doesn't trust them.
func githubURL(u string) string {
	if strings.HasPrefix(u, "https://github.com/") {
		return u
	}
	return ""
}

// humanSeconds says a duration in its largest two-ish units, "5 h", "2 d".
func humanSeconds(s int64) string {
	d := time.Duration(s) * time.Second
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", s)
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h", d.Hours())
	}
	return fmt.Sprintf("%.1f d", d.Hours()/24)
}

type reviewerChip struct{ Login, Class string }

// reviewerChips tells who is asked to review: those TLM requested plain, ones a human added "human",
// and those a human removed "gone" (TLM won't re-add them).
func reviewerChips(v lastmile.View) []reviewerChip {
	var out []reviewerChip
	for _, l := range v.Reviewing {
		c := reviewerChip{Login: l}
		if !slices.Contains(v.Requested, l) && v.Snapshot != nil {
			c.Class = "human"
		}
		out = append(out, c)
	}
	for _, l := range v.Removed {
		if !slices.Contains(v.Reviewing, l) {
			out = append(out, reviewerChip{Login: l, Class: "gone"})
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
	case d.Archive():
		return "History"
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
	from, _ := strconv.Atoi(r.URL.Query().Get("from"))
	events, err := svc.FollowRun(r.Context(), prID(r), r.PathValue("run"), from)
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
	for sse.send("", svc.Status()) {
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
	sse := newSSE(w)
	var last uint64
	send := func(e app.LogEntry) bool {
		if e.Seq <= last {
			return true // already sent with the backlog
		}
		last = e.Seq
		return sse.send("", e)
	}
	for _, e := range svc.Logs() {
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

// MeanSec is the shadow report's mean time in review, for the secs helper.
func (h homePage) MeanSec() int64 { return int64(h.Shadow.MeanTimeInReviewSec) }
