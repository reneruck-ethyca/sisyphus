package config

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Duration is a time.Duration that reads from JSON strings like "10m".
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalJSON writes "10m", "1h30m" (not "10m0s"), as one would in the file.
func (d Duration) MarshalJSON() ([]byte, error) {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = s[:len(s)-2]
	}
	if strings.HasSuffix(s, "h0m") {
		s = s[:len(s)-2]
	}
	return json.Marshal(s)
}

type Config struct {
	User          string            `json:"user"` // empty = the GitHub token's user
	Org           string            `json:"org"`
	Interval      Duration          `json:"interval"`
	ReposRoot     string            `json:"repos_root"`
	StateFile     string            `json:"state_file"`
	IncludeDrafts bool              `json:"include_drafts"`
	Hooks         map[string]string `json:"hooks"` // "on_conflict": "<shell cmd>"
	HookTimeout   Duration          `json:"hook_timeout"`
	HTTPAddr      string            `json:"http_addr"` // dashboard + JSON API
	Review        Review            `json:"review"`
	LastMile      LastMile          `json:"lastmile"`
	Path          string            `json:"-"` // the file this was loaded from
}

// LastMile configures the-last-mile (TLM): PRs handed off by agents, taken to merge.
type LastMile struct {
	Disabled        bool                 `json:"disabled"`
	Interval        Duration             `json:"interval"`          // between ticks over the open handoffs
	BotReviewer     string               `json:"bot_reviewer"`      // requested on every PR once ready ("" = none)
	Reviewers       Reviewers            `json:"reviewers"`         // human reviewers when a handoff names none
	RepoReviewers   map[string]Reviewers `json:"repo_reviewers"`    // per "owner/name"; replaces Reviewers for that repo
	MaxFixRounds    int                  `json:"max_fix_rounds"`    // default of a handoff's --max-fix-rounds
	NeedsHumanLabel string               `json:"needs_human_label"` // put on a PR that waits for a human
	Flakes          []string             `json:"flakes"`            // regexes: a failed check whose log matches is a flake, rerun not fixed
	FlakeReruns     int                  `json:"flake_reruns"`      // reruns per head SHA
	MaxDiffLines    int                  `json:"max_diff_lines"`    // an agent's change above this is thrown away
	AllowPaths      []string             `json:"allow_paths"`       // globs an agent may change although the denylist names them
	BotLogins       []string             `json:"bot_logins"`        // user accounts that act as bots (their feedback counts as bot feedback)
	RiskyPaths      []string             `json:"risky_paths"`       // globs: a PR touching one would not be merged by TLM
	MigrationPaths  []string             `json:"migration_paths"`   // globs: a PR touching one must be up to date with its base to merge
	MergeFreeze     []FreezeWindow       `json:"merge_freeze"`      // windows in which TLM would not merge
}

// FreezeWindow is a daily (or weekly, with Weekday) span in TZ in which nothing merges.
// It does not cross midnight: write two windows.
type FreezeWindow struct {
	Weekday string `json:"weekday,omitempty"` // "monday".."sunday"; "" = every day
	Start   string `json:"start"`             // "HH:MM", inclusive
	End     string `json:"end"`               // "HH:MM", exclusive, after Start
	TZ      string `json:"tz"`                // IANA name; "" = UTC
}

func (w FreezeWindow) parse() (day time.Weekday, all bool, start, end int, loc *time.Location, err error) {
	all = w.Weekday == ""
	if !all {
		i := slices.IndexFunc(weekdays, func(d string) bool { return strings.EqualFold(d, w.Weekday) })
		if i < 0 {
			return 0, false, 0, 0, nil, fmt.Errorf("weekday %q is not a day name", w.Weekday)
		}
		day = time.Weekday(i)
	}
	clock := func(v string) (int, error) {
		t, err := time.Parse("15:04", v)
		return t.Hour()*60 + t.Minute(), err
	}
	if start, err = clock(w.Start); err == nil {
		end, err = clock(w.End)
	}
	if err != nil {
		return 0, false, 0, 0, nil, fmt.Errorf("start and end must be HH:MM: %w", err)
	}
	if end <= start {
		return 0, false, 0, 0, nil, errors.New("end must be after start (windows do not cross midnight)")
	}
	loc = time.UTC
	if w.TZ != "" {
		if loc, err = time.LoadLocation(w.TZ); err != nil {
			return 0, false, 0, 0, nil, err
		}
	}
	return day, all, start, end, loc, nil
}

var weekdays = []string{"sunday", "monday", "tuesday", "wednesday", "thursday", "friday", "saturday"}

// Active says t falls inside the window; an invalid window (rejected at load) never is.
func (w FreezeWindow) Active(t time.Time) bool {
	day, all, start, end, loc, err := w.parse()
	if err != nil {
		return false
	}
	t = t.In(loc)
	m := t.Hour()*60 + t.Minute()
	return (all || t.Weekday() == day) && m >= start && m < end
}

// Reviewers is a roster TLM picks human reviewers from, round-robin.
type Reviewers struct {
	Roster []string `json:"roster"`
	Count  int      `json:"count"` // how many to request; 0 = the global count (or 1)
}

// ReviewersFor is the roster for repo: its override, else the global one.
func (l LastMile) ReviewersFor(repo string) Reviewers {
	r, ok := l.RepoReviewers[repo]
	if !ok {
		r = l.Reviewers
	}
	if r.Count == 0 {
		r.Count = max(l.Reviewers.Count, 1)
	}
	return r
}

func (l LastMile) check() error {
	if l.Disabled {
		return nil
	}
	if l.Interval.Duration <= 0 || l.MaxFixRounds < 1 || l.NeedsHumanLabel == "" {
		return errors.New("needs a positive interval, max_fix_rounds >= 1 and a needs_human_label")
	}
	if l.FlakeReruns < 0 || l.MaxDiffLines < 1 {
		return errors.New("needs flake_reruns >= 0 and max_diff_lines >= 1")
	}
	for i, w := range l.MergeFreeze {
		if _, _, _, _, _, err := w.parse(); err != nil {
			return fmt.Errorf("merge_freeze[%d]: %w", i, err)
		}
	}
	for _, f := range l.Flakes {
		if _, err := regexp.Compile(f); err != nil {
			return fmt.Errorf("flakes: %w", err)
		}
	}
	if slices.Contains(l.BotLogins, "") {
		return errors.New("bot_logins must be non-empty logins")
	}
	check := func(where string, r Reviewers) error {
		if r.Count < 0 || slices.Contains(r.Roster, "") {
			return fmt.Errorf("%s: count must be >= 0 and the roster logins non-empty", where)
		}
		return nil
	}
	if err := check("reviewers", l.Reviewers); err != nil {
		return err
	}
	for repo, r := range l.RepoReviewers {
		if owner, name, ok := strings.Cut(repo, "/"); !ok || owner == "" || name == "" || strings.Contains(name, "/") {
			return fmt.Errorf("repo_reviewers key %q must be owner/name", repo)
		}
		if err := check(fmt.Sprintf("repo_reviewers[%q]", repo), r); err != nil {
			return err
		}
	}
	return nil
}

// Agent selects the CLI and its model settings for one review.
type Agent struct {
	Vendor string `json:"vendor"`
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

// UnmarshalJSON accepts the pre-profile string form ("claude" or "codex")
// so existing configs continue to load and can be saved through the new UI.
func (a *Agent) UnmarshalJSON(b []byte) error {
	var vendor string
	if json.Unmarshal(b, &vendor) == nil {
		a.Vendor = vendor
		return nil
	}
	type plain Agent
	return json.Unmarshal(b, (*plain)(a))
}

// Review configures the agent review runs.
type Review struct {
	Disabled       bool                  `json:"disabled"`
	Agent          Agent                 `json:"agent"`
	Post           bool                  `json:"post"`       // post the agent's review to GitHub (false = dry run)
	CodexHome      string                `json:"codex_home"` // private CODEX_HOME seeded from the local Codex sign-in
	MaxTurns       int                   `json:"max_turns"`
	MaxBudgetUSD   float64               `json:"max_budget_usd"`  // 0 = no limit (only enforced with API-key auth)
	PromptFile     string                `json:"prompt_file"`     // Go text/template; "" = built-in prompt
	Env            []string              `json:"env"`             // names copied from sisyphus's own environment
	EnvFile        string                `json:"env_file"`        // CLAUDE_CODE_OAUTH_TOKEN, GH_TOKEN, …
	ClaudeSettings string                `json:"claude_settings"` // settings.json given to the agent ("" = none)
	PluginsVolume  string                `json:"plugins_volume"`  // Docker volume with Claude plugins ("" = none)
	Timeout        Duration              `json:"timeout"`         // reviews past this are flagged, never stopped; lastmile tasks are stopped
	MaxConcurrent  int                   `json:"max_concurrent"`
	FiveHourPct    float64               `json:"five_hour_percent"`  // share of each 5-hour subscription window reviews may use; 0 = no limit
	Cleanup        bool                  `json:"cleanup"`            // reset worktrees sisyphus created after each run
	StackFrom      string                `json:"stack_from"`         // where stacks start: "worktree" (PR code on the host) | "safe"
	StepTimeout    Duration              `json:"setup_step_timeout"` // each setup step; past it the step is killed and the run fails
	Retries        int                   `json:"retries"`            // re-runs of a review that failed before Claude started
	RetryBackoff   Duration              `json:"retry_backoff"`      // wait before the first retry, doubling after each
	Repos          map[string]RepoReview `json:"repos"`              // only these repos are reviewed
}

// RepoReview is how one repo's PRs are reviewed.
type RepoReview struct {
	Image      string            `json:"image"`
	Workdir    string            `json:"workdir"` // container path for the worktree; "" = its host path
	EnvFile    string            `json:"env_file"`
	Env        map[string]string `json:"env"`
	Setup      string            `json:"setup"` // shell, run in workdir before Claude starts; each line is a logged, timed step
	Cache      []string          `json:"cache"` // dirs kept in per-repo volumes: workdir-relative, or absolute container paths
	PromptFile string            `json:"prompt_file"`
	Notes      string            `json:"notes"`             // repo-specific instructions, added to the prompt
	Related    *Related          `json:"related,omitempty"` // another repo the reviewer reads; nil = none
	Stack      *Stack            `json:"stack,omitempty"`   // nil = no local services
}

// Stack is the local services (DB, cache) a review needs. Up runs on the host
// before the review container starts; the container joins <project>_default.
type Stack struct {
	From    string `json:"from"`     // "worktree" | "safe"; "" = review.stack_from
	SafeDir string `json:"safe_dir"` // where "safe" runs Up; "" = the repo's clone under repos_root
	Up      string `json:"up"`       // shell; gets WORKTREE_ID=pr-<n>, and PORT_OFFSET when retrying a port clash
	Project string `json:"project"`  // compose project Up creates; "{id}" = WORKTREE_ID
}

// Related is another repo checked out read-only next to the review: the open PR
// there that names one of the reviewed PR's tickets, else Base.
type Related struct {
	Repo   string `json:"repo"`   // "owner/name"
	Base   string `json:"base"`   // branch checked out when no open PR there names one of the tickets
	Target string `json:"target"` // container path
	Clone  string `json:"clone"`  // host clone fetched from; "" = the repo's clone under repos_root
}

// Stack sources.
const (
	StackFromWorktree = "worktree"
	StackFromSafe     = "safe"
)

// fidesplus mirrors its devcontainer (.devcontainer/devcontainer.json, post-create.sh).
var fidesplus = RepoReview{
	Image:   "sisyphus-reviewer:fidesplus",
	Workdir: "/fidesplus",
	EnvFile: "~/Dev/fidesplus/.devcontainer/devcontainer.env",
	Env: map[string]string{"PYTHONPATH": "/fidesplus/src", "RUNNING_IN_DOCKER": "TRUE",
		// The stack's services by name on its network (devcontainer.env points at host.docker.internal).
		"FIDES__DATABASE__SERVER": "fidesplus-db", "FIDES__DATABASE__PORT": "5432",
		"FIDES__REDIS__HOST": "fidesplus-redis", "FIDES__REDIS__PORT": "6379",
		"UV_CACHE_DIR": "/sisyphus/cache/uv", "npm_config_cache": "/sisyphus/cache/npm",
		"UV_LINK_MODE": "copy", // the uv cache is another volume: hardlinks can't cross it
		// Lint and test caches out of the worktree: TLM holds any gitignored file an agent run
		// writes there against it, and the agent runs these checks as it fixes.
		"RUFF_CACHE_DIR": "/tmp/ruff", "MYPY_CACHE_DIR": "/tmp/mypy", "COVERAGE_FILE": "/tmp/.coverage",
		"PYTEST_ADDOPTS": "-p no:cacheprovider", "PYTHONDONTWRITEBYTECODE": "1"},
	Setup: `uv sync --all-groups
uv pip install --no-deps -e .
cp -f /usr/local/share/fides/libpbac.so src/fides/bin/libpbac.so
(cd src/fidesplus/gpp_js_integration && npm install && npm run build)
(cd clients && npm install)`,
	Cache: []string{".venv", "src/fides/bin", "src/fidesplus/gpp_js_integration/node_modules",
		"src/fidesplus/gpp_js_integration/compiled", "clients/node_modules",
		// Download caches, so a rebuilt .venv or node_modules doesn't fetch everything again. Not under
		// $HOME: a volume's missing parent dirs (~/.cache) would be created root-owned.
		"/sisyphus/cache/uv", "/sisyphus/cache/npm"},
	// The worktree's own Postgres/Redis, via fidesplus' per-worktree nox stacks.
	Stack: &Stack{Up: "uv run nox -s dev_datastores", Project: "fidesplus-{id}"},
}

// fides-qa is fidesplus' QA automation; reviews cover cardea/, its Playwright suite. Nothing runs the
// specs yet (they need a Fides deployment), so a review checks them against the ticket and against
// fidesplus' source. The fidesplus image already has node and the talos plugins.
var fidesQA = RepoReview{
	Image:   "sisyphus-reviewer:fidesplus",
	Workdir: "/fides-qa",
	Env:     map[string]string{"npm_config_cache": "/sisyphus/cache/npm"},
	Setup:   "(cd cardea && npm install)",
	Cache:   []string{"cardea/node_modules", "/sisyphus/cache/npm"},
	Related: &Related{Repo: "ethyca/fidesplus", Base: "dev", Target: "/fidesplus"},
	Notes: `This repo is the QA automation for fidesplus, the code under test (at /fidesplus, see above). cardea/
holds its Playwright end-to-end specs: read cardea/AGENTS.md, and use the ethyca-talos
` + "`cardea-conventions`" + ` skill for its conventions.

No Fides deployment is reachable from here, so don't try to run the specs. The review's main question is
whether the new and changed tests verify what their ticket asks for:
- Find the Jira ticket (PR title, branch or description) and use the ethyca-talos
  ` + "`check-acceptance-criteria`" + ` skill's steps 1-3 to gather its criteria and linked plans and trace each
  criterion to the PR's tests; skip its interactive steps. If the ticket links a fidesplus feature ticket,
  its criteria count too. With no ticket, say so and work from the PR description.
- For each criterion: is there a test that fails when the behaviour breaks? Flag criteria no test covers,
  tests that assert something the criteria don't say, and assertions too weak to catch a regression.
- Check the selectors, routes and API calls the tests rely on against /fidesplus (clients/admin-ui,
  clients/privacy-center, src/).
- Run ` + "`npm run lint`, `npm run typecheck` and `npm run format:check`" + ` in cardea/.`,
}

func DefaultPath() string {
	return ExpandHome("~/.config/sisyphus/config.json")
}

// Load reads path over the defaults. A missing file means all defaults.
func Load(path string) (Config, error) {
	cfg, err := read(path)
	if err != nil {
		return cfg, err
	}
	cfg.Path = path
	cfg.ReposRoot = ExpandHome(cfg.ReposRoot)
	r := &cfg.Review
	r.EnvFile, r.PromptFile, r.ClaudeSettings = ExpandHome(r.EnvFile), ExpandHome(r.PromptFile), ExpandHome(r.ClaudeSettings)
	r.CodexHome = ExpandHome(cmp.Or(r.CodexHome, "~/.config/sisyphus/codex"))
	for name, repo := range r.Repos {
		repo.EnvFile, repo.PromptFile = ExpandHome(repo.EnvFile), ExpandHome(repo.PromptFile)
		if repo.Stack != nil {
			st := *repo.Stack // the defaults share theirs
			st.SafeDir = ExpandHome(cmp.Or(st.SafeDir, filepath.Join(cfg.ReposRoot, filepath.Base(name))))
			repo.Stack = &st
		}
		if repo.Related != nil {
			rel := *repo.Related // the defaults share theirs
			rel.Clone = ExpandHome(cmp.Or(rel.Clone, filepath.Join(cfg.ReposRoot, filepath.Base(rel.Repo))))
			repo.Related = &rel
		}
		r.Repos[name] = repo
	}
	cfg.StateFile = ExpandHome(cfg.StateFile)
	return cfg, nil
}

// read is path over the defaults, checked, with paths as written ("~/…").
func read(path string) (Config, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return defaults(), err
	}
	return parse(path, b)
}

// parse is b (nil = no file) over the defaults, checked.
func parse(path string, b []byte) (Config, error) {
	cfg := defaults()
	if b != nil {
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("config %s: %w", path, err)
		}
		// model and codex_model were the pre-profile settings. Preserve a
		// configured value while migrating it to review.agent.model.
		var raw struct {
			Review struct {
				Model      string `json:"model"`
				CodexModel string `json:"codex_model"`
			} `json:"review"`
		}
		_ = json.Unmarshal(b, &raw)
		if raw.Review.Model != "" && cfg.Review.Agent.Vendor == "claude" {
			cfg.Review.Agent.Model = raw.Review.Model
		}
		if raw.Review.CodexModel != "" && cfg.Review.Agent.Vendor == "codex" {
			cfg.Review.Agent.Model = raw.Review.CodexModel
		}
	}
	if cfg.Interval.Duration <= 0 || cfg.HookTimeout.Duration <= 0 {
		return cfg, fmt.Errorf("config %s: interval and hook_timeout must be positive", path)
	}
	if err := cfg.Review.check(); err != nil {
		return cfg, fmt.Errorf("config %s: review: %w", path, err)
	}
	if err := cfg.LastMile.check(); err != nil {
		return cfg, fmt.Errorf("config %s: lastmile: %w", path, err)
	}
	return cfg, nil
}

func defaults() Config {
	return Config{
		Org:         "ethyca",
		Interval:    Duration{10 * time.Minute},
		ReposRoot:   "~/Dev",
		StateFile:   "~/.local/state/sisyphus/state.json",
		HookTimeout: Duration{30 * time.Minute},
		HTTPAddr:    "127.0.0.1:7465",
		Review: Review{
			Agent:         Agent{Vendor: "claude", Model: "opus", Effort: "medium"},
			Post:          true,
			CodexHome:     "~/.config/sisyphus/codex",
			MaxTurns:      200,
			EnvFile:       "~/.config/sisyphus/review.env",
			PluginsVolume: "sisyphus-claude-plugins",
			Timeout:       Duration{60 * time.Minute},
			MaxConcurrent: 1,
			Cleanup:       true,
			StackFrom:     StackFromWorktree,
			StepTimeout:   Duration{20 * time.Minute},
			Retries:       3,
			RetryBackoff:  Duration{2 * time.Minute},
			Repos:         map[string]RepoReview{"ethyca/fidesplus": fidesplus, "ethyca/fides-qa": fidesQA},
		},
		LastMile: LastMile{
			Interval:        Duration{2 * time.Minute},
			BotReviewer:     "ethycabot",
			Reviewers:       Reviewers{Count: 1},
			MaxFixRounds:    3,
			NeedsHumanLabel: "tlm:needs-human",
			FlakeReruns:     2,
			MaxDiffLines:    400,
			MigrationPaths:  []string{"**/migrations/**", "**/alembic/versions/**"},
			RiskyPaths:      []string{"**/migrations/**", "**/auth/**", ".github/**", "**/Dockerfile*", "infra/**", "**/*.tf"},
			BotLogins:       []string{"ethycabot"}, // the org's review bot, a user account; sisyphus posts as its user, so it isn't one
		},
	}
}

// volumeName is what docker run --volume takes as a named volume; anything else is a bind mount.
var volumeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

func (r Review) check() error {
	if err := r.Agent.Check(); err != nil {
		return err
	}
	if r.Disabled {
		return nil
	}
	if r.PluginsVolume != "" && !volumeName.MatchString(r.PluginsVolume) {
		return fmt.Errorf("plugins_volume %q must be a Docker volume name, not a path", r.PluginsVolume)
	}
	if r.StackFrom != StackFromWorktree && r.StackFrom != StackFromSafe {
		return fmt.Errorf("stack_from must be %q or %q", StackFromWorktree, StackFromSafe)
	}
	if r.FiveHourPct < 0 || r.FiveHourPct > 100 {
		return errors.New("five_hour_percent must be between 0 and 100")
	}
	if r.StepTimeout.Duration <= 0 || r.Retries < 0 || r.RetryBackoff.Duration <= 0 {
		return errors.New("needs a positive setup_step_timeout and retry_backoff, and retries >= 0")
	}
	if r.MaxTurns < 1 || r.Timeout.Duration <= 0 || r.MaxConcurrent < 1 {
		return errors.New("needs max_turns >= 1, a positive timeout and max_concurrent >= 1")
	}
	for name, repo := range r.Repos {
		if repo.Image == "" {
			return fmt.Errorf("repos[%q] needs an image", name)
		}
		if repo.Workdir != "" && !filepath.IsAbs(repo.Workdir) {
			return fmt.Errorf("repos[%q].workdir must be absolute", name)
		}
		if st := repo.Stack; st != nil {
			if st.From != "" && st.From != StackFromWorktree && st.From != StackFromSafe {
				return fmt.Errorf("repos[%q].stack.from must be %q, %q or empty", name, StackFromWorktree, StackFromSafe)
			}
			if st.Up == "" || st.Project == "" {
				return fmt.Errorf("repos[%q].stack needs up and project", name)
			}
		}
		for _, c := range repo.Cache {
			if !filepath.IsLocal(c) && !filepath.IsAbs(c) {
				return fmt.Errorf("repos[%q].cache %q must be a path inside the worktree or an absolute container path", name, c)
			}
		}
	}
	return nil
}

// Check verifies the agent vendors currently available in review containers.
func (a Agent) Check() error {
	if a.Vendor != "claude" && a.Vendor != "codex" {
		return fmt.Errorf("agent.vendor must be %q or %q", "claude", "codex")
	}
	if a.Model == "" {
		return errors.New("agent.model is required")
	}
	return nil
}

// ExpandHome replaces a leading "~" or "~/" with the user's home directory.
func ExpandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~/") {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, p[1:])
}
