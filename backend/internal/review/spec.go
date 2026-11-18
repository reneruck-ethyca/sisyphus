package review

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/reneruck/sisyphus/internal/config"
	"github.com/reneruck/sisyphus/internal/container"
	"github.com/reneruck/sisyphus/internal/worktree"
)

// spec builds run's container spec, writing its prompt and discussion into run.Dir.
// beforeAgent is shell run after setup, right before the agent ("" = none).
func (r *Reviewer) spec(ctx context.Context, run Run, beforeAgent string) (container.Spec, error) {
	tr := run.Job.PR
	agent := agentFor(r.cfg, run.Job)
	repo, ok := r.cfg.Repos[tr.Repo]
	if !ok {
		return container.Spec{}, fmt.Errorf("no review.repos entry for %s", tr.Repo)
	}
	workdir := cmp.Or(repo.Workdir, tr.Worktree)
	for _, d := range []string{"out", "claude"} {
		if err := os.MkdirAll(filepath.Join(run.Dir, d), 0o755); err != nil {
			return container.Spec{}, err
		}
	}
	related, err := r.checkoutRelated(ctx, run, repo.Related)
	if err != nil {
		return container.Spec{}, err
	}
	prompt, err := renderPrompt(cmp.Or(repo.PromptFile, r.cfg.PromptFile),
		PromptData{PR: tr, Reason: run.Job.Reason, PreviousHead: r.previousHead(tr.Key()), Workdir: workdir, Diff: diffCommand(run.Job),
			Related: related, Notes: repo.Notes, Stack: repo.Stack != nil})
	if err != nil {
		return container.Spec{}, fmt.Errorf("prompt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(run.Dir, "prompt.md"), []byte(prompt), 0o644); err != nil {
		return container.Spec{}, err
	}
	if r.GitHub != nil {
		if err := r.writeDiscussion(ctx, tr.PRRef, filepath.Join(run.Dir, "context")); err != nil {
			return container.Spec{}, err
		}
	}
	network := ""
	if run.Stack != nil {
		network = run.Stack.Network()
	}
	return buildSpec(ctx, r.cfg, specReq{
		Repo: tr.Repo, Worktree: tr.Worktree, Dir: run.Dir, RunID: run.ID, Agent: agent, Network: network,
		Name: "sisyphus-" + strings.NewReplacer("/", "-", "#", "-").Replace(tr.Key()) + "-" + run.ID,
		Env: map[string]string{
			"SISYPHUS_REASON": run.Job.Reason, "SISYPHUS_PR_URL": tr.URL, "SISYPHUS_PR_NUMBER": strconv.Itoa(tr.Number),
			"SISYPHUS_BRANCH": tr.HeadRef, "SISYPHUS_HEAD_SHA": tr.HeadSHA,
		},
		Labels:  map[string]string{"sisyphus": "1", "sisyphus.run": run.ID, "sisyphus.pr": tr.Key()},
		Related: repo.Related, RelatedDir: relatedDir(run),
		WithContext: r.GitHub != nil, BeforeAgent: beforeAgent,
	})
}

// specReq is what differs between a review and a task in building a container
// spec: the rest (image, caches, plugins, env files, git mounts) is shared.
// The caller has made Dir/out and Dir/claude and written Dir/prompt.md.
type specReq struct {
	Repo, Worktree, Dir, RunID, Name, Network string
	Agent                                     config.Agent
	Env                                       map[string]string // on top of the common variables, under the repo's and config's
	Override                                  map[string]string // wins over everything
	Labels                                    map[string]string
	Related                                   *config.Related
	RelatedDir                                string
	WithContext                               bool   // mount Dir/context at /sisyphus/context
	Gate                                      bool   // after setup, announce setupDone and wait for /out/<gateFile>
	BeforeAgent                               string // shell between setup (and gate) and the agent
}

// buildSpec builds the container spec of q under cfg.
func buildSpec(ctx context.Context, cfg config.Review, q specReq) (container.Spec, error) {
	agent := q.Agent
	repo, ok := cfg.Repos[q.Repo]
	if !ok {
		return container.Spec{}, fmt.Errorf("no review.repos entry for %s", q.Repo)
	}
	workdir := cmp.Or(repo.Workdir, q.Worktree)
	if cfg.ClaudeSettings != "" {
		b, err := os.ReadFile(cfg.ClaudeSettings)
		if err != nil {
			return container.Spec{}, fmt.Errorf("claude_settings: %w", err)
		}
		if err := os.WriteFile(filepath.Join(q.Dir, "claude", "settings.json"), b, 0o644); err != nil {
			return container.Spec{}, err
		}
	}
	if agent.Vendor == "codex" {
		if err := prepareCodexHome(cfg.CodexHome); err != nil {
			return container.Spec{}, err
		}
	}

	// Sources are read/write; git metadata is read-only. Host-side git (sync,
	// cleanup) runs in this worktree, so a container that could write hooks or
	// config (core.fsmonitor, …), or re-point the worktree's .git file, could run
	// commands on the host. The worktree's .git points into the clone's .git,
	// which is mounted at its own host path so the pointer resolves.
	dotGit := filepath.Join(q.Worktree, ".git")
	mounts := []container.Mount{
		{Source: q.Worktree, Target: workdir},
		{Source: dotGit, Target: filepath.Join(workdir, ".git"), ReadOnly: true},
	}
	gitDir, err := worktree.CommonDir(ctx, q.Worktree)
	if err != nil {
		return container.Spec{}, err
	}
	if real, _ := filepath.EvalSymlinks(dotGit); gitDir != real {
		mounts = append(mounts, container.Mount{Source: gitDir, Target: gitDir, ReadOnly: true})
	}
	if q.Related != nil {
		mounts = append(mounts, container.Mount{Source: q.RelatedDir, Target: q.Related.Target, ReadOnly: true})
	}
	// Dependency caches: per-repo volumes over the worktree, so the host's macOS
	// builds stay out and installs survive between runs. Volumes start root-owned.
	var owned []string
	for _, c := range repo.Cache {
		target := c // an absolute path is a container path (a cache in $HOME); relative ones are in the worktree
		if !filepath.IsAbs(c) {
			target = filepath.Join(workdir, c)
		}
		mounts = append(mounts, container.Mount{Source: volumeName(q.Repo, c), Target: target})
		owned = append(owned, target)
	}
	mounts = append(mounts,
		container.Mount{Source: filepath.Join(q.Dir, "out"), Target: "/out"},
		container.Mount{Source: filepath.Join(q.Dir, "prompt.md"), Target: "/sisyphus/prompt.md", ReadOnly: true},
		container.Mount{Source: filepath.Join(q.Dir, "claude"), Target: claudeDir}, // fresh per run
	)
	if agent.Vendor == "codex" {
		mounts = append(mounts, container.Mount{Source: cfg.CodexHome, Target: "/sisyphus/codex"})
	}
	if q.WithContext {
		mounts = append(mounts, container.Mount{Source: filepath.Join(q.Dir, "context"), Target: "/sisyphus/context", ReadOnly: true})
	}
	if cfg.PluginsVolume != "" {
		mounts = append(mounts, container.Mount{Source: cfg.PluginsVolume, Target: claudeDir + "/plugins"})
		owned = append(owned, claudeDir+"/plugins")
	}

	env := map[string]string{
		"SISYPHUS_RUN_ID": q.RunID, "SISYPHUS_REPO": q.Repo, "SISYPHUS_WORKTREE": workdir,
		"SISYPHUS_OUT":      "/out",
		"CLAUDE_CONFIG_DIR": claudeDir,
		// The container's user doesn't own the mounted files; let git work anyway.
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "safe.directory", "GIT_CONFIG_VALUE_0": "*",
	}
	maps.Copy(env, q.Env)
	maps.Copy(env, repo.Env)
	if agent.Vendor == "codex" {
		env["CODEX_HOME"] = "/sisyphus/codex"
	}
	for _, k := range cfg.Env {
		if v, ok := os.LookupEnv(k); ok {
			env[k] = v
		}
	}
	maps.Copy(env, q.Override)
	var envFiles []string
	for _, f := range []string{repo.EnvFile, cfg.EnvFile} { // later files win: secrets last
		if f != "" {
			envFiles = append(envFiles, f)
		}
	}
	return container.Spec{
		Network: q.Network, Name: q.Name,
		Image: repo.Image, Command: []string{"bash", "-c", script(cfg, repo, owned, agent, q.Gate, q.BeforeAgent)}, Env: env, EnvFiles: envFiles,
		Mounts: mounts, Workdir: workdir, Labels: q.Labels,
	}, nil
}

// claudeDir is Claude Code's config dir inside review containers.
const claudeDir = "/sisyphus/claude"

// setupDone is the line a gated container prints when its setup is over; it
// then waits for the host to create /out/<gateFile>.
const (
	setupDone = "── sisyphus: setup done ──"
	gateFile  = ".setup-ack"
)

// script is the container's command: own the volumes, run the repo's setup,
// with gate wait for the host, then the agent headless on the rendered prompt.
func script(cfg config.Review, repo config.RepoReview, owned []string, agent config.Agent, gate bool, beforeAgent string) string {
	s := setupScript(cfg, repo, owned)
	if gate {
		s += fmt.Sprintf("echo %s\nuntil [ -e /out/%s ]; do sleep 0.2; done\nrm -f /out/%s\n", shellQuote(setupDone), gateFile, gateFile)
	}
	return s + beforeAgent + agentScript(cfg, agent)
}

func agentScript(cfg config.Review, agent config.Agent) string {
	if agent.Vendor == "codex" {
		return codexScript(agent)
	}
	return claudeScript(cfg, agent)
}

func agentFor(cfg config.Review, job Job) config.Agent {
	if job.Agent.Vendor == "" {
		return cfg.Agent // compatibility with review jobs saved before profiles
	}
	return job.Agent
}

func codexScript(agent config.Agent) string {
	var sb strings.Builder
	sb.WriteString("sudo -n chown -R \"$(id -u):$(id -g)\" /sisyphus/codex 2>/dev/null || true\nsudo -n chmod 700 /sisyphus/codex 2>/dev/null || true\n")
	sb.WriteString("echo '── sisyphus: codex ──'\n")
	args := []string{"codex", "exec", "--json", "--dangerously-bypass-approvals-and-sandbox", "--model", agent.Model}
	if agent.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+agent.Effort)
	}
	args = append(args, "-")
	for i := range args {
		args[i] = shellQuote(args[i])
	}
	sb.WriteString("exec " + strings.Join(args, " ") + " < /sisyphus/prompt.md\n")
	return sb.String()
}

// prepareCodexHome keeps Codex credentials in a dedicated directory rather
// than exposing the user's entire ~/.codex to a review container. Existing CLI
// sign-ins are copied once; Codex maintains its refreshed auth there afterward.
func prepareCodexHome(home string) error { // config.Load always sets it
	if err := os.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("codex_home: %w", err)
	}
	// Review containers write into home, so never follow a link they left there.
	if fi, err := os.Lstat(home); err != nil || !fi.IsDir() {
		return fmt.Errorf("codex_home %s must be a real directory", home)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		return fmt.Errorf("codex_home: %w", err)
	}
	auth := filepath.Join(home, "auth.json")
	if fi, err := os.Lstat(auth); err == nil {
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("codex_home: %s is not a regular file", auth)
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	userHome, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(userHome, ".codex", "auth.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil // OPENAI_API_KEY or a first-run CLI login can still be used
	}
	if err != nil {
		return fmt.Errorf("reading Codex CLI sign-in: %w", err)
	}
	f, err := os.OpenFile(auth, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("copying Codex CLI sign-in: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return fmt.Errorf("copying Codex CLI sign-in: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("copying Codex CLI sign-in: %w", err)
	}
	return nil
}

func setupScript(cfg config.Review, repo config.RepoReview, owned []string) string {
	var sb strings.Builder
	sb.WriteString("set -eo pipefail\n")
	for _, d := range owned {
		fmt.Fprintf(&sb, "sudo -n chown \"$(id -u):$(id -g)\" %s 2>/dev/null || true\n", shellQuote(d))
	}
	// Each step is announced and timed, so the run log shows where it is, and runs
	// in its own shell under setup_step_timeout, so a hung install can't hold the run.
	limit := int(cfg.StepTimeout.Seconds())
	for _, step := range setupSteps(repo.Setup) {
		label := strings.Join(strings.Fields(strings.ReplaceAll(step, "\\\n", " ")), " ")
		fmt.Fprintf(&sb, `echo %s
t=$SECONDS; rc=0
timeout -k 30 %d bash -eo pipefail -c %s || rc=$?
[ $rc -ne 124 ] && [ $rc -ne 137 ] || echo %s
echo "── sisyphus: took $((SECONDS-t))s"
[ $rc -eq 0 ] || exit $rc
`, shellQuote("── sisyphus: setup: "+label+" ──"), limit, shellQuote(step),
			shellQuote("── sisyphus: timed out after "+cfg.StepTimeout.String()+" (setup_step_timeout)"))
	}
	return sb.String()
}

func claudeScript(cfg config.Review, agent config.Agent) string {
	var sb strings.Builder
	sb.WriteString("echo '── sisyphus: claude ──'\n")
	args := []string{"claude", "-p", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions",
		"--model", agent.Model, "--max-turns", strconv.Itoa(cfg.MaxTurns)}
	if agent.Effort != "" {
		args = append(args, "--effort", agent.Effort)
	}
	if cfg.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(cfg.MaxBudgetUSD, 'f', -1, 64))
	}
	for i, a := range args {
		args[i] = shellQuote(a)
	}
	// The image's tooling, if it has any: plugins (skills, LSPs) and MCP servers.
	sb.WriteString(`extra=()
for d in /opt/sisyphus/plugins/*/; do [ -d "$d" ] && extra+=(--plugin-dir "$d"); done
[ -f /opt/sisyphus/mcp.json ] && extra+=(--mcp-config /opt/sisyphus/mcp.json)
`)
	sb.WriteString("exec " + strings.Join(args, " ") + ` "${extra[@]}" < /sisyphus/prompt.md` + "\n")
	return sb.String()
}

// setupSteps splits a setup script into its commands: one per line, a line
// ending in a backslash continuing on the next.
func setupSteps(setup string) []string {
	var steps []string
	cur := ""
	for _, line := range strings.Split(setup, "\n") {
		cur += line
		if strings.HasSuffix(line, "\\") {
			cur += "\n"
			continue
		}
		if strings.TrimSpace(cur) != "" {
			steps = append(steps, cur)
		}
		cur = ""
	}
	if strings.TrimSpace(cur) != "" {
		steps = append(steps, cur)
	}
	return steps
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// volumeName is the per-repo cache volume for a workdir-relative path.
func volumeName(repo, path string) string {
	clean := func(s string) string {
		return strings.Map(func(c rune) rune {
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-' {
				return c
			}
			return '-'
		}, s)
	}
	return "sisyphus-" + clean(repo) + "-" + clean(path)
}
