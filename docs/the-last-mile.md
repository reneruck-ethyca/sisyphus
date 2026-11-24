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
  --labels … [--reviewers a,b] \
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
any open state ──(new review feedback)──→ feedback ──[review-response agent]──→ ci-running
awaiting-review ─┬→ approved ──[merge mode / button]──→ (queued →) merged ✔ ──→ cleanup, history
queued ──(kicked out)──→ flake: re-queue │ CI red: fix │ conflict: rebase │ else: queue agent
                 └→ conflict ──[rebase; agent only on conflict]──→ ci-running
any state ──(budget/rounds exhausted, repeated failure, policy)──→ needs-human ⏸
needs-human sent by CI ──(CI green again)──→ pr-open
closed by a human ──→ closed ✖
```

**What the harness does on every tick, with no agent involved:**
- builds the PR snapshot: head, checks, failed job logs, threads since the last cursor,
  mergeability, base drift
- reruns known flakes and cancelled checks within a budget
- rebases when the base moved and `merge-tree` reports no conflict, and when someone asked for it: the
  dashboard's Rebase button, or a fix or review agent's `"rebase": true` in its result (conflicts go to the
  conflict agent with fix-ci, else to a human)
- moves a draft PR to ready once the bot has reviewed it and CI is green (§6)
- requests reviewers (once, see §6) and re-requests after addressing their feedback

**Review feedback comes before CI.** New feedback starts the review agent whatever CI says, pending or
red: its push restarts CI anyway, so fixing a red run first would only cost another CI run. A red run gets
the fix agent (or a flake rerun, or a human) on the first tick with nothing new to answer; `ci-running` is
where a handoff waits once everything is answered.

**Adopting a PR.** When the branch already has an open PR, the review cursor starts past what's on it, except
for the review threads still waiting on the user: unresolved, not outdated, last comment by someone other than
the user (`user` in the config). Those are feedback on the first tick. Review bodies and conversation comments
have no resolved state, so all of them count as answered.

**An agent starts only in these cases:**

| Trigger | Agent gets | Agent returns |
|---|---|---|
| CI red (not a known flake) | snapshot, failed log excerpts, diff | edits in the worktree + `{summary, confidence, escalate?}` |
| New review threads | snapshot, threads, diff, current PR body | edits + `[{thread_id, action: fix\|reply\|defer\|skip, reply}]` + optional `{body, reason}` |
| Rebase conflict | conflicted files, both sides | resolved files, or `escalate` |
| Kicked out of the merge queue, cause unknown (§7.2) | snapshot, removal reason, failed logs | edits + `{action: requeue\|escalate, summary}` |

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
  Where checks write build output into the tree and no env var moves it (turbo's `.turbo/`, Next's `.next/`,
  terraform's `.terraform/` and, in terraform-modules, the gitignored `.terraform.lock.hcl`), the repo lists it
  under `scratch`: ignored files under those globs that the run created or changed are removed after it without
  failing it (a build makes them again). fidesplus lists `__pycache__`/`.pyc` (a subprocess can drop
  `PYTHONDONTWRITEBYTECODE`), `node_modules/.cache` and `clients/openapi.json` (openapi-ts' input).
  Putting the worktree back (`git clean`) spares the repo's in-tree `cache` dirs: they are volume mount points.
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
    "agent running" in place of the panel, and Resume is refused until the run is applied: a run failing on a
    resumed handoff would leave the human's changes uncommitted with no one told.

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

**When.** A ready PR means "ready for human review", so TLM opens every PR as a draft and the bot goes
first. Which path a handoff takes depends on the PR it finds:
- **No PR, or a draft PR.** TLM opens the PR as a draft (or adopts the draft) and requests ethycabot at
  once. It gets CI green and addresses ethycabot's reviews, at most 3 review-agent rounds on the bot's
  feedback alone (`maxBotRounds`); after a push the bot is asked to review the new head. Once CI is green,
  no agent is at work and the bot has reviewed the head (or the 3 rounds are spent, or it stayed quiet
  for 30 minutes), TLM marks the PR ready and requests the humans. From there it is the normal loop.
- **A ready PR.** ethycabot and the humans are requested at once; humans already on the PR count, so it
  is only topped up. Then the normal loop: CI green, the bot's feedback (same 3-round cap), the humans'.

Bot feedback past its 3 rounds is left on the PR for the humans: it neither escalates nor uses up the
human feedback rounds (`max_fix_rounds`). fidesplus runs CI on drafts, so the draft phase doesn't save
CI runs; it keeps humans out until the bot's findings are dealt with. After the first request,
**GitHub is the source of truth**:
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
3. **`merge=auto`**: merge once the evaluator below is satisfied.
4. **`merge=approved`**: merge as soon as the PR is approved; the evaluator is not consulted.

The evaluator's rule set:
   - required approvals present, none stale
   - all required checks green on the current head
   - no unresolved threads, except threads whose last comment is ours (the `gh` login): answered
     threads don't block
   - base up to date, unless the PR touches no migration (`lastmile.migration_paths`); a PR without
     migrations may merge while behind
   - no risky paths touched (migrations, auth, infra)
   - outside a merge-freeze window

There is no diff-size rule (dropped 2026-11-18).

### 7.1 How a merge happens

- **The repo allows GitHub auto-merge** (`autoMergeAllowed`): for `merge=auto` and `merge=approved`
  TLM enables GitHub's auto-merge (squash) on the PR and GitHub decides when. The evaluator still
  runs and is logged, but it decides nothing. TLM keeps tracking the PR until it is merged or
  closed, and applies the same retries and error handling as its own merges: red CI, conflicts,
  and removals from the merge queue (§7.2). If GitHub drops auto-merge (for example after a
  removal), TLM turns it back on once the cause is dealt with.
- **It doesn't:** TLM merges itself, on the tick, per the mode above.
- **By hand:** once the PR is approved, the dashboard shows **Squash and merge**. The dashboard can
  also turn auto-merge on or off for a handoff (`off`, `when checks pass`, `when approved`; just
  on/off where GitHub auto-merge is used).

Every merge TLM makes itself (button, `approved`, `auto`) needs a current human approval: one
that isn't stale and isn't a bot's. The merge is pinned to that head. A merge freeze holds
`approved` and `auto`, not the button, and not GitHub's own auto-merge, whose timing is GitHub's.
A PR GitHub reports as already mergeable can't get auto-merge, so TLM merges it itself. Turning the mode to `off` (or `shadow`) withdraws a pressed
button and turns off GitHub auto-merge that TLM turned on; auto-merge a person set on GitHub is
left alone. TLM also turns its own auto-merge off when it escalates to needs-human or the handoff is
cancelled: when TLM lets go, nothing it armed keeps going.

**Manual-merge repos** (`lastmile.manual_merge`; by default fidesinfra, terraform-modules and
internal-helm-charts, where a merge to main deploys or publishes a release) merge only on a person's
say-so in the dashboard. A handoff can't ask for `merge=auto` or `merge=approved` there, and the merge
button and those two modes refuse the API token (`tlm`, agents) with 403: only the dashboard's session
cookie can press the button or turn merging on. `off` and `shadow` stay open to the token.

Whatever starts it, a merge squashes. If the base branch uses a merge queue, merging means adding
the PR to the queue (`enqueuePullRequest`), and the handoff goes to a new state, **`queued`**. TLM
keeps ticking until GitHub reports the PR merged.

### 7.2 Kicked out of the merge queue

GitHub records each removal (`RemovedFromMergeQueueEvent`, with a reason). TLM counts them per
handoff, and on the **10th** removal it escalates to needs-human. Before that, by cause:

| Cause | TLM |
|---|---|
| Known flaky check | re-queue |
| Real CI failure | fix agent (only with `fix-ci`, otherwise needs-human), then re-queue |
| Conflict with the base | rebase (agent on conflict, as §4), then re-queue |
| Anything else | a **queue agent** investigates: it gets the snapshot, the removal reason and failed logs, and returns `requeue`, edits to commit, or `escalate` |

The queue's CI runs on the merge group, not on the PR head, so failed logs come from the
`merge_group` run.

A removal by a person (dequeued by hand) is not a kick-out: TLM counts nothing, sets the handoff's
merge mode to `off` and leaves it to them. "Ours", for answered threads, means the last comment is
by the PR's author (the `gh` login TLM acts as) or one TLM posted.

Removals count only while TLM means to merge the PR (the button was pressed, or the mode is `auto`
or `approved`); a PR a person queued by hand is theirs. Removals from before the handoff or from a
needs-human stretch don't count, and Resume after the 10th starts the count again. A fix pushed
after a removal makes the approval stale, so the PR goes back into the queue once someone approves
the new head.

### 7.3 After a merge

When a handoff reaches `merged`, TLM removes its worktree and local branch (`wtp rm
--with-branch`); the remote branch is left to the repo's delete-on-merge setting. A worktree with
uncommitted changes, unpushed commits, or that isn't a linked worktree is kept and the handoff
says so. Finished handoffs (merged, closed,
cancelled) move to a **History** section of the Last mile page. The `merged` pill is purple.

Signals to collect from the start, even before they're used: diff size, touched-path risk, number of
fix rounds, reviewer identity (human or bot), time spent in review, CI flake count.

### 7.4 Downrev bumps (fidesplus)

A PR that adds an alembic migration goes stale every time another migration lands on the base: its
first migration's `down_revision` must point at the base's new head. Each bump used to cost a full
CI run, during which the next migration could land. fidesplus has a `downrev-only` label that makes
CI, on the PR and in the merge queue, run only the migration checks.

- **Bump without an agent.** When TLM rebases a PR onto a moved base (§4) and, per alembic
  versions directory, the PR's own chain hangs off a revision that is no longer the base's single
  head, TLM rewrites the `down_revision` of the PR's first migration to the base head and commits
  it as its own commit, in the same run as the rebase, before the push. Several heads on the base,
  or a PR migration TLM can't parse, goes to needs-human.
- **Label only when provably downrev-only.** TLM adds `downrev-only` when the rebase was clean (no
  conflict agent), the head before it was fully green, and the only commit TLM added is the bump.
  It records the head it labelled.
- **Label off as soon as anything else changes.** Any other head (an agent's commit, a person's
  push, a rebase without a bump) removes the label before the new head's CI matters.
- **Approval carries over.** A human approval of the head before a labelled bump counts as an
  approval of the bumped head (§7.1's fresh-approval rule), since the label vouches that nothing
  else changed. GitHub may still demand re-approval if branch protection dismisses stale reviews.
- **Scope:** per repo in config (`lastmile.downrev_labels`, `{"ethyca/fidesplus": "downrev-only"}`);
  other repos still get the bump, without a label.
- **As built:** one bump commit per versions directory, rewriting the `down_revision` value
  (quotes and a trailing comment kept) and the docstring's `Revises:` line. The base's head is
  read from the base branch only, never from files the PR changed, and every revision id must be
  plain (`[A-Za-z0-9_]+`). The label goes on before the push, since fidesplus CI reads labels off
  the push's event; a failed push takes it off again, a failed label means a push without it. A
  stack child's rebase after its parent merged also gets the bump, never the label. The label
  stays on while GitHub still shows the pre-bump head (it lags the push), and comes off on cancel.
  The carry-over spans a chain of rebase+bump-only heads: an approval of the head before the first
  of them counts for the last.
  Every other TLM push from a labelled head (an agent's commit, a rebase without a bump) takes the
  label off before pushing, and goes to needs-human if it can't. A person's push while the label is
  on can't be pre-empted; fidesplus's `check_downrev_only.py` and the merge queue are the backstop.

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
5. **Repos: fidesplus and fides-qa, then kairos, fidesinfra, terraform-modules and internal-helm-charts**
   (2026-11-20). The infra repos merge only from the dashboard (`lastmile.manual_merge`, §7.1).

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
  starts past them (threads waiting on the user: superseded 2026-11-23, see below), and (b) humans already requested or with a standing review count toward the
  roster count. Adopting still marks a draft PR ready unless the handoff asked for `--draft` (kept
  on purpose; superseded 2026-11-19, see below). Title, body and labels of an adopted PR are left as they are.
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
- **2026-11-18:** Resume is refused while an agent run is in flight on the handoff. The dashboard offered
  Resume during a dirty-agent run; if the agent then failed, its outcome only set the reason on a handoff
  already back at pr-open, so the human's unwrapped changes sat in the worktree without an escalation.
- **2026-11-18:** Merging (§7). TLM can now merge: `merge=auto` (evaluator) and the new `merge=approved`
  (approval alone), using GitHub's auto-merge where the repo allows it, plus a **Squash and merge** button
  once approved. Merge-queue repos get a `queued` state; TLM tracks the PR until it merges, re-queues by
  cause after a removal (a queue agent for unknown causes) and escalates on the 10th. The evaluator was too
  strict: threads answered by us no longer block, a PR without migrations may merge while behind, and the
  diff-size rule is gone. Merged handoffs clean up their worktree and branch and move to History; the
  merged pill is purple.
- **2026-11-18:** Merge safety, from review. Where GitHub auto-merge is allowed, TLM turns it on as soon as
  the PR is ready, for `auto` too, rather than after its own evaluator agrees: GitHub decides, as §7.1
  says. Every merge TLM makes itself needs a fresh human approval of the current head and is pinned to it;
  a freeze holds `approved`/`auto` but not the button; switching to `off` withdraws the button and turns
  off the auto-merge TLM enabled.
- **2026-11-18:** GitHub won't take auto-merge on a PR that is already mergeable, so TLM merges such a PR
  itself, under its own gates. TLM turns off the auto-merge it armed when it escalates or is cancelled.
- **2026-11-18:** From the whole-branch review. Cleanup also keeps a worktree with unpushed commits or one
  that isn't a linked worktree. If a person turns GitHub auto-merge off (same head, no removal, after a
  short grace), TLM takes it as a decision and sets the mode to `off` instead of re-arming. GitHub's
  auto-merge is used only where GitHub enforces reviews; otherwise TLM merges itself, with a human
  approval. When TLM lets go (off, cancel, needs-human) it also takes out of the queue a PR it queued. A
  merge GitHub keeps refusing goes to needs-human instead of being retried forever. Removals caused by a
  push, or classifiable by cause, are never read as a person's dequeue.
- **2026-11-18:** Downrev bumps (§7.4), lined up after merging: TLM bumps a stale migration's
  `down_revision` itself after a rebase and labels the PR `downrev-only` (fidesplus) while that is
  provably all that changed, so CI and the merge queue run only the migration checks.
- **2026-11-18:** §7.4 as built: one bump commit per versions directory, `Revises:` left alone,
  stack-child rebases bump without a label, the label survives GitHub's lag behind the push, and
  approval carry-over spans one bump, not a chain of them.
- **2026-11-18:** §7.4 after review: the label goes on before the push (CI reads it off the push's
  event), comes off on cancel and after a failed push; carry-over spans a chain of consecutive
  bumps; `Revises:` is rewritten too; the base head is read from the base branch only, with
  revision ids validated.
- **2026-11-18:** §7.4: TLM's own non-qualifying pushes from a labelled head take the label off
  first; a person's push is left to fidesplus's `check_downrev_only.py` and the merge queue.
- **2026-11-19:** Draft first, humans after the bot (§6). `--draft` is gone: every PR opens as a draft,
  ethycabot is requested at once, and TLM marks the PR ready (and requests the humans) when CI is green
  and the bot has reviewed the head, its feedback has had 3 rounds, or it stayed quiet for 30 minutes.
  A PR adopted as ready gets ethycabot and the humans at once, no longer at the first green. The bot's
  feedback alone has its own 3-round cap; past it, its items are left for the humans instead of
  escalating. Why: a handoff with `--draft` stayed draft and never got a reviewer, and humans were
  pinged before the bot's findings were fixed. fidesplus runs CI on drafts (only `openhands` drafts
  are held), so the draft phase can't hold CI back; reviewing before the PR exists was the alternative.
- **2026-11-19:** A CI escalation clears itself, cancelled checks are flakes, approvals show. fidesplus#7579
  sat in needs-human, approved and mergeable on GitHub, because needs-human was sticky: TLM only watched for
  merged and closed. It got there because every job of one run was cancelled, which counted as red and spent a
  fix round. Now a handoff that CI escalated (`escalated_by: ci`) goes back to pr-open once CI is green again;
  other escalations (review feedback, rebase, dirty worktree) still wait for a person. A `CANCELLED` check is
  treated as a flake and re-run within the flake budget. The dashboard puts "approved by …" (and "mergeable"
  when GitHub's merge state is clean) next to the state on the list, the handoff page and Home, and in the
  needs-human notice.
- **2026-11-19:** CI follows GitHub's required checks. fidesplus#7579 was mergeable on GitHub (merge state
  clean) while TLM waited on a re-run of jobs branch protection doesn't require: TLM's CI rollup counted every
  check. Checks now carry GitHub's `isRequired` for the PR, and when any check is required only those decide
  red, pending and green. A repo without required checks keeps the old rule. The fix agent still gets the logs
  of every failed job, since a required summary job fails because of them.
- **2026-11-19:** Rebase on request. A handoff can be rebased onto its base without waiting for GitHub to call
  it behind: the dashboard's Rebase button (`POST /api/handoffs/{id}/rebase`, refused unless the branch is
  behind and the handoff is open; from needs-human it resumes the handoff too, unless the worktree is dirty), or the fix and review agents, which may set
  `"rebase": true` with a `rebase_reason` in result.json. The request is `rebase_asked` on the handoff, and
  both start a tick at once (not at `lastmile.interval`), whose drift step runs it like any rebase; while
  GitHub hasn't caught up with an agent's push the tick looks again every 15 seconds. An agent asks through its result rather than a tool: agents
  get no route to the sisyphus API from their container, and the rebase runs after the run anyway. A fix
  agent that changes nothing but asks for a rebase (the fix is on the base already) no longer escalates.
- **2026-11-19:** Rebase from needs-human. The Rebase button was hidden on needs-human handoffs, yet a fix agent
  that finds the fix on the base escalates exactly there (fidesplus#6532: the Semgrep config only exists on dev,
  828 commits ahead). Pressing Rebase on a needs-human handoff now resumes it as `tlm resume` does and asks for
  the rebase; it stays refused while the worktree has uncommitted changes or an agent runs.
- **2026-11-20:** More repos: kairos, fidesinfra, terraform-modules and internal-helm-charts are in the default
  `review.repos`, so they are both reviewed and handed off. None has a devcontainer, so they share one image,
  `sisyphus-reviewer:ops` (`make reviewer-image-ops`: node 26 + pnpm, terraform 1.14.5, tflint, helm, yq, just,
  hadolint, shellcheck, uv, at the repos' CI versions); `reviewer/Dockerfile` takes the base's user as `RUN_USER`.
  Their notes keep agents to local checks (no plan/apply, no cluster or registry). New per-repo `scratch` globs
  (§5) for build output checks leave in the tree, which otherwise failed every agent run on these repos. New
  `lastmile.manual_merge` (§7.1): the three infra repos merge only from the dashboard, since a merge to main
  deploys (fidesinfra `cd.yml`) or publishes a release; kairos may auto-merge. `risky_paths` stays global: it
  already blocks `merge=auto` for `*.tf`, Dockerfiles and `.github/`.
  Security review follow-ups: the manual-merge gate allow-lists a valid session cookie rather than
  deny-listing the bearer token (still a guard rail: a process that reads the token file can derive the
  cookie, and has `gh` anyway), and a lost run removes new scratch files instead of skipping them;
  fidesinfra's uv venv moved out of the tree (`UV_PROJECT_ENVIRONMENT`) rather than into `scratch`.
  First kairos run: setup failed, as the root `prepare` script (`lefthook install`) can't write hooks into the
  read-only main `.git`. So node_modules never joined the baseline and the run went to needs-human over rewritten
  `node_modules/.bin` shims. kairos setup now runs `pnpm install --ignore-scripts`. Neither `LEFTHOOK=0` nor `CI`
  stops `lefthook install`, and `pnpm rebuild` runs `prepare` again. The dependencies' scripts aren't needed for
  checks, and a branch can no longer run its own install scripts during setup.
  Its next run tripped on `apps/kairos/dist/tsconfig.tsbuildinfo`: a host build had left it there and the agent's
  type check rewrote it. A changed pre-existing scratch file is now removed instead of failing the run, as a new
  one already was and as a lost run already did (§5). It is build output, so deleting it costs at most a rebuild.
- **2026-11-23:** §4: review feedback now goes before red CI. Before, a red PR sent new comments to the back:
  the fix agent ran, pushed, and only the tick after that handed the comments to the review agent, whose push
  then started yet another CI run. Feedback was already handled while CI was pending; now it is on red too, and
  the red run is handled once nothing new is left to answer. A needs-human from red CI is still marked as CI's
  (so it resumes on green), but one from feedback on a red PR is now the review's.
- **2026-11-23:** §4: an adopted PR's unresolved, current threads whose last comment isn't the user's are
  feedback, no longer history. fidesplus#7660 was handed off with 4 open reviewer threads posted minutes earlier;
  TLM sat in ci-running and never answered them. The usual reason to hand off an existing PR is to have TLM
  finish it, open threads included. Resolved, outdated and already-answered threads stay history, as do review
  bodies and conversation comments, which can't be told answered from unanswered.
- **2026-11-24:** §5: fidesplus gained `scratch` globs and `git clean` spares cache mount points. fidesplus#7692
  and #7693 went needs-human over `.pyc` files written despite `PYTHONDONTWRITEBYTECODE`, and over openapi-ts'
  `clients/openapi.json` and jiti cache from the Check-OpenAPI-Types fix: all rebuildable output. #7692's reset also
  failed because the branch stops tracking `src/fides/bin`, a cache volume mount point, so `git clean -d` tried to
  remove it while Docker held it.
- **2026-11-24:** §5: agent prompts now name the PR number, and container setup puts HEAD's tracked files back
  under in-tree cache mounts. fidesplus#7693's CI agent escalated a fixable Check-Changelog (it had to rename
  `0000-*.yaml` to the PR number and had no way to learn it), and reported `gpp_js_integration/compiled/__init__.py`
  as deleted: the cache volume over `compiled/` hid that tracked file inside the container.
