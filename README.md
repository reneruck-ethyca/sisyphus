# waterboy

macOS menu-bar service that keeps a worktree for every PR waiting on your review.
On each tick it:

1. **discovers** open PRs in `org` where review is requested from `user` or `user` has reviewed,
   and checks each out into a worktree (reusing an existing worktree on that branch, else `wtp add`),
2. **updates** tracked PRs: logs new commits / new replies, sets `pending_updates`, syncs the worktree,
3. **cleans up** PRs that are done: removes worktrees it created, forgets reused ones.

It does not run reviews yet. `onTracked` / `onUpdated` in `tracker.go` are the hooks for that.

UIs talk to `Service` (`service.go`): `Status()`, `PR(id)`, `RunNow()`, `OnChange(fn)`. It owns
the run loop and hands out copies of state, so a UI or API never races a running tick.

## Usage

```bash
make build                     # bin/waterboy (needs cgo / Xcode CLT)
bin/waterboy -once             # single tick, no tray; good for testing
bin/waterboy                   # menu-bar app ("WB"): status, Run now, Open state file, Quit
make install-agent             # run at login via LaunchAgent; logs to ~/Library/Logs/waterboy.log
make uninstall-agent
```

Requires `gh` (authenticated), `git`, and `wtp` on PATH. `install-agent` bakes your current
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

## `on_conflict` hook

Runs via `sh -c` with the worktree as cwd, at most 3 times per PR head (sync is retried after
each run). If it's still conflicting the PR gets `last_error`; the hook runs again only after a
new push. Environment:
`WATERBOY_HOOK`, `WATERBOY_PR_URL`, `WATERBOY_REPO`, `WATERBOY_PR_NUMBER`, `WATERBOY_BRANCH`,
`WATERBOY_WORKTREE`, `WATERBOY_CONFLICT_REASON` (`dirty` | `ahead` | `diverged` | `untracked-collision`).
