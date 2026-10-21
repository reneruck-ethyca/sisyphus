# waterboy

macOS menu-bar service that keeps a worktree for every PR waiting on your review and runs a
review container in it. On each tick it:

1. **discovers** open PRs in `org` where review is requested from `user` or `user` has reviewed,
   and checks each out into a worktree (reusing an existing worktree on that branch, else `wtp add`),
2. **updates** tracked PRs: notices new commits / new replies, syncs the worktree,
3. **cleans up** PRs that are done: removes worktrees it created, forgets reused ones.

Scheduling and reviewing meet in `tracker.Events`: the tracker announces a PR (`Tracked`, `Updated`)
only once its worktree is synced to the head it announces, asks for a worktree back (`Reclaim`) when
it needs to sync or remove one a review is using, and reports `Dropped`. `review.Reviewer` implements
it: a queue, review containers, and run records. The default container is a placeholder that proves
the plumbing; the real reviewer image is the next step.

| Package | |
|---|---|
| `internal/tracker` | discover / update / drop, state file, drop rules, `on_conflict`, events, per-PR worktree locks |
| `internal/review` | job queue (one per PR), container runs, run records, re-attach after restart |
| `internal/container` | `Runtime` interface; `Docker` (docker CLI) and `Fake` |
| `internal/worktree` | git/wtp operations |
| `internal/github` | `Client` interface; `CLI` (gh) and `Fake` |
| `internal/app` | `Service`: the boundary UIs talk to (status snapshots, run now, run dirs, logs) |
| `main` | wiring, dashboard (`http.go`, `web/`), Wails window and menu bar (`ui.go`, `frontend/`) |

UIs talk to `app.Service`: `Status()`, `PR(id)`, `RunNow()`, `RunDir(pr, run)`, `OnChange(fn)`,
`Logs()`, `SubscribeLogs()`. It hands out copies of state, so a UI or API never races a tick or a run.

The menu-bar item and window are Wails v3 (beta, pinned in `go.mod`) (`ui.go`), one process.
The window (`frontend/index.html`, plain HTML/JS, embedded in the binary) calls the bound `UI`
struct via `/wails/runtime.js` and re-renders on the `status` event pushed from `OnChange`.
No Wails CLI or npm needed; `go build` is enough.

## Usage

```bash
make build                     # bin/waterboy (needs cgo / Xcode CLT)
bin/waterboy -once             # single tick, no tray or window; good for testing
bin/waterboy                   # menu-bar app ("WB"): status, Open window, Open dashboard, Run now, Open state file, Quit
make install-agent             # run at login via LaunchAgent; logs to ~/Library/Logs/waterboy.log
make uninstall-agent
```

The menu-bar app also serves a dashboard (overview + per-PR details, auto-refreshing) and a JSON
API on `http_addr`: `GET /api/status`, `GET /api/prs/{owner}/{repo}/{number}`, `POST /api/run`,
`GET /api/logs` (last 1000 ops log lines) and `GET /api/logs/stream` (server-sent events: backlog,
then live). If the port is taken it logs and runs without it. Both the dashboard (`/logs`) and the
window ("Logs") have a live log viewer. PR pages list review runs; a run's files are served at
`/prs/{owner}/{repo}/{number}/runs/{run}/` (`log`, `run.json`, `out/`).

Requires `gh` (authenticated), `git`, and `wtp` on PATH, and Docker for reviews (if it's down,
reviews wait in the queue and are retried every minute). `install-agent` bakes your current
shell's PATH into the LaunchAgent.

## Config

Optional JSON at `~/.config/waterboy/config.json` (`-config` to override):

| Field | Default | |
|---|---|---|
| `user` | `gh api user` | GitHub login whose reviews are tracked |
| `org` | `ethyca` | owner searched for PRs |
| `interval` | `10m` | Go duration between ticks |
| `repos_root` | `~/Dev` | clones live at `<repos_root>/<repo>`; PRs without a clone are skipped |
| `state_file` | `~/.local/state/waterboy/state.json` | tracked PRs |
| `include_drafts` | `false` | also track draft PRs |
| `hooks` | `{}` | `{"on_conflict": "<shell command>"}` |
| `hook_timeout` | `30m` | per hook run |
| `http_addr` | `127.0.0.1:7465` | dashboard + JSON API; only loopback `Host`s and same-origin POSTs are accepted |
| `review.disabled` | `false` | don't run reviews |
| `review.image` | `alpine/git:v2.49.1` | review container image |
| `review.command` | placeholder script | full argv, replaces the image's entrypoint |
| `review.env` | `[]` | names of variables copied from waterboy's environment |
| `review.env_file` | — | `KEY=value` file for secrets (launchd gives waterboy a bare environment) |
| `review.timeout` | `30m` | per run; the container is stopped after it |
| `review.max_concurrent` | `1` | review containers at a time |
| `review.cleanup` | `true` | after each run, reset worktrees waterboy created to the reviewed head |

## Rules

A PR is dropped (and its worktree removed, if waterboy created it) when it is merged or closed,
you approved it, someone else approved it and your latest review isn't "changes requested",
it became a draft (unless `include_drafts`), or it no longer shows up as requested/reviewed by you.
Fork PRs are skipped.

Syncing on new commits fast-forwards the worktree. After a force-push it hard-resets only if
the worktree is still exactly where waterboy last put it. Reused worktrees and pre-existing
branches are never reset or deleted. Modified tracked files, unpushed local commits, diverged
history, or an incoming file colliding with an untracked one are a conflict.

A PR missing from search results is only dropped if its own payload agrees (you're not a
requested reviewer and haven't reviewed), so search lag can't delete worktrees.

## Reviews

A run is started when a PR is first tracked (`new`), after new commits (`commits`, which also stops a
run still going on the old head) and after new replies (`replies`, queued behind a running one). Each
PR has at most one queued job (the newest; a never-reviewed PR stays `new`) and one running container,
which holds the PR's worktree lock: the tracker doesn't sync or remove that worktree meanwhile, asks
for it back instead, and catches up on the next tick.

The container gets the worktree and the clone's `.git` mounted read/write at their own paths (a
worktree's `.git` points into the clone), `/out` for results, the worktree as cwd, and
`WATERBOY_RUN_ID`, `WATERBOY_REASON`, `WATERBOY_PR_URL`, `WATERBOY_REPO`, `WATERBOY_PR_NUMBER`,
`WATERBOY_BRANCH`, `WATERBOY_HEAD_SHA`, `WATERBOY_WORKTREE`, `WATERBOY_OUT`. With `review.cleanup`,
worktrees waterboy created are reset to the reviewed head afterwards (tracked changes, commits on top
and untracked files go; ignored files stay). Reused worktrees are never reset: they may hold your work.

Runs live in `<state dir>/runs/<owner>/<repo>/<n>/<run>/` (`run.json`, `log`, `out/`); the queue and
the last 10 runs per PR are in `<state dir>/reviews.json`. Containers keep running when waterboy
quits and are re-attached on start; a run whose container is gone is marked `lost` and requeued.
Nothing is posted to GitHub yet.

## `on_conflict` hook

Runs via `sh -c` with the worktree as cwd, at most 3 times per PR head (sync is retried after
each run). If it's still conflicting the PR gets `last_error`; the hook runs again only after a
new push. Environment:
`WATERBOY_HOOK`, `WATERBOY_PR_URL`, `WATERBOY_REPO`, `WATERBOY_PR_NUMBER`, `WATERBOY_BRANCH`,
`WATERBOY_WORKTREE`, `WATERBOY_CONFLICT_REASON` (`dirty` | `ahead` | `diverged` | `untracked-collision`).
