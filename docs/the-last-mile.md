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
- the worktree is clean
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
- **Replies are posted in one batch from the host.**
- **Bot comments get the same treatment as human ones, with a cap.** TLM replies to review bots
  (sisyphus, EthycaBot, others) but at most 3 replies per bot thread; after that it stops replying
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
  - a failure seen twice with the same fingerprint (job plus error signature) → needs-human
  - a reviewer re-raising a thread TLM already "fixed" → needs-human
- **Escalation:** a `tlm:needs-human` label, the `needs-human` notify event, and a dashboard banner.

## 6. Requesting reviewers

**Who.** Every TLM PR gets `EthycaBot` (a GitHub user account, so it can be requested directly).
Human reviewers come from the first source that has any:

1. `--reviewers` on the handoff. These replace the roster and are applied as given: no count, no
   filtering.
2. the roster: a global `lastmile.reviewers.roster: [alice, bob, …]` with
   `lastmile.reviewers.count: 1`, optionally overridden per repo. TLM picks round-robin, skipping
   the PR author, and keeps the cursor in its state store.
3. nobody, with a dashboard warning

**When.** EthycaBot is requested when the PR is ready for review. Humans are requested once, when CI
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
| **M0** | `handoff` command, state store, open the PR (template, Jira, labels, reviewers incl. EthycaBot and roster), snapshot, dashboard rows, `notify` | No | An orchestrator hands off a branch, the PR opens, and the callback fires on merged/closed |
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
  (EthycaBot always, then `--reviewers` or a global roster, GitHub as source of truth after the
  initial request, dashboard override). Extended the snapshot and the review-agent contract to match.
  New open questions on roster selection, scope and timing.
- **2026-11-05:** Resolved the reviewer questions (§9, items 6–9): round-robin from a global roster
  with optional per-repo overrides, humans requested after the first green CI (EthycaBot still at
  ready), `--reviewers` replaces the roster and is applied as given.
- **2026-11-05:** M1 implemented. Refinements: agent runs are goroutines per handoff and only the tick writes
  handoffs (a run leaves its outcome for the next tick), `FixRounds` is per trigger, fingerprints already
  fixed are kept in the handoff, flake reruns also apply with `fix-ci` off. Added a `cancelled` terminal
  state, `resume`/`cancel` (API, CLI, dashboard), and `lastmile.{flakes,flake_reruns,max_diff_lines,
  allow_paths,repo_checks}`. Agent tasks run through `review.Reviewer.RunTask` (shared slots and budget,
  no GitHub token, no stack yet).
- **2026-11-05:** M2 implemented. Refinements: the cursor is ids (per thread, per review source, per
  conversation-comment source), not timestamps; TLM's own comments are excluded by recorded id because it posts
  as the user's login; bot status also comes from `lastmile.bot_logins` (EthycaBot and sisyphus are user
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
  cap). `lastmile.bot_logins` defaults to `EthycaBot` only: sisyphus posts as its user, so "sisyphus" (in the M2
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
