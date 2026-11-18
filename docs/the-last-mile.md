# the-last-mile (TLM): design

> Codename, temporary. Status: **draft**, iterating. Owner: René.

## Problem

Agents doing implementation work also do the last mile of a PR themselves: opening it, polling CI,
reading and answering review threads, restacking and merging. In my own Claude Code sessions
(Oct 8 – Nov 4, 2026, not counting reviews of other people's PRs) that phase caused 544 permission
prompts:

| Phase | Prompts | Share |
|---|---|---|
| Merging / restacking | 150 | 28% |
| Watching / fixing CI | 138 | 25% |
| Opening / editing the PR | 113 | 21% |
| Addressing review feedback | 89 | 16% |
| Development and investigation | 54 | 10% |

- 90% of the prompts happen after the code is written.
- 72% come from subagents rediscovering PR state with their own `gh` calls.
- Four multi-PR orchestration sessions produced 79% of them.
- Most of them are triggered by the org policy's `ask` rule on `Bash(gh api*)`, which local allow
  rules can't override.

The polling, reading and writing is mechanical. Only a few moments need judgment: a CI failure that
isn't a flake, a review comment, a rebase conflict.

## Idea

Split the last mile off the implementing agent:

1. An agent (possibly an orchestrator) does the implementation and local reviews.
2. When the work is ready for a PR, it **hands the branch/worktree to TLM** and is done.
3. TLM opens the PR, watches CI and reviews, and **starts an agent in a container only when
   judgment is needed**, giving it a snapshot of the state.
4. On top of that, we experiment with automated merge heuristics.

The principle: **the harness owns GitHub (reads, writes, polling, state). Agents get a snapshot and
return a decision or an artifact.** Agents never run `gh`.

## 1. Where it lives: a mode in sisyphus, not a fork

TLM is a new mode inside the sisyphus repo (`internal/lastmile`, next to `internal/tracker`). Fork
later only if the two diverge.

- **One budget.** Reviews and fixes draw on the same subscription window and the same Docker host.
  Two daemons would compete for `review.five_hour_percent` and `review.max_concurrent` without
  knowing about each other.
- **Reused as-is:** `internal/github`, `internal/container`, `internal/worktree` (with its lock),
  local stacks, run records, re-attach after restart, retries, the dashboard, evals, the hook runner.
- **No collisions:** sisyphus reviews teammates' PRs and TLM handles mine, so they never act on the
  same PR.

## 2. Handoff contract

The implementing agent runs one command (or `POST /api/handoffs` on the loopback API) and is done:

```
tlm handoff \
  --worktree ~/Dev/.worktrees/fidesplus/ENG-1234 \
  --base dev [--stack-parent <handoff-id|branch>] \
  --title "…" --body-file pr.md --jira ENG-1234 \
  --labels … [--reviewers a,b] --draft=false \
  --autonomy fix-ci,address-reviews,merge=shadow \
  --notify '<command template>' --notify-on merged,needs-human,closed \
  --agent claude:opus:medium --max-fix-rounds 3
```

**Before accepting, TLM checks:**
- the worktree is clean: no tracked changes. Untracked files are allowed (wtp hooks copy untracked config,
  like a `.claude` symlink, into new worktrees) and recorded as the handoff's `untracked`. They are the human's
  for good: every "clean" check (agent runs, rebases, the dirty panel) leaves them out, no commit (agent,
  panel, "agent" action) includes them, stash and cleanup resets leave them in place, they aren't sent to the
  assessment model, and an agent changing one is held against its run like a gitignored file
- the branch has commits ahead of `--base`
- the repo is in `review.repos`, so an image and setup exist
- no other handoff already owns the branch

**Ownership moves to TLM.** It takes the worktree lock, and the handing-off agent must not touch the
worktree again. The handoff returns an id, and everything afterwards is keyed by it.

## 3. Closing the loop with an orchestrator

The orchestrator should not block or poll.

- **Callback (main path).** `--notify` is a command template TLM runs on chosen events, using the
  existing hook runner. Examples: `orca orchestration send --to {{.Requester}} …`, a T3 thread
  message, a Slack DM. Events: `pr-opened`, `merged`, `needs-human`, `closed`,
  `stack-parent-merged`.
- **Pull.** `tlm status <id> --json` and `tlm wait <id> --until merged|needs-human`, for scripts that
  really want to block.
- **Stacks.** With `--stack-parent` (a handoff id, or a branch), TLM retargets and rebases children itself when the parent
  merges (`git rebase --onto origin/<new base> <parent's last head>`, so a squash merge is fine). The orchestrator hands off the whole stack once and never manages bases again.

What the orchestrator gets back is a terminal outcome (merged, or needs-human with a reason and a
link), not a stream of CI noise.

## 4. Lifecycle state machine

One per handoff:

```
handed-off → pr-open → ci-running ─┬→ ci-red ──[fix agent]──→ ci-running
                                    └→ ci-green → awaiting-review
awaiting-review ─┬→ feedback ──[review-response agent]──→ ci-running
                 ├→ approved → mergeable? ──[merge policy]──→ merged ✔
                 └→ conflict ──[rebase; agent only on conflict]──→ ci-running
any state ──(budget/rounds exhausted, repeated failure, policy)──→ needs-human ⏸
closed by a human ──→ closed ✖
```

**What the harness does on every tick, with no agent involved:**
- builds the PR snapshot: head, checks, failed job logs, threads since the last cursor,
  mergeability, base drift
- reruns known flakes within a budget
- rebases when the base moved and `merge-tree` reports no conflict
- moves the PR from draft to ready
- requests reviewers (once, see §6) and re-requests after addressing their feedback

**An agent starts only in these cases:**

| Trigger | Agent gets | Agent returns |
|---|---|---|
| CI red (not a known flake) | snapshot, failed log excerpts, diff | edits in the worktree + `{summary, confidence, escalate?}` |
| New review threads | snapshot, threads, diff, current PR body | edits + `[{thread_id, action: fix\|reply\|defer\|skip, reply}]` + optional `{body, reason}` |
| Rebase conflict | conflicted files, both sides | resolved files, or `escalate` |
| Merge decision (later) | snapshot | advisory verdict only |

### PR snapshot

The one read model everything uses, built by the harness and given to agents as input:

- head SHA, draft and ready state, `mergeable` / `mergeStateStatus`
- review decision, approvals and whether they're stale (commits after the approval)
- review threads, unresolved and new since the cursor, each author marked human or bot, with
  TLM's reply count per bot thread
- PR body, and whether a human edited it since TLM last wrote it
- requested reviewers: who TLM requested, who's currently requested, who was removed
- check runs, plus the log tail of each failed job, with a failure fingerprint
- base branch, how far behind it is, stack parent and children

## 5. Write safety

Keep sisyphus's model (the container never touches `.git`) and add a controlled write path.

- **No git inside the container.** The agent edits files in the mounted worktree, and `.git` stays
  read-only.
- **The host commits and pushes, after checks:**
  - a path denylist: `.github/workflows/**`, migrations unless allowed, lockfiles unless allowed
  - a diff-size cap
  - the repo's fast lint and tests (optional)
  - commit messages generated from the agent's summary, with a `TLM:` prefix
- **Gitignored files are the agent's only from setup on.** The container runs the repo's setup
  (`uv sync`, `npm install`, …) before the agent, and those installs write ignored files into the
  worktree (npm workspaces' nested `node_modules`). The host snapshots ignored files once setup has
  finished (the container waits for it) and adds what setup left under `node_modules/` to the
  baseline. Everything else setup writes is still held against the run: setup runs the branch's
  install scripts, which an earlier agent round may have changed. A reason names at most 20 paths and counts the rest.
  The agent is asked to run the repo's checks, so a repo's `env` sends the checks' caches out of the worktree
  (fidesplus: `RUFF_CACHE_DIR`, `MYPY_CACHE_DIR`, `COVERAGE_FILE` under `/tmp`, pytest's cache plugin off,
  no `.pyc`); a cache a repo's env misses still fails the run.
- **One writer per worktree, across processes.** The worktree lock is an in-process lock plus an
  flock under the state dir, so two sisyphus processes on the same state (a smoke-test binary next
  to the LaunchAgent) don't run agents in the same worktree at once. On top of that, a second
  sisyphus on the same state dir refuses to start (`sisyphus.lock`), since every instance rewrites
  the whole state files; running several instances is reneruck-ethyca/sisyphus#1.
- **A run in flight is marked on the handoff** (`run`: trigger, start, starting head), stored when the run
  starts. The run itself lives in memory, so a mark with no run behind it means a restart lost the run, and the
  next tick cleans up as after a failed run: a lost conflict run's rebase is aborted (any other rebase in
  progress is a human's, resolving a needs-human conflict, and stays), the tree goes back to the starting head
  unless a commit moved it on, and a lost dirty-agent run's WIP commit is unwrapped. Gitignored files whose
  ctime is past the run's start are named in a needs-human reason but not removed: without the pre-run stamps a
  planted file can't be told from a changed one. Only `node_modules/` files changed before the container's setup
  finished are exempt (the mark records `setup_done` when it does; unrecorded, none are), and a check that
  fails escalates too. With nothing found, the handoff just carries on. A lost fix round's failures don't count as seen (they're recorded when a round ends).
  A finished run's outcome stays in memory until the tick that applies it is stored: a tick discarded because
  the handoff changed meanwhile (an assessment, a dashboard action) leaves it for the next tick to apply.
- **Replies are posted in one batch from the host.**
- **Bot comments get the same treatment as human ones, with a cap.** TLM replies to review bots
  (sisyphus, ethycabot, others) but at most 3 replies per bot thread; after that it stops replying
  in that thread and logs it. The agent is told to be critical: nitpicks, restatements and
  suggestions it disagrees with on weak grounds get `skip` (no reply posted, reason logged), not a
  polite acknowledgement. Only real fixes and real disagreements get a reply.
- **PR body.** The review-response and fix agents may return a new body with a reason when their
  changes alter what the PR does (scope, approach, steps to confirm). The host applies it with
  `gh pr edit`, keeps the repo's template sections, and logs the reason. If a human edited the
  body since TLM last wrote it, TLM doesn't overwrite it and posts the proposed change as a PR
  comment instead.
- **Thread resolution stays with reviewers.** TLM replies "fixed in `<sha>`" and leaves resolving to
  the reviewer, at least at first.
- **Stopping rules:**
  - `--max-fix-rounds` per trigger type
  - a failure seen twice with the same fingerprint (job plus error signature) → needs-human; a fingerprint
    counts once a fix round on it has ended
  - a reviewer re-raising a thread TLM already "fixed" → needs-human
- **Escalation:** a `tlm:needs-human` label, the `needs-human` notify event, and a dashboard banner.
- **Dirty worktree panel:** while a handoff is needs-human, each tick checks its worktree with `git status
  --porcelain`. On a new status sisyphus lists the files itself, on that same tick: path, porcelain code
  (M, A, D, R, ??), and `+added −removed` from `git diff --numstat HEAD`, or for an untracked file its line
  count, binary when its first 8 KB hold a NUL; untracked directories are expanded (`-uall`), at most 500
  rows. A clean worktree or `resume` clears it. A stored `dirty` with no `files` (the earlier, options-based
  format) counts as new and is rebuilt.

  No model is in the action path. The page shows a files-first panel in place of the needs-human notice and
  the head's Resume/Cancel buttons (these become links at its foot): a table of the files, each ticked by
  default, each path expanding to its diff; a commit message box; and three actions, each taking the status
  hash the page was rendered for and answering 409 if the worktree's status has moved on since:
  - `POST /api/handoffs/{id}/dirty/commit {hash, paths, message}`: every path must be a listed file, at least
    one; the message is trimmed, non-empty, at most 2000 bytes. Runs the write path's checks (protected
    paths, nested repos, symlinks, binaries; not the line cap, it's the human's work) before staging anything,
    adds and commits only those paths with hooks off, pushes, resumes. Unlike an agent's commit it never resets
    the worktree: a failure unstages, or uncommits after a failed push, and leaves the files as they were.
  - `POST …/dirty/stash {hash}`: `git stash push -u -m "tlm: <branch> WIP"`. No resume; the page then offers
    the usual Resume.
  - `POST …/dirty/leave {hash}`: sets `dismissed`, collapsing the panel to a one-line banner until the status
    changes.
  - `POST …/dirty/agent {hash, message, task}`: hands the dirt to an agent. Every listed file is committed
    locally with the message (the same checks as commit, not pushed), and a fix-agent run starts from that
    "WIP commit" with the human's task (empty: finish it, make the tests pass, drop what doesn't belong). The
    agent's prompt shows the WIP commit, the branch's diff and the task. Since the run starts at the WIP
    commit, the usual write path only judges the agent's own edits. Pushed (both commits, or the WIP alone if
    the agent changed nothing) → the handoff resumes. Otherwise the WIP commit is unwrapped (`reset --mixed
    HEAD~1`, only while it's the unpushed head), the changes are uncommitted again, and the reason says so.
    `dirty.wip` records the commit for the run's length and survives the panel's rebuilds; a run lost to a
    restart is cleaned up like any lost run (above), which unwraps it. While the run lasts the page shows
    "agent running" in place of the panel.

  `GET …/dirty/diff?path=` (path must be a listed file) is `git diff HEAD -- path` for a tracked file, the
  content of an untracked regular file, cut at 200 KB, or "binary file". Actions run under the worktree lock
  and each is a `dirty` timeline event. Like every API call, they need the API token (see the revision log, 2026-11-12).

  Haiku only writes text, off the tick, once per distinct status (`claude -p --json-schema`, no tools, no
  settings): the porcelain list, the last five commits, `git diff HEAD` (cut at 40 KB) and small untracked
  text files (regular files only, ≤8 KB each, ≤20 KB in all) go in, marked as data, not instructions. It
  answers `{summary, message}`: ≤60 words on what the changes are and a ≤72-character commit subject, which
  go into the stored `dirty` only while its status is still the one assessed, leaving the rest of it (`dismissed`,
  `wip`) as it is by then. The
  panel works without it: "Analysing…" while it runs, "Couldn't summarise" on an error. Its subject fills the
  message box only when the human hasn't typed in it; the draft and the ticks survive the page's reloads in
  `sessionStorage`, keyed by handoff and status hash.

## 6. Requesting reviewers

**Who.** Every TLM PR gets `ethycabot` (a GitHub user account, so it can be requested directly).
Human reviewers come from the first source that has any:

1. `--reviewers` on the handoff. These replace the roster and are applied as given: no count, no
   filtering.
2. the roster: a global `lastmile.reviewers.roster: [alice, bob, …]` with
   `lastmile.reviewers.count: 1`, optionally overridden per repo. TLM picks round-robin, skipping
   the PR author, and keeps the cursor in its state store. Humans already on the PR (requested, or
   with a standing review) count toward the count and aren't picked again, so an adopted PR is only
   topped up.
3. nobody, with a dashboard warning (not when the PR already has humans)

**When.** ethycabot is requested when the PR is ready for review. Humans are requested once, when CI
first goes green, so nobody is pinged on a red PR. After that, **GitHub is the source of truth**:
- TLM records the set it requested.
- Someone a human removed on GitHub is never re-added.
- Reviewers a human added are left alone.
- After TLM pushes a commit that addresses a reviewer's "changes requested", it re-requests that
  reviewer.

**Overrides.** The dashboard can edit a handoff's reviewers. Before the initial request this replaces
the planned set; afterwards TLM applies the difference on GitHub. Editing reviewers on GitHub works
too, by the rule above.

## 7. Merge heuristics: start in shadow mode

1. **`merge=off` (default).** TLM gets the PR to green and approved, notifies, and stops.
2. **`merge=shadow`.** Every tick logs `would-merge: yes/no + signals`, and when I merge by hand TLM
   records what I did. The comparison is the dataset.
3. **`merge=auto`** for a rule set that has earned it, for example:
   - required approvals present, none stale
   - all required checks green on the current head
   - zero unresolved threads
   - base up to date
   - no risky paths touched (migrations, auth, infra)
   - diff under N lines
   - outside a merge-freeze window

Signals to collect from the start, even before they're used: diff size, touched-path risk, number of
fix rounds, reviewer identity (human or bot), time spent in review, CI flake count.

## 8. Milestones

| M | Scope | Agent? | Done when |
|---|---|---|---|
| **M0** | `handoff` command, state store, open the PR (template, Jira, labels, reviewers incl. ethycabot and roster), snapshot, dashboard rows, `notify` | No | An orchestrator hands off a branch, the PR opens, and the callback fires on merged/closed |
| **M1** | CI watch, flake rerun, fix-agent container with host-side commit and push, round limits, needs-human | Yes | A seeded CI failure gets fixed without a human, and a repeated failure escalates |
| **M2** | Review inbox with a cursor, review-response agent, batch replies, bot-thread cap, PR body updates, re-requesting reviewers | Yes | A review round turns into one commit plus replies |
| **M3** | Stacks: parent-merged handling, retarget, rebase, conflict agent | On conflict | A 3-PR stack lands bottom-up with no orchestrator involved |
| **M4** | Merge policy in shadow mode, signal logging, comparison report | No | Two weeks of shadow data, then pick the first auto rule set |
| **M5** | `merge=auto` for that rule set | No | — |

## 9. Decisions

1. **Org policy: cleared.** TLM writing to GitHub as me from outside Claude is fine.
2. **Identity: my `gh` login**, same as sisyphus. A bot account or GitHub App is a later change.
3. **Bot reviewers: reply, capped at 3 replies per thread, and be critical** about what deserves a
   reply (§5).
4. **PR body: TLM may update it** when its changes warrant it (§5).
5. **Repos: fidesplus and fides-qa only.** Infra repos come later, with their own setup.

6. **Roster selection: round-robin.**
7. **Roster scope: global, with optional per-repo overrides.**
8. **Human reviewers are requested after CI first goes green.**
9. **`--reviewers` replaces the roster** and is applied as given.

## 10. Open questions

None right now.

## Revision log

- **2026-11-04:** First draft, from the permission-prompt analysis and a brainstorm of harness tasks.
- **2026-11-05:** Resolved the five open questions (now §9): policy cleared, my `gh` login as
  identity, reply to bots with a 3-reply cap per thread and a `skip` action, TLM may update the PR
  body (but not over a human's edit), fidesplus and fides-qa only. Added §6 on requesting reviewers
  (ethycabot always, then `--reviewers` or a global roster, GitHub as source of truth after the
  initial request, dashboard override). Extended the snapshot and the review-agent contract to match.
  New open questions on roster selection, scope and timing.
- **2026-11-05:** Resolved the reviewer questions (§9, items 6–9): round-robin from a global roster
  with optional per-repo overrides, humans requested after the first green CI (ethycabot still at
  ready), `--reviewers` replaces the roster and is applied as given.
- **2026-11-05:** M1 implemented. Refinements: agent runs are goroutines per handoff and only the tick writes
  handoffs (a run leaves its outcome for the next tick), `FixRounds` is per trigger, fingerprints already
  fixed are kept in the handoff, flake reruns also apply with `fix-ci` off. Added a `cancelled` terminal
  state, `resume`/`cancel` (API, CLI, dashboard), and `lastmile.{flakes,flake_reruns,max_diff_lines,
  allow_paths,repo_checks}`. Agent tasks run through `review.Reviewer.RunTask` (shared slots and budget,
  no GitHub token, no stack yet).
- **2026-11-05:** M2 implemented. Refinements: the cursor is ids (per thread, per review source, per
  conversation-comment source), not timestamps; TLM's own comments are excluded by recorded id because it posts
  as the user's login; bot status also comes from `lastmile.bot_logins` (ethycabot and sisyphus are user
  accounts). Review bodies and conversation comments are answered with one shared PR comment that links
  what it answers; inline threads under their first comment. A `fix` action without a pushed change posts
  nothing but its text. The bot cap counts only bot-started inline threads. New feedback with `address-reviews`
  off escalates only for human comments; the cursor moves on escalation by feedback rules (re-raise, off) but not
  when the round cap or a missing runtime stops it, so a resume retries those items. The body guard compares
  line endings and edge whitespace loosely and applies to the CI fix agent's `body` too. Every handoff keeps a
  bounded event log (100). Security follow-ups for all agent kinds: cleanup on every exit after a run, inode
  and ctime in the ignored-file stamp, nested repos and directory entries refused.
- **2026-11-05:** M3 implemented. `--stack-parent` resolves at handoff time (id, else branch among the repo's open
  handoffs, else a plain base) and stores the parent's id; the child's `Base` is the parent's branch until the
  parent merges, and `ParentHead` (the parent's head while open) is the old base point for `rebase --onto`. Drift
  rebases need GitHub's BEHIND/DIRTY/CONFLICTING plus a real local behind-count; `merge-tree` only decides between a
  background rebase and the conflict path. The conflict agent is gated by `fix-ci` (conflicts are CI-like, no new
  autonomy flag) and its rounds are `FixRounds["conflict"]`. All rebases run in one background run per handoff under
  the worktree lock, hooks off; the host checks a resolution (markers, only conflicted files, protected paths,
  nested repos, links) and continues, else aborts back to the pre-rebase head. A rebase left in progress without a
  run is aborted on the next tick. Retargeting reuses `EditPR` (it already carried `--base`); no new GitHub method.
- **2026-11-05:** M4 implemented. Refinements: the verdict is kept on every handoff (`merge=off` too, for the
  UI) but logged only for `merge=shadow`, and refreshed only on a change or hourly (so a quiet PR neither grows
  the log nor rewrites the store). "Approval" means a current non-bot approval. Freeze windows don't cross
  midnight (rejected at load; use two). Time in review starts when the PR is first seen ready. New config
  `lastmile.{risky_paths,max_merge_lines,merge_freeze}`, `GET /api/lastmile/shadow`, outcome lines for merged and
  closed PRs carrying the last verdict and whether it was about the merged head.
- **2026-11-05:** M0 implemented, on branch `feat/the-last-mile`. Scope of this round: M0–M4; M5
  (`merge=auto`) stays out until the shadow data exists, and a handoff asking for it is refused. The handoff
  API takes `--requester` for the notify template; notify values are shell-quoted and also passed as `TLM_*`
  env vars. Creating a handoff needs a token (`lastmile.token`, 0600, in the state dir; `tlm` sends it, and only
  to loopback): review containers can reach the loopback API, and a handoff carries a shell command. Remotes
  must be on github.com, the PR template is read from git (not the file system), and a restart adopts only a
  same-repo PR for the branch. A needs-human handoff comes back with `resume`.
- **2026-11-05:** Write safety hardened beyond §5, after security reviews: nothing an agent writes runs on the
  host. TLM commits, pushes and rebases with git hooks off, and hook configs (`.husky/**`,
  `.pre-commit-config.yaml`, …) are protected paths. The host-side "repo's fast lint and tests" step is dropped
  (`lastmile.repo_checks`, listed in the M1 entry above, was removed again): it would run agent-edited code on
  the host, so the agent runs the checks in its container and CI is the gate. An agent run that creates or
  changes gitignored files is rejected (new files removed), and binary changes are refused (no line count to
  cap). `lastmile.bot_logins` defaults to `ethycabot` only: sisyphus posts as its user, so "sisyphus" (in the M2
  entry above) isn't a bot account. A stack child records its parent's head only while its branch contains it,
  so a parent's own rebase doesn't move the child's fork point.
- **2026-11-05:** UI: the Last mile is its own tab (not a section of the overview), with the needs-human banner,
  the handoff list (stacks indented) and the only link to the shadow merge report; the overview keeps a "N
  handoffs need you" line. New pages: a handoff page (`/lastmile/{id}`: timeline, snapshot, reviewers editor,
  "would merge" with a link to its shadow history) and the shadow merge report (`/lastmile/shadow`). Built from
  a Claude Design mock; left out for now: list filters, stack group headers, the per-PR "verdict vs what you
  did" notes and a JSONL export.
- **2026-11-05:** Dashboard: `/` is now a landing page over both modes (needs you, reviews, last mile, budget and
  run slots, shadow merge headline); the review list moved to its own Reviews tab (`/reviews`). The status now
  carries the last-mile agent runs in flight and the run slots in use (reviews and agent tasks share
  `max_concurrent`).
- **2026-11-05:** Agent guidance: a `handing-off-to-tlm` skill (when to hand off, flags, `--notify` with the
  `$TLM_*` env vars, stacks, what to do on needs-human) ships embedded in the binary, and
  `tlm install-skill [--for claude,codex]` installs or updates it in `~/.claude/skills` and `~/.codex/skills`.
  `tlm resume` help now says it hands the handoff back to TLM.
- **2026-11-06:** Merged into main (fast-forward to `1f8e8e4`, rollback tag `pre-last-mile-merge`).
- **2026-11-06:** `tlm install` (`make install`): asks where to put `tlm` (default: the first writable bin dir
  on PATH) and symlinks it to the built `bin/sisyphus`, so rebuilds need no reinstall; then offers the skill
  to Claude Code and Codex when their CLI or config dir is present.
- **2026-11-06:** The dashboard flags an installed handoff skill that differs from the binary's copy (every
  page, under the top bar) and updates it on a click (`POST /api/skill/update`). It compares with the embedded
  skill on each render instead of storing a hash; agents without the skill aren't offered it.
- **2026-11-06:** The skill check runs once at startup and sets an in-memory marker the pages read (the
  update clears it), instead of reading the skill files on every render: the skill only changes with the binary.
- **2026-11-06:** The skill update moved to the Settings page ("Agent skill" card); when one is available the
  Settings tab shows a dot instead of a notice on every page.
- **2026-11-06:** The Settings page's "Agent skill" card lists each agent found (or holding the skill) with
  its state and Install / Update / Uninstall buttons. Actions run in the background (`POST
  /api/skill/{agent}/{install|uninstall}`, `GET /api/skill`); the open page follows them, other pages pick the
  result up on their next load. The marker is re-read after each action.
- **2026-11-06:** Handing off a branch that already has an open PR: the PR is adopted as before, and
  now (a) its existing reviews, comments and threads are history, not feedback: the review cursor
  starts past them, and (b) humans already requested or with a standing review count toward the
  roster count. Adopting still marks a draft PR ready unless the handoff asked for `--draft` (kept
  on purpose). Title, body and labels of an adopted PR are left as they are.
- **2026-11-06:** Gitignored-file check moved after container setup. A review round on fidesplus#7136
  went to needs-human because the setup's `npm install` in `clients/` rewrote workspace `node_modules`
  (about 15k files), which counted as the agent's change and were deleted. The snapshot is now taken
  when setup reports done; the container waits for it. Reasons list at most 20 paths (the full list
  had made the handoff 2.4 MB). The worktree lock gained an flock under the state dir after several
  sisyphus processes ran overlapping agents on one worktree.
- **2026-11-08:** A second sisyphus on the same state dir now exits at startup (flock on
  `<state dir>/sisyphus.lock`; `-shell` and `-eval` are exempt). Running several instances is tracked
  in reneruck-ethyca/sisyphus#1.
- **2026-11-08:** The post-setup baseline covers only `node_modules/`, after a security review: taking
  setup's writes wholesale would let an install script an agent committed earlier plant `.envrc` or
  `.venv/bin` files unflagged. `node_modules/.bin` stays trusted; protecting `package.json` scripts
  would close that.
- **2026-11-09:** Advice on dirty worktrees (§5, "Dirty worktree advice"). fidesplus#7136 sat in needs-human
  with "worktree has uncommitted changes", and the page gave no hint what the changes were or that `resume`
  would only bounce back. Handoffs gain `dirty` (status, advice, error); the assessment runs off the tick,
  host-side, once per distinct `git status`, and only for needs-human handoffs. The handoff page also says
  what `resume` does, and the timeline shows state changes as coloured chips.
- **2026-11-09:** Dirty worktree advice becomes options the human can run (§5). Free-text advice told people
  which git commands to type, built from an untrusted diff. `dirty` now holds `{summary, kind, options}` from
  a JSON schema; the model only picks an action from a closed set and its parameters, sisyphus validates
  them, builds the command and runs it on a click, with a status hash so a changed worktree gets 409 rather
  than a stale action. Human commits get their own path, `commitHuman`, because `commitAgentWork` resets the
  worktree on a refusal, which would destroy the work the human wanted committed. `changedLines` takes a
  pathspec so the binary check can cover just the files being committed. Small untracked files now reach
  the model too; Haiku couldn't see them before. No `agent` action yet, and the endpoint has no token, like
  resume and cancel.
- **2026-11-09:** The dirty-worktree options give way to a deterministic, files-first panel (§5). The model
  no longer picks actions: sisyphus lists the files and line counts itself and offers commit (of the ticked
  files, with the human's message), stash all, and leave it; Haiku only writes an optional summary and a
  suggested commit subject. Why: no model in the action path, so nothing it writes can steer what runs; the
  panel appears on the tick that sees the dirt instead of after the model answers; and the option schema,
  its validation, the gitignore and discard actions and the command previews go, which makes the code
  smaller. `dirty` gains `files` and `message` and loses `kind` and `options`; the action endpoint becomes
  `dirty/commit|stash|leave` plus `dirty/diff`, all keyed by the status hash.
- **2026-11-09:** A tick waits while GitHub hasn't caught up with a push. Committing from the dirty-worktree
  panel on fidesplus#7136 pushed `56b56f1`, Resume kicked a tick four seconds later, GitHub still reported
  `f3f1233`, and the review agent started against it and escalated ("worktree is at …, the PR at …"). Now,
  when the worktree's HEAD equals `origin/<branch>` and descends from the PR's head, the tick does nothing
  and tries again next time. Any other mismatch still escalates.
- **2026-11-09:** Dirty worktree → agent (`dirty/agent`). Cleaning up a worktree by hand was the only way out
  of "uncommitted changes" short of committing untested work. The agent needs a clean tree, because
  `runAgent` counts everything that differs from HEAD as the agent's and resets to it on failure; so the
  human's changes become a local WIP commit first and the run starts from there, and a failure unwraps it.
  The run uses the `dirty` trigger; it isn't counted against `max_fix_rounds`, since a human starts each one.
  `Resume`'s state reset is shared with the run's success path (`unescalate`). Model and container are the
  handoff's agent, as for fix rounds. Token auth on the dashboard's actions stays out of scope.
- **2026-11-12:** Only TLM's own rebases are aborted (§5). Every tick with no run in memory ran `rebase --abort`,
  so a human resolving a needs-human "rebase conflict" with `git rebase origin/<base>` lost it within two minutes.
  Handoffs gain `run` (trigger, start), stored when a run starts and cleared when its outcome is applied; a tick
  aborts a rebase only when that mark says a conflict run was lost to a restart.
- **2026-11-12:** A run's outcome survives a discarded tick (§5). `agentFinished` dropped the run from memory
  before the tick's commit, so when the commit was refused because `Gen` moved (the assessment finishing,
  leave, resume), pushed work stayed in needs-human and the run's events were lost. The run is now dropped only
  after the commit. The dirty assessment likewise wrote its whole copy of `dirty` back, erasing a `wip` set by an
  agent run that started meanwhile; it now merges only summary, message and error, and only into the same status.
- **2026-11-12:** A run lost to a restart is cleaned up (§5). Runs live in memory, so a restart mid-run skipped
  the cleanup and the ignored-file check (files the agent planted stayed, and the next run took them as its
  baseline), left the round's fingerprints counted (the same red checks escalated at once as "same failure
  twice"), and, for a dirty-agent run that had already edited files, rebuilt `dirty` without `wip`, stranding
  the human's changes in an unpushed commit a later panel commit would push unseen. The `run` mark gains the
  starting head; a lost run gets the failed run's cleanup, with ignored files judged by ctime since the pre-run
  stamps are gone. Fingerprints are recorded when a fix round ends, and `wip` is kept when `dirty` is rebuilt.
- **2026-11-12:** One rule for a clean worktree (§2). Handoff acceptance ignored untracked files, but agent runs
  and rebases refused any, so a handoff with wtp-copied config escalated on its first agent run, the dirty panel
  listed the files, and the panel's agent action committed and pushed them unticked. The untracked files present
  at handoff are now recorded (`untracked`) and excluded wherever TLM judges, commits, stashes or cleans the
  worktree; they're stamped with the ignored files, so an agent changing one is still caught.
- **2026-11-12:** The lost-run check fails closed, after a security review. A failed ignored-file listing now
  escalates instead of carrying on, and `node_modules/` is exempt only for what changed before setup finished
  (`run.setup_done`, recorded by the post-setup hook; a tick's commit keeps the stored mark so it isn't lost),
  so an agent writing `node_modules/.bin` after setup is still named.
- **2026-11-12:** Every API call now needs the token, not just creating a handoff. A security review showed a
  review container could reach the loopback API with any `Host` header and, with no token, change settings
  to mount the host into the next run, or start a dirty-worktree agent that pushes. `tlm` sends
  `lastmile.token` as a Bearer token on every call; the dashboard signs in with a one-time code the menu-bar
  item opens it with, traded for an HttpOnly cookie derived from the token. This supersedes "none take a
  token yet" for resume, cancel, reviewers and the dirty actions.
- **2026-11-18:** Lint and test caches leave the worktree. fidesplus#7136 went to needs-human three more
  times (Nov 8, 9, 13) because the agent, as its prompt asks, ran ruff, and `.ruff_cache/` counted as a
  gitignored file the run changed, which threw the fix away. fidesplus' repo env now points ruff, mypy and
  coverage at `/tmp`, turns off pytest's cache plugin and sets `PYTHONDONTWRITEBYTECODE`. The ignored-file
  check is unchanged; running agents on a copy of the worktree, which would retire it, is being scoped.
