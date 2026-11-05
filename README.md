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
   `CLAUDE_CODE_OAUTH_TOKEN` (or `OPENAI_API_KEY` for API-billed Codex) and a read-only fine-grained `GH_TOKEN` (the agent uses it for
   `gh pr diff`; posting uses your host `gh` login). Until the file exists, reviews wait and say so.
   For ChatGPT plan usage, sign in once with `codex` on the host. Sisyphus seeds the dedicated
   `review.codex_home` from `~/.codex/auth.json` on the first Codex run and keeps refreshed credentials there.
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

The dashboard (`http_addr`) and the window show the same things. The landing page (`/`) gathers what
needs you (handoffs waiting for a person, failed reviews, PRs sisyphus reviewed that wait for yours),
the reviews and last-mile handoffs in flight, run slots and the 5-hour budget, and the shadow-merge
report; `/reviews` lists every tracked PR with its sync and
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
| `review.agent` | `{"vendor":"claude","model":"opus","effort":"medium"}` | app-wide default review profile; a tracked PR can override it in the dashboard |
| `review.max_turns` | `200` | Claude turn limit; hitting it fails the run |
| `review.max_budget_usd` | `0` (none) | spend limit (only enforced with API-key auth) |
| `review.prompt_file` | built-in | Go `text/template` prompt (see `internal/review/prompt.md`) |
| `review.env_file` | `~/.config/sisyphus/review.env` | `CLAUDE_CODE_OAUTH_TOKEN` or optional `OPENAI_API_KEY`, and a read-only `GH_TOKEN` for `gh pr diff` (sisyphus posts with your host `gh` login) |
| `review.codex_home` | `~/.config/sisyphus/codex` | private persistent Codex CLI state and ChatGPT sign-in for review containers |
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
| `review.repos` | `ethyca/fidesplus`, `ethyca/fides-qa` | per repo: `image`, `workdir`, `env_file`, `env`, `setup`, `cache`, `prompt_file`, `notes` (repo instructions added to the prompt), `related` (another repo checked out read-only, see [fides-qa](#fides-qa)), `stack`; only these repos are reviewed |

## Which PRs

A PR is picked up only when it requests a review from you or one of your teams. Until you've
reviewed it, it's dropped (and its worktree removed, if sisyphus created it) when it's merged or
closed, someone else approved it, it became a draft (unless `include_drafts`), or it no longer requests
a review from you or your teams. Once you've reviewed it (by hand, or the agent as you), only merging
or closing drops it: new commits and replies keep getting follow-up reviews. Once your latest verdict
is an approval, only new commits trigger one; replies are ignored.
Fork PRs are skipped.

Syncing on new commits fast-forwards the worktree. After a force-push it hard-resets only if
the worktree is still exactly where sisyphus last put it. Reused worktrees and pre-existing
branches are never reset or deleted. Modified tracked files, unpushed local commits, diverged
history, or an incoming file colliding with an untracked one are a conflict.

GitHub search (`--review-requested`) only finds candidates; the PR's own payload decides, both when
a PR is picked up and on every tick (`requested_reviewers`, and `requested_teams` against your teams).
If your teams can't be fetched, the tick does nothing rather than drop team-requested PRs.

## Reviews

Design reviews are tracked but never reviewed on their own: no first review, no follow-ups. Start
one from the PR page when you want it. A PR counts as a design review when every file it changes is
prose (under `docs/`, or `.md`/`.mdx`/`.rst`, ignoring `changelog/` fragments) and its title says
design, ADR or RFC, or a doc sits under a `designs/`, `adrs/`, `rfcs/` or `plans/` directory. This is
decided again for each new head, so a design PR that gains code gets automatic reviews from then on.

Otherwise, a run is started when a PR is first tracked (`new`), after new commits (`commits`, which also stops a
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

Each run uses the selected CLI headlessly in the repo's image (`make reviewer-image` builds
`sisyphus-reviewer:fidesplus` from the fidesplus devcontainer in `$(FIDESPLUS)`, default
`~/Dev/fidesplus`, with Claude Code pinned; fides-qa reviews reuse it):

1. the repo's cache volumes (`sisyphus-<repo>-<path>`) are mounted: relative paths over the worktree
   (`.venv`, `node_modules`), absolute ones as is (fidesplus keeps uv's and npm's download caches in
   `/sisyphus/cache/`). Then its `setup` runs, each line a step announced and timed in the run log
   (`uv sync`, `npm install`, …; a trailing `\` continues a line), each in its own shell under
   `setup_step_timeout`; a failing or timed-out step fails the run;
2. The selected CLI gets the rendered prompt (`prompt.md` in the run dir): Claude uses
   `claude -p --output-format stream-json --verbose --dangerously-skip-permissions --model … --max-turns …`;
   Codex uses `codex exec --json --dangerously-bypass-approvals-and-sandbox --model … -` (the container is the sandbox; Codex's own can't start inside Docker). The selected profile also supplies its
   reasoning effort. It reads the PR's discussion from
   `/sisyphus/context/` (fetched on the host: PR, reviews, inline comments with ids, issue comments),
   reviews, runs tests, and writes its review to `/out/review.json`. Claude has a fresh config
   directory per run; Codex keeps its dedicated sign-in state in `review.codex_home`;
3. sisyphus posts that review from the host with your `gh` login (`review.post`, default on; off is a
   dry run): the verdict, body (plus a "🤖 Reviewed by sisyphus" footer) and inline comments, pinned to
   the reviewed head, and replies in inline threads. If GitHub refuses the review (an inline comment
   outside the diff, or approving your own PR) it's posted once more as one plain comment carrying
   everything. No `review.json` means nothing to say. The agent itself never needs the GitHub API.

The worktree is mounted read/write at the repo's `workdir` (`/fidesplus`); its `.git` and the clone's
`.git` are read-only (a container that could write them could plant hooks or config that host-side git
would run; it also means no `git commit` inside). The container gets `/out`, the env files (repo's,
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
each criterion to the PR's tests, checks selectors, routes and API calls against fidesplus' source, and
runs cardea's lint, typecheck and format check.

fidesplus is the repo's `related` checkout, read-only at `/fidesplus`. Before each run sisyphus takes the
Jira keys from the PR's title and branch (not its body, whose "Part of" epics match unrelated PRs) and
looks for open fidesplus PRs naming one (`gh pr list --search '"ENG-…" in:title,body'`, forks left out). The most
recently updated one's head is checked out, else `dev`; the prompt says which and why. It's a
`git archive` of that commit from `~/Dev/fidesplus` into the run dir, deleted after the run: tracked
files only, so nothing ignored in your clone (`.env` files) reaches the container, and no git metadata.

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

## Last mile (TLM)

An agent that finished a branch hands it over instead of babysitting the PR; sisyphus then owns the
worktree (the agent must not touch it again). sisyphus then pushes the branch and opens the PR (title
prefixed `ENG-1234: ` when `--jira` is given, body from `--body-file`, else the repo's
`.github/pull_request_template.md`), and every `lastmile.interval` reads the PR and moves the handoff along:
`handed-off → pr-open → ci-running → ci-red | ci-green → awaiting-review → approved → merged`, or `closed`
when someone closes it. CI is green when every check completed without failure (skipped and neutral pass).
Red CI puts the handoff in `needs-human` (the `lastmile.needs_human_label` on the PR, the `needs-human`
event, a banner on the Last mile page) unless `fix-ci` is on, where the fix agent takes over (below).
`needs-human` stays until a person deals with the PR; merged and closed are still noticed.
`tlm resume <id>` (or the button on the Last mile page, `POST /api/handoffs/{id}/resume`) hands it back to TLM:
the label goes, the round counter of the trigger that escalated starts again, and the handoff is re-read from
`pr-open`. `tlm cancel <id>` (`POST /api/handoffs/{id}/cancel`) ends the handoff as `cancelled`, freeing the
branch and leaving GitHub as it is.
A restart after a crash between push and PR creation adopts the open PR instead of opening a second.

Pages: `/lastmile` lists the handoffs (stacks indented, a banner and a red count on the nav tab when one needs
you); `/lastmile/{id}` is a handoff's page (event timeline, PR snapshot, reviewers editor, resume/cancel, the
merge verdict); `/lastmile/shadow` is the shadow merge report (verdict vs what the human did, signals per PR),
and `/lastmile/shadow?handoff={id}` or `?pr=owner/repo%23n` adds that PR's verdict history. The overview shows
a line when handoffs need you.

Reviewers: the bot (`lastmile.bot_reviewer`) is requested once the PR is ready (a PR the handoff didn't ask
to be a draft is marked ready); humans are requested once, at the first green CI, from `--reviewers` as
given, else `lastmile.reviewers` (per-repo override, `count`, round-robin skipping the PR author, the cursor
kept in `lastmile.json`), else nobody and a warning. After that GitHub decides: whoever a human removes is
never re-added, reviewers a human adds are left alone. `PUT /api/handoffs/{id}/reviewers` (a JSON list, or
`{"reviewers": [...]}`) replaces the human list: before the request it changes the plan, afterwards the
next tick applies the difference (naming someone anew re-adds them).

CI fixing: on red CI the snapshot gets each failed check with the tail (150 lines) of its failed job log
(`gh run view --job`; checks that aren't Actions jobs get none) and a failure fingerprint (job name plus a hash
of the error lines with timestamps, SHAs, numbers and temp paths removed). A failure whose log matches a
`lastmile.flakes` regex is rerun (`gh run rerun --failed`) up to `lastmile.flake_reruns` times per head SHA.
Otherwise, with `fix-ci` on, the fix agent runs in a container as a one-shot task of the review runtime: it
shares `review.max_concurrent` and the 5-hour budget with reviews, has no GitHub token, and is not run when
reviews are disabled (the handoff goes to `needs-human`). It runs beside the tick loop; the handoff shows
`ci-red` "fix agent running" meanwhile, and a restart loses a run in flight (the next tick may start another).
The agent gets the snapshot, failed log excerpts, the diff against `origin/<base>`, the title and body, and
is told: edit files only, no git, no gh, write `/out/result.json`:
`{"summary": str, "confidence": "low|medium|high", "escalate": bool, "escalate_reason": str, "body": str?, "body_reason": str?}`
(`body`: see below). Its edits then go through the host write path: `escalate` or no change →
`needs-human`; a change to `.github/workflows/**`, `**/migrations/**` or a lockfile (`uv.lock`, `poetry.lock`,
`package-lock.json`, `yarn.lock`, `pnpm-lock.yaml`, `go.sum`; exempt with `lastmile.allow_paths`), or more than
`lastmile.max_diff_lines` changed lines, or a binary file → the worktree is reset and `needs-human`; otherwise
`git add -A`, commit `TLM: <summary>`, `git push`, back to `ci-running`. Git hook configs (`.husky/**`,
`.pre-commit-config.yaml`, …) are protected too, and TLM commits and pushes with hooks off: nothing the agent
wrote runs on the host. The agent runs the repo's checks in its container; CI is the gate. Rounds are
counted per trigger and capped by `max_fix_rounds`; a failure whose fingerprint already had a fix round →
`needs-human` ("same failure twice").

Review feedback: every tick reads the PR's review threads (GraphQL), review bodies (`COMMENTED` and
`CHANGES_REQUESTED` with text) and conversation comments. Each author is marked human or bot: GitHub's
`Bot` type, a `[bot]` login, or a login in `lastmile.bot_logins` (default `EthycaBot`, a user
account that acts as a bot). What TLM itself posted is excluded by comment id (it posts as your login). A
cursor in the handoff (per thread the last comment id, per source the last review and comment id) makes
only items newer than what an agent was already handed "new"; the snapshot lists the unresolved threads,
the new items, TLM's reply count per bot thread and whether a human edited the PR body. With `address-reviews`
on, new items with CI not red start the review agent (`feedback`, "review agent running"); with it off, new
human comments → `needs-human` ("review feedback: n new comments") and bot-only feedback just shows in the
snapshot. The agent gets the snapshot, the new items with their full thread history (as data: comments are
untrusted), the diff and the PR body, is told to be critical (nitpicks, restatements and weak suggestions get
`skip` with a reason, not a reply), and returns the shared result plus
`"actions": [{"thread_id", "action": "fix|reply|defer|skip", "reply", "reason"}]`; ids it wasn't given are
dropped and logged. The host then commits and pushes any edits (one `TLM: <summary>` commit; a rejection →
`needs-human`, nothing posted), then posts the replies in one batch: `fix` → "Fixed in <sha>." plus its text,
`reply`/`defer` → the text, `skip` → nothing (kept in the handoff's event log). Inline threads are answered under
their first comment; review bodies and conversation comments share one PR comment linking what it answers. At
most 3 replies per bot-started thread, then the reply is logged, not posted. Threads are never resolved. A
`body` in the result updates the PR description only if it is still what TLM last wrote and keeps every `## `
heading; otherwise the proposal is posted as a PR comment (the CI fix agent's `body` works the same way).
After a push that fixed items of a reviewer whose latest review is `CHANGES_REQUESTED`, that reviewer is
re-requested unless a human removed them. Stops: `max_fix_rounds` review rounds, and a new human comment on a
thread TLM answered "Fixed in …" → `needs-human` ("reviewer re-raised a fixed thread"); `tlm resume` resets the
review counter. Every handoff keeps a log of its last 100 events (PR opened, state changes, agent runs,
commits, replies posted or skipped, bot-cap hits, body updates) in its JSON as `events`.

Stacks (`--stack-parent <id|branch>`): the parent is a handoff of the same repo (by id, or by branch among the
open ones); the child's PR base is the parent's branch and it opens right away. A branch nobody hands off is
just a plain base. Cycles and a parent in another repo are refused. TLM notes the parent's head while it is
open. When the parent merges the child gets `stack-parent-merged`, its PR is retargeted to the parent's own
base (`gh pr edit --base`) and rebased with `git rebase --onto origin/<base> <parent's last head> <branch>`
(that survives a squash merge) and pushed with `--force-with-lease`. A parent that was closed or cancelled
sends the child to `needs-human` ("stack parent closed"). The Last mile page shows a stack under its root,
children indented, "waiting on <parent>" while the parent is open.

Base drift: TLM rebases only when GitHub says the PR is `BEHIND` or `DIRTY` (or `CONFLICTING`) and the base
really moved, never merely because it did. `git merge-tree --write-tree` predicts the result: no conflict →
rebase onto `origin/<base>`, force-push with lease, back to `ci-running`. A conflict → state `conflict`: with
`fix-ci` on (conflicts count as CI-like), the host starts the rebase and the conflict agent resolves the
files each stop leaves (`git diff` of the markers, both sides, the replayed commit); without it `needs-human`
("rebase conflict: <files>"). The host checks the files (no `<<<<<<<`, `=======`, `>>>>>>>` line, only the
conflicted files edited, no protected path, nested repo or link), `git add`s them and runs
`rebase --continue`, up to `max_fix_rounds` agent runs (`tlm resume` resets them). An escalation, a refused
resolution or a failure aborts the rebase and puts the worktree back at its head → `needs-human`. Every git
command of a rebase runs with hooks off, and a rebase found in progress with no run behind it (a crash) is
aborted on the next tick. Retargets, rebases, conflict-agent runs and force-pushes go in the handoff's event log.

Whatever an agent leaves behind on any exit (a failed run, an invalid result, an escalation) is cleaned: new
gitignored files are removed, changed ones are named in the reason (size, mtime, mode, inode and ctime are
compared), and the tree goes back to the head it started at. A nested git repository or `.git` entry is refused.

Shadow merge (`merge=shadow`; `merge=off` is the default, `merge=auto` is refused): TLM never merges, but on
each tick of an open, ready PR it asks the merge policy "would you merge this?" and keeps the verdict on the
handoff (`shadow`, shown for `merge=off` handoffs too). The policy says yes when there is a current approval from
a human (not a bot) and GitHub's decision isn't `CHANGES_REQUESTED`/`REVIEW_REQUIRED`, no approval is stale, all
checks are green on the head, no review thread is unresolved, the base is up to date (not behind, not
`BEHIND`/`DIRTY`), no changed file matches `lastmile.risky_paths`, additions plus deletions are within
`lastmile.max_merge_lines`, and now is outside every `lastmile.merge_freeze` window. Each verdict lists its
blockers and the signals it saw (diff size, risky paths, fix rounds per trigger, who approved and whether they are
bots, time in review since the PR was first ready, flake reruns, agent commits, unresolved threads, commits
behind, stale approvals). For `merge=shadow` handoffs a verdict is appended to `lastmile-shadow.jsonl` next to
`lastmile.json` when it or its blockers changed, else at most once an hour; when a human merges or closes the
PR an outcome line records what happened next to the last verdict. `GET /api/lastmile/shadow` reads the log:
totals, an agreement matrix (last verdict yes/no against merged/closed/open), the mean time in review of merged
PRs and a row per PR; `?pr=owner/repo%23n` or `?handoff=<id>` narrows it to one PR and adds its verdict history.
A corrupt log line is skipped. A merge freeze window is `{"weekday": "friday", "start": "15:00", "end": "23:00",
"tz": "Europe/Berlin"}` (`weekday` optional, `end` after `start`: split a window across midnight in two).

```
tlm handoff --worktree <dir> --base dev --title "…" --body-file pr.md --jira ENG-1234 \
  --labels a,b --reviewers alice,bob --draft \
  --autonomy fix-ci,address-reviews,merge=off|shadow \
  --notify '<command template>' --notify-on merged,needs-human,closed \
  --agent claude:opus:medium --max-fix-rounds 3 --stack-parent <id|branch> --requester <who>
tlm status <id> [--json]
tlm wait <id> [--until merged|needs-human|closed|<state>[,…]]
tlm resume <id>
tlm cancel <id>
tlm install-skill [--for claude,codex]
```

`tlm install-skill` installs or updates the `handing-off-to-tlm` agent skill (`skills/handing-off-to-tlm`,
built into the binary) in `~/.claude/skills` and `~/.codex/skills` (`CLAUDE_CONFIG_DIR` and `CODEX_HOME`
override them). Run it again after `make build` to bring the skill up to date.

`tlm` is `sisyphus tlm` (`make build` links `bin/tlm`); it talks to the running app at `http_addr`
(`--addr` overrides it). `handoff` prints the id; everything afterwards is keyed by it. A handoff is
refused (400, with the reason) unless the worktree is clean (untracked files don't count), its branch
has commits ahead of `origin/<base>`, its repo is in `review.repos`, and no open handoff owns the
branch. `merge=auto` isn't supported yet. `wait` polls every 15s and exits 0 at one of the `--until`
states or when the handoff is merged or closed.

`--notify` is a Go `text/template` run through `sh -c` in the worktree on the `--notify-on` events
(`pr-opened`, `merged`, `needs-human`, `closed`, `stack-parent-merged`). Fields: `ID`, `Event`, `Repo`,
`Branch`, `PRURL`, `PRNumber`, `State`, `Reason`, `Requester`; each is shell-quoted when rendered, and also
in the environment as `TLM_ID`, `TLM_EVENT`, …. A failing command is logged, never fatal.

API: `POST /api/handoffs` (JSON body with the flags' names in snake_case; 201 with the handoff),
`GET /api/handoffs`, `GET /api/handoffs/{id}`, `GET /api/lastmile/shadow`, `PUT /api/handoffs/{id}/reviewers`,
`POST /api/handoffs/{id}/resume`, `POST /api/handoffs/{id}/cancel`. `tlm` sends the API token only on
POST/PUT requests to a loopback address. The dashboard lists the
handoffs on its Last mile page (`/lastmile`). Handoffs are kept in `lastmile.json` next to the state file.

| Field | Default | |
|---|---|---|
| `lastmile.disabled` | `false` | no handoffs |
| `lastmile.interval` | `2m` | between ticks over the open handoffs |
| `lastmile.bot_reviewer` | `EthycaBot` | requested on every PR once it's ready |
| `lastmile.reviewers` | `{"roster":[],"count":1}` | human reviewers when a handoff names none |
| `lastmile.repo_reviewers` | `{}` | per `owner/name`: replaces `reviewers` for that repo |
| `lastmile.max_fix_rounds` | `3` | default of `--max-fix-rounds` |
| `lastmile.needs_human_label` | `tlm:needs-human` | label of a PR that waits for a human |
| `lastmile.flakes` | `[]` | regexes: a failed job whose log tail matches is rerun, not fixed |
| `lastmile.flake_reruns` | `2` | flake reruns per head SHA |
| `lastmile.max_diff_lines` | `400` | an agent change over this many lines is discarded |
| `lastmile.allow_paths` | `[]` | globs an agent may change although the denylist names them |
| `lastmile.bot_logins` | `["EthycaBot"]` | user accounts whose review feedback counts as bot feedback |
| `lastmile.risky_paths` | migrations, auth, `.github/**`, Dockerfiles, `infra/**`, `*.tf` | globs: touching one blocks a would-merge verdict |
| `lastmile.max_merge_lines` | `400` | additions plus deletions above this block a would-merge verdict |
| `lastmile.merge_freeze` | `[]` | windows `{weekday?, start, end, tz}` in which a PR would not be merged |

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
| `internal/lastmile` | the-last-mile: handoffs, their store, notify |
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
