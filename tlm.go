package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/lastmile"
)

const tlmUsage = `usage: tlm handoff --base <branch> [--worktree dir] [--title t] [--body-file f] [--jira KEY] [--labels a,b]
                   [--reviewers a,b] [--draft] [--autonomy fix-ci,address-reviews,merge=off|shadow]
                   [--notify 'cmd template'] [--notify-on merged,needs-human,closed] [--agent vendor:model:effort]
                   [--max-fix-rounds n] [--stack-parent id|branch] [--requester who]
       tlm status <id> [--json]
       tlm wait <id> [--until merged|needs-human|closed|<state>[,…]]
       tlm resume <id>   take a needs-human handoff back
       tlm cancel <id>   stop managing a handoff (GitHub is left as it is)`

// waitPoll is how often `tlm wait` asks; a var for tests.
var waitPoll = 15 * time.Second

// runTLM is the `tlm` command: a thin client of the running app's API. It
// returns the exit status.
func runTLM(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, tlmUsage)
		return 2
	}
	var err error
	switch args[0] {
	case "handoff":
		err = tlmHandoff(args[1:], stdout, stderr)
	case "status":
		err = tlmStatus(args[1:], stdout, stderr)
	case "wait":
		err = tlmWait(args[1:], stdout, stderr)
	case "resume", "cancel":
		err = tlmAct(args[0], args[1:], stdout, stderr)
	default:
		_, _ = fmt.Fprintln(stderr, tlmUsage)
		return 2
	}
	switch {
	case errors.Is(err, flag.ErrHelp):
		return 0
	case err != nil:
		_, _ = fmt.Fprintln(stderr, "tlm:", err)
		return 1
	}
	return 0
}

// tlmFlags is a flag set with the --addr every subcommand takes.
func tlmFlags(name string, stderr io.Writer) (*flag.FlagSet, *string) {
	fs := flag.NewFlagSet("tlm "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	// ponytail: the configured address; if the dashboard fell back to a free port at startup, pass --addr.
	addr := fs.String("addr", "", "sisyphus API address (default: http_addr from the config)")
	return fs, addr
}

// apiToken is the handoff API's token, from the state dir of the config
// (lastmile.TokenFile); "" if it can't be read, and the API says so. A var for tests.
var apiToken = func() string {
	cfg, err := config.Load(config.DefaultPath())
	if err != nil {
		return ""
	}
	b, _ := os.ReadFile(lastmile.TokenFile(filepath.Dir(cfg.StateFile)))
	return strings.TrimSpace(string(b))
}

func apiBase(addr string) string {
	if addr == "" {
		addr = "127.0.0.1:7465" // the default http_addr, if the config can't be read
		if cfg, err := config.Load(config.DefaultPath()); err == nil {
			addr = cfg.HTTPAddr
		}
	}
	return "http://" + addr
}

func commaList(s string) []string {
	var out []string
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func tlmHandoff(args []string, stdout, stderr io.Writer) error {
	fs, addr := tlmFlags("handoff", stderr)
	var (
		worktree    = fs.String("worktree", ".", "worktree to hand over")
		base        = fs.String("base", "", "branch the PR targets")
		stackParent = fs.String("stack-parent", "", "handoff id or branch this PR stacks on")
		title       = fs.String("title", "", "PR title (default: the last commit's subject)")
		bodyFile    = fs.String("body-file", "", "file with the PR body (default: the repo's template)")
		jira        = fs.String("jira", "", "Jira key, put in the title")
		labels      = fs.String("labels", "", "comma-separated PR labels")
		reviewers   = fs.String("reviewers", "", "comma-separated reviewers, instead of the roster")
		draft       = fs.Bool("draft", false, "open the PR as a draft")
		autonomy    = fs.String("autonomy", "", "fix-ci,address-reviews,merge=off|shadow")
		notify      = fs.String("notify", "", "command template to run on --notify-on events")
		notifyOn    = fs.String("notify-on", "", "pr-opened,merged,needs-human,closed,stack-parent-merged (default: merged,needs-human,closed)")
		agent       = fs.String("agent", "", "vendor:model:effort (default: review.agent)")
		maxRounds   = fs.Int("max-fix-rounds", 0, "fix rounds per trigger (default: lastmile.max_fix_rounds)")
		requester   = fs.String("requester", "", "who asked, for {{.Requester}} in --notify")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	wt, err := filepath.Abs(*worktree)
	if err != nil {
		return err
	}
	req := lastmile.Request{Worktree: wt, Base: *base, StackParent: *stackParent, Title: *title, Jira: *jira,
		Labels: commaList(*labels), Reviewers: commaList(*reviewers), Draft: *draft, Autonomy: *autonomy,
		Notify: *notify, NotifyOn: commaList(*notifyOn), Agent: *agent, MaxFixRounds: *maxRounds, Requester: *requester}
	if *bodyFile != "" {
		b, err := os.ReadFile(*bodyFile)
		if err != nil {
			return err
		}
		req.Body = string(b)
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	var h lastmile.Handoff
	if err := call(http.MethodPost, apiBase(*addr)+"/api/handoffs", body, &h); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, h.ID)
	return nil
}

// idArg parses a subcommand's flags and its one <id>, wherever the id stands.
func idArg(fs *flag.FlagSet, args []string) (string, error) {
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if fs.NArg() == 0 {
		return "", errors.New("needs a handoff id")
	}
	id := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return "", err
	}
	if fs.NArg() > 0 {
		return "", fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	return id, nil
}

func tlmStatus(args []string, stdout, stderr io.Writer) error {
	fs, addr := tlmFlags("status", stderr)
	asJSON := fs.Bool("json", false, "print the handoff as JSON")
	id, err := idArg(fs, args)
	if err != nil {
		return err
	}
	var raw json.RawMessage
	if err := call(http.MethodGet, apiBase(*addr)+"/api/handoffs/"+id, nil, &raw); err != nil {
		return err
	}
	if *asJSON {
		_, _ = fmt.Fprintln(stdout, string(raw))
		return nil
	}
	var h lastmile.Handoff
	if err := json.Unmarshal(raw, &h); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s  %s\n  %s on %s", h.ID, h.State, h.Branch, h.Repo)
	if h.PRURL != "" {
		fmt.Fprintf(stdout, " · %s", h.PRURL)
	}
	if h.Reason != "" {
		fmt.Fprintf(stdout, "\n  %s", h.Reason)
	}
	_, _ = fmt.Fprintln(stdout)
	return nil
}

// tlmAct is `tlm resume|cancel <id>`: a POST that prints the handoff's new state.
func tlmAct(verb string, args []string, stdout, stderr io.Writer) error {
	fs, addr := tlmFlags(verb, stderr)
	id, err := idArg(fs, args)
	if err != nil {
		return err
	}
	var h lastmile.Handoff
	if err := call(http.MethodPost, apiBase(*addr)+"/api/handoffs/"+id+"/"+verb, nil, &h); err != nil {
		return err
	}
	_, _ = fmt.Fprintln(stdout, cmp.Or(h.State, "?"))
	return nil
}

// tlmWait polls until the handoff is in one of the --until states or over
// (merged, closed), and prints that state. Stopping on any end state is the
// point: a script waiting for "merged" must not hang on a closed PR.
func tlmWait(args []string, stdout, stderr io.Writer) error {
	fs, addr := tlmFlags("wait", stderr)
	until := fs.String("until", lastmile.StateMerged+","+lastmile.StateNeedsHuman+","+lastmile.StateClosed, "states to stop at, comma-separated")
	id, err := idArg(fs, args)
	if err != nil {
		return err
	}
	states := commaList(*until)
	for {
		var h lastmile.Handoff
		if err := call(http.MethodGet, apiBase(*addr)+"/api/handoffs/"+id, nil, &h); err != nil {
			return err
		}
		if slices.Contains(states, h.State) || lastmile.Terminal(h.State) {
			_, _ = fmt.Fprintln(stdout, cmp.Or(h.State, "?"))
			return nil
		}
		time.Sleep(waitPoll)
	}
}

// sendsToken says whether req may carry the API token: only mutating requests,
// and only to a loopback host, so a mistyped or hostile --addr never receives it.
func sendsToken(req *http.Request) bool {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return false
	}
	host := req.URL.Hostname()
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// call sends one API request and decodes a 2xx answer into out; anything else
// is an error carrying the server's reason.
func call(method, url string, body []byte, out any) error {
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if sendsToken(req) {
		if tok := apiToken(); tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("is sisyphus running? %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.Unmarshal(b, out)
}
