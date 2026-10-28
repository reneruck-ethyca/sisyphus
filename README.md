# sisyphus

Automated PR reviews for the PRs waiting on you. sisyphus finds open PRs that request your review,
checks each out into its own worktree, and has Claude Code review it in a container built like the
repo's devcontainer, with the PR's own local services (Postgres, Redis) next to it. The agent reads the
code and the discussion, runs tests, and writes a review; sisyphus posts it to GitHub as you. New
commits and replies get follow-up reviews until the PR merges or closes.

It runs on your Mac as a menu-bar app with a local dashboard, every `interval` (default 10m):

1. **track**: pick up PRs in `org` that request a review from you or one of your teams, with a
   worktree each (an existing one on that branch, else `wtp add`); sync them on new commits; notice new
   replies; drop PRs that are done (see [Which PRs](#which-prs));
2. **review**: queue a review for a new PR, new commits or new replies, and run it in a container:
   repo setup, then `claude -p` headless (see [Reviews](#reviews));
3. **post**: publish the agent's verdict, summary and inline comments from the host with your `gh`
   login.

## Getting started

Requires `gh` (authenticated), `git` and `wtp` on PATH, Docker, and for fidesplus' local stack `uv`
on the host. `make install-agent` bakes your current shell's PATH into the LaunchAgent.

1. Put the reviewer's secrets in `~/.config/sisyphus/review.env` (`chmod 600`):
   `CLAUDE_CODE_OAUTH_TOKEN` and a read-only fine-grained `GH_TOKEN` (the agent uses it for
   `gh pr diff`; posting uses your host `gh` login). Until the file exists, reviews wait and say so.
   For Jira and Confluence (read-only), add `JIRA_USERNAME` (your Atlassian email) and an Atlassian
   API token as `JIRA_API_TOKEN`; without them the reviewer has no Atlassian tools.
2. Build the review image: `make reviewer-image` builds `sisyphus-reviewer:fidesplus` from the
   fidesplus devcontainer in `$(FIDESPLUS)` (default `~/Dev/fidesplus`), with Claude Code pinned, plus
   the reviewer's tooling: the ethyca-talos and superpowers plugins and `adversarial-review`, copied
   from your Claude Code install without their hooks (`make reviewer-plugins`; rebuild to pick up
   updates), pyright and typescript-language-server for the LSP tool, semgrep, and `mcp-atlassian`.
3. Start it: `make install-agent` (at login, via LaunchAgent) or `make start`. The dashboard is at
   <http://127.0.0.1:7465/>.

## Usage

```bash
make build                     # bin/sisyphus (needs cgo / Xcode CLT)
make test                      # with -race
make lint                      # golangci-lint (.golangci.yml)
bin/sisyphus -once             # single tick, no tray or window; good for testing
bin/sisyphus                   # menu-bar app ("SY"): status, Open window, Open dashboard, Check now, Open state file, Quit
bin/sisyphus -config <file>    # another config file
make install-agent             # run at login via LaunchAgent; logs to ~/Library/Logs/sisyphus.log
make start | stop | restart    # this checkout's build, through the LaunchAgent if installed, else in the background
make reset                     # stop, wipe all state (see below), start again
make reset PR='ethyca/fidesplus#123'   # …just that PR
make shell PR='ethyca/fidesplus#123'   # a shell in that PR's review container (see below)
make reviewer-image            # rebuild the fidesplus review image
make uninstall-agent
```

The dashboard (`http_addr`) and the window show the same things: every tracked PR with its sync and
review state, a PR page with its review runs, its stack and the [PR actions](#pr-actions), each run
streamed live, a live log viewer and the settings: the window is the dashboard in a webview. If the
port is taken, sisyphus logs it and serves on a free one (the window and "Open dashboard" use it). If Docker is down, reviews wait in the queue and are retried every minute; at
login, re-attaching to running reviews waits for it too.

## Config

Optional JSON at `~/.config/sisyphus/config.json` (`-config` to override). The dashboard (`/settings`)
and the window ("Settings") edit the scalar fields below; `repos`, `hooks`, `env`, `state_file` and
`http_addr` stay file-only. Saving writes only the changed fields, keeps the rest of the file, and is
refused if the result wouldn't load. Changes apply on restart: the Restart button re-execs sisyphus in
place (same PID, so the LaunchAgent is happy; review containers are re-attached).

| Field | Default | |
|---|---|---|
| `user` | `gh api user` | GitHub login whose reviews are tracked |
| `org` | `ethyca` | owner searched for PRs |
| `interval` | `10m` | Go duration between ticks |
| `repos_root` | `~/Dev` | clones live at `<repos_root>/<repo>`; PRs without a clone are skipped |
| `state_file` | `~/.local/state/sisyphus/state.json` | tracked PRs |
| `include_drafts` | `false` | also track draft PRs |
| `hooks` | `{}` | `{"on_conflict": "<shell command>"}` |
| `hook_timeout` | `30m` | per hook run |
| `http_addr` | `127.0.0.1:7465` | dashboard + JSON API; only loopback `Host`s and same-origin POSTs are accepted |
| `review.disabled` | `false` | don't run reviews |
| `review.post` | `true` | post the agent's review to GitHub; `false` = dry run (review stays in `out/review.json`) |
| `review.model` | `opus` | Claude model |
| `review.max_turns` | `200` | Claude turn limit; hitting it fails the run |
| `review.max_budget_usd` | `0` (none) | spend limit (only enforced with API-key auth) |
| `review.prompt_file` | built-in | Go `text/template` prompt (see `internal/review/prompt.md`) |
| `review.env_file` | `~/.config/sisyphus/review.env` | `CLAUDE_CODE_OAUTH_TOKEN`, and a read-only `GH_TOKEN` for `gh pr diff` (sisyphus posts with your host `gh` login) |
| `review.env` | `[]` | names of variables copied from sisyphus's environment |
| `review.claude_settings` | — | a `settings.json` given to Claude in each run |
| `review.plugins_volume` | `sisyphus-claude-plugins` | Docker volume mounted as Claude's `plugins/` |
| `review.timeout` | `60m` | runs longer than this are flagged in the UIs, never stopped |
| `review.max_concurrent` | `1` | review containers at a time |
| `review.five_hour_percent` | `0` (none) | share of each 5-hour subscription window reviews may use; past it, new reviews wait for the reset |
| `review.cleanup` | `true` | after each run, reset worktrees sisyphus created to the reviewed head |
| `review.setup_step_timeout` | `20m` | each setup step; past it the step is killed and the run fails |
| `review.retries` | `3` | re-runs of a review that failed before Claude started (setup, stack, container start); `0` = none |
| `review.retry_backoff` | `2m` | wait before the first retry, doubling after each (2m, 4m, 8m) |
| `review.stack_from` | `worktree` | where local stacks start: `worktree` (the PR's own stack files, run on the host) or `safe` (each repo's `stack.safe_dir`); see [Local stack](#local-stack) |
| `review.repos` | `ethyca/fidesplus`, `ethyca/fides-qa` | per repo: `image`, `workdir`, `env_file`, `env`, `setup`, `cache`, `prompt_file`, `notes` (repo instructions added to the prompt), `mounts` (host path → container path, read-only), `stack`; only these repos are reviewed |

## Which PRs

A PR is picked up only when it requests a review from you or one of your teams. Until you've
reviewed it, it's dropped (and its worktree removed, if sisyphus created it) when it's merged or
closed, someone else approved it, it became a draft (unless `include_drafts`), or it no longer requests
a review from you or your teams. Once you've reviewed it (by hand, or the agent as you), only merging
or closing drops it: new commits and replies keep getting follow-up reviews, approvals included.
Fork PRs are skipped.

Syncing on new commits fast-forwards the worktree. After a force-push it hard-resets only if
the worktree is still exactly where sisyphus last put it. Reused worktrees and pre-existing
branches are never reset or deleted. Modified tracked files, unpushed local commits, diverged
history, or an incoming file colliding with an untracked one are a conflict.

GitHub search (`--review-requested`) only finds candidates; the PR's own payload decides, both when
a PR is picked up and on every tick (`requested_reviewers`, and `requested_teams` against your teams).
If your teams can't be fetched, the tick does nothing rather than drop team-requested PRs.

## Reviews

A run is started when a PR is first tracked (`new`), after new commits (`commits`, which also stops a
run still going on the old head) and after new replies (`replies`, queued behind a running one). Each
PR has at most one queued job (the newest; a never-reviewed PR stays `new`) and one running container,
which holds the PR's worktree lock: the tracker doesn't sync or remove that worktree meanwhile, asks
for it back instead, and catches up on the next tick. Only repos in `review.repos` are reviewed. A
tracked PR that never had a review gets one queued after the next tick. Jobs whose env files are missing
stay queued, say so, and start within a minute of the file appearing.

A run that fails before Claude starts (a setup step failing or passing `setup_step_timeout`, the stack
not coming up, the container not starting) is likely transient: it's retried up to `review.retries`
times, after `review.retry_backoff`, doubling each time. Claude's own failures and posting failures
aren't retried. New commits, replies, or **Review now** (PR page, both UIs; queues a full review behind
any run in progress) replace a waiting retry.

Every tick logs one `PR` line per tracked PR: its head, sync state, and where its review stands and
why (`queued, waiting: env file missing: …`, `up to date: reviewed 4f0b24f8 at …; next one on new
commits or replies`, …). The UIs show the same line on the PR page and as the review cell's tooltip.

Each run is Claude Code headless in the repo's image (`make reviewer-image` builds
`sisyphus-reviewer:fidesplus` from the fidesplus devcontainer in `$(FIDESPLUS)`, default
`~/Dev/fidesplus`, with Claude Code pinned; fides-qa reviews reuse it):

1. the repo's cache volumes (`sisyphus-<repo>-<path>`) are mounted: relative paths over the worktree
   (`.venv`, `node_modules`), absolute ones as is (fidesplus keeps uv's and npm's download caches in
   `/sisyphus/cache/`). Then its `setup` runs, each line a step announced and timed in the run log
   (`uv sync`, `npm install`, …; a trailing `\` continues a line), each in its own shell under
   `setup_step_timeout`; a failing or timed-out step fails the run;
2. `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --model … --max-turns …`
   gets the rendered prompt (`prompt.md` in the run dir), reads the PR's discussion from
   `/sisyphus/context/` (fetched on the host: PR, reviews, inline comments with ids, issue comments),
   reviews, runs tests, and writes its review to `/out/review.json`. A fresh session each run; its
   transcript stays in the run's `claude/` dir;
3. sisyphus posts that review from the host with your `gh` login (`review.post`, default on; off is a
   dry run): the verdict, body (plus a "🤖 Reviewed by sisyphus" footer) and inline comments, pinned to
   the reviewed head, and replies in inline threads. If GitHub refuses the review (an inline comment
   outside the diff, or approving your own PR) it's posted once more as one plain comment carrying
   everything. No `review.json` means nothing to say. The agent itself never needs the GitHub API.

The worktree is mounted read/write at the repo's `workdir` (`/fidesplus`); its `.git` and the clone's
`.git` are read-only (a container that could write them could plant hooks or config that host-side git
would run; it also means no `git commit` inside). A repo's `mounts` add other checkouts read-only. The container gets `/out`, the env files (repo's,
then `review.env_file`), and `SISYPHUS_RUN_ID`, `SISYPHUS_REASON`, `SISYPHUS_PR_URL`, `SISYPHUS_REPO`,
`SISYPHUS_PR_NUMBER`, `SISYPHUS_BRANCH`, `SISYPHUS_HEAD_SHA`, `SISYPHUS_WORKTREE`, `SISYPHUS_OUT`. With
`review.cleanup`, worktrees sisyphus created are reset to the reviewed head afterwards; reused ones never.

Both UIs stream each run live and readably (Claude's text, tool calls, collapsible tool output, the
final result with turns and cost); the raw output is the run's `log`. Runs live in
`<state dir>/runs/<owner>/<repo>/<n>/<run>/` (`run.json`, `prompt.md`, `log`, `stack.log`, `context/`,
`claude/`, `out/`); the
queue and the last 10 runs per PR are in `<state dir>/reviews.json`. Containers keep running when
sisyphus quits and are re-attached on start; a run whose container is gone is marked `lost` and requeued.

The agent may approve; the PR stays tracked until it merges or closes.

### fides-qa

fides-qa reviews cover `cardea/`, the Playwright suite. Nothing runs the specs yet (they need a Fides
deployment), so the prompt's repo notes point the agent at the ticket instead: it gathers the Jira
ticket's acceptance criteria and linked plans (talos `check-acceptance-criteria`, steps 1-3), traces
each criterion to the PR's tests, checks selectors, routes and API calls against fidesplus' source
(`~/Dev/fidesplus`, mounted read-only at `/fidesplus`, at whatever that clone has checked out), and runs
cardea's lint, typecheck and format check.

## Local stack

A repo with a `stack` gets its own services (DB, cache) per PR, next to the review container.
fidesplus uses its per-worktree nox stacks: `{"up": "uv run nox -s dev_datastores", "project":
"fidesplus-{id}"}`, i.e. Postgres and Redis.

Before the container starts, sisyphus runs `up` on the host with `WORKTREE_ID=pr-<n>` (so a PR keeps
its compose project, `fidesplus-pr-<n>`, and volumes whichever mode started it), output in the run's
`stack.log`. The container joins `<project>_default` and reaches the services by name
(`fidesplus-db:5432`, `fidesplus-redis:6379`); host ports don't matter to it. If `up` fails on a host
port that's taken ("port is already allocated"), sisyphus retries with the next `PORT_OFFSET` slot and
remembers the one that worked for that PR. Any other failure fails the run.

After a succeeded (or cancelled) run the stack is stopped, not removed, so the PR's next review starts
on the same DB. After a failed or errored one it's removed, volumes included: its DB may be half set up. It's
removed with its volumes (`docker compose -p <project> down`, volumes by label) when the PR is
dropped or reset.

Where `up` runs (`stack.from`, else `review.stack_from`):

- `worktree` (default): in the PR's worktree, so the PR's own `noxfiles/` and `docker-compose.yml` run
  on your host. Fine for private, trusted repos.
- `safe`: in `stack.safe_dir` (default: the repo's clone under `repos_root`, `~/Dev/fidesplus`), so the
  PR's code only ever runs in the container. That checkout's own nox code is what runs, on whatever
  branch it's on.


## PR actions

The `…` menu on a PR's page (dashboard and window), each confirmed first and run between ticks:

- **Reset review status**: stops a running review, forgets the PR's review history, run dirs and
  queued jobs; a fresh review is queued. The stack stays.
- **Wipe review data**: the same, plus the PR's stack (containers, network, DB volume) and the images
  its containers ran, unless other containers use them. Tracking and the worktree stay; the reviewer
  image and per-repo caches are shared, so they stay too.
- **Untrack PR**: wipe, then forget the PR and remove the worktree and branch sisyphus created. It's
  ignored from then on, even while it requests your review; the PR list's "Untracked" footer tracks it
  again.

The PR page also shows its stack as Docker has it: containers with state and image, volumes, network.

## Reset

`make reset` (or `bin/sisyphus -reset all|owner/repo#n` with the app stopped) removes sisyphus's review
containers, local stacks, review history and run directories, the worktrees it created (a worktree it can't remove
keeps its PR tracked, and the error says why), and the tracked entries. On start, sisyphus rediscovers
whatever still awaits your review as new: fresh worktree, fresh review. PRs you untracked stay ignored.

## Debug shell

`make shell PR='owner/repo#n'` (`bin/sisyphus -shell owner/repo#n`) opens bash in the container a review
of that tracked PR would run in: same image, worktree and cache mounts, env files and variables,
prompt and discussion, with its stack up and the container on its network. `/sisyphus/setup.sh` runs
the setup as a review does; `/sisyphus/review.sh` runs setup and then Claude (its review lands in
`/out`, nothing is posted). Exiting removes the container and stops the stack, keeping its volumes. It
runs next to the app without touching its state, and refuses while a review of that PR is running.
The app keeps syncing the worktree meanwhile, so new commits can move it under the shell.

## Evals

`bin/sisyphus -eval <case file or dir>` replays past reviews to measure the reviewer: each case is a PR
whose review (by people, or a run you trust) is the gold standard, written down as the findings it
raised. A run reviews the PR in a fresh worktree at the head the gold review saw, diffs against the
merge-base, and sees only the discussion from before the gold review; Claude gets no GitHub token, so
it can't read the rest. Nothing is posted. Each case runs `-eval-reps` times (3), one run at a time,
then a judge (`claude -p`, no tools) scores every run against its case and the report lands in
`report.md` next to the runs (`-eval-out`, default `~/.config/sisyphus/eval/runs/<time>`):

- recall: gold findings found (1) or partly found (½), weighted must 3, should 2, nice 1; and the
  same over must findings alone
- false positives (including any `non_issues` the run reported), and plausible new findings the gold
  review didn't raise: label those in the case, as findings or non-issues, then
  `bin/sisyphus -judge -eval-out <dir>` re-scores the runs against the edited cases
- whether the verdict matches, how many findings the review says it reproduced, cost, turns, minutes
- leaked: the PR's later commits or its merge commit showing up in the run's log. They're still in the
  clone, so a run that went looking for them is marked, not prevented

A variant is a sisyphus config (`-config`): image, prompt, model and turn limit come from it, so to
compare two, run the same cases with each and compare the reports. Run evals while the app is idle; they
don't wait on `review.five_hour_percent`. Keep cases out of this repo (it's public); the default spot
is `~/.config/sisyphus/eval/cases/`. A case:

```json
{"pr": "ethyca/fidesplus#6529", "sha": "d963d1a7edb5", "cutoff": "2026-10-26T17:17:00Z",
 "verdict": "REQUEST_CHANGES",
 "findings": [{"id": "rbac-403", "weight": "must", "where": "src/fidesplus/api/v1/endpoints/x.py:40-52",
               "what": "respondents get a 403 once RBAC is on", "source": "https://github.com/…#discussion_r1"}],
 "non_issues": [{"id": "dup-query", "what": "the second query is intentional: it bypasses the cache"}],
 "notes": "context the judge needs that the PR doesn't say"}
```

`sha` is the head the gold review saw and `cutoff` when it was written; `base` overrides the diff base
(needed when the PR was merged with a merge commit).

## `on_conflict` hook

Runs via `sh -c` with the worktree as cwd, at most 3 times per PR head (sync is retried after
each run). If it's still conflicting the PR gets `last_error`; the hook runs again only after a
new push. Environment:
`SISYPHUS_HOOK`, `SISYPHUS_PR_URL`, `SISYPHUS_REPO`, `SISYPHUS_PR_NUMBER`, `SISYPHUS_BRANCH`,
`SISYPHUS_WORKTREE`, `SISYPHUS_CONFLICT_REASON` (`dirty` | `ahead` | `diverged` | `untracked-collision`).

## Architecture

Tracking and reviewing meet in `tracker.Events`: the tracker announces a PR (`Tracked`, `Updated`)
only once its worktree is synced to the head it announces, asks for a worktree back (`Reclaim`) when
it needs to sync or remove one a review is using, and reports `Dropped`. `review.Reviewer` implements
it: a queue, review containers running Claude Code, local stacks, run records and posting.

| Package | |
|---|---|
| `internal/tracker` | discover / update / drop, state file, drop rules, untrack, `on_conflict`, events, per-PR worktree locks |
| `internal/review` | job queue (one per PR), container runs, local stacks, posting, run records, re-attach after restart, debug shell |
| `internal/container` | `Runtime` interface; `Docker` (docker CLI) and `Fake` |
| `internal/worktree` | git/wtp operations |
| `internal/github` | `Client` interface; `CLI` (gh) and `Fake` |
| `internal/config` | config file, defaults, the settings the UIs edit |
| `internal/app` | `Service`: the boundary UIs talk to |
| `internal/proc`, `internal/jsonfile` | external commands with timeouts; atomic JSON state files |
| `internal/gittest`, `internal/testkit` | test helpers |
| `main` | wiring, CLI flags, dashboard (`http.go`, `web/`), Wails window and menu bar (`ui.go`) |

UIs talk to `app.Service` (`Status`, `PR`, `RunNow`, `ReviewNow`, `ResetReviews`, `Wipe`, `Untrack`,
`Retrack`, `Stack`, `RunDir`, `RunEvents`, `SubscribeRunEvents`, `OnChange`, `Logs`, `SubscribeLogs`,
`Settings`, `SaveSettings`). It hands out copies of state, so a UI or API never races a tick or a run;
the tracker locks, so PR actions wait for a tick in progress.

The menu-bar item and window are Wails v3 (beta, pinned in `go.mod`) (`ui.go`), one process. The
window is a webview on the dashboard's URL: one UI, server-rendered. Its user agent carries
`sisyphus-window`, which the pages use to send external links to `POST /api/open` (GitHub only) so
the webview never leaves the dashboard. No Wails CLI, npm or JS bindings; `go build` is enough.

The dashboard (`web/`, server-rendered) serves `/`, `/prs/{owner}/{repo}/{number}`, a run at
`…/runs/{run}` with its files under `…/runs/{run}/` (`log`, `run.json`, `out/`, …; served sandboxed with
CSP `sandbox` and `nosniff`, so HTML a reviewer writes can't script the dashboard), `/logs` and
`/settings`. Only loopback `Host`s and same-origin POSTs are accepted. The JSON API:

| | |
|---|---|
| `GET /api/status`, `GET /api/prs/{owner}/{repo}/{number}` | status snapshots |
| `GET /api/status/stream` | the status on every change (server-sent events); pages reload on it |
| `POST /api/open` (`url=`), `POST …/runs/{run}/open` | open a GitHub link in the browser; a run's directory in Finder |
| `POST /api/run` | tick now |
| `POST /api/prs/{owner}/{repo}/{number}/review` | Review now |
| `POST /api/prs/{owner}/{repo}/{number}/reset` \| `wipe` \| `untrack` \| `track` | [PR actions](#pr-actions); `track` undoes untrack |
| `GET /api/prs/{owner}/{repo}/{number}/runs/{run}/events` | a run's parsed events (server-sent events: backlog, then live) |
| `GET /api/logs`, `GET /api/logs/stream` | last 1000 ops log lines; live (server-sent events) |
| `GET`/`PUT /api/settings`, `POST /api/restart` | settings; re-exec to apply them |
