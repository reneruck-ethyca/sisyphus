# sisyphus: architecture and code quality review

Date: 2026-10-27, at commit `8ea696c` (rename to sisyphus). Scope: every Go file outside
`reviewer/staged/`, both UIs, Makefile, Dockerfile, README. Checks run: `go build`, `go vet`,
`gofmt`, `golangci-lint` (default linters), `go test -race` (once, then the review package 4 more
times).

## Verdict

The foundation is sound. Keep it. The package layout matches the README, dependencies point one
way (`app → review → tracker → {github, worktree, config}`), interfaces exist only where a fake
exists (`container.Runtime`, `github.Client`, `review.GitHub`, `tracker.Events`), state files are
written atomically, every external call has a timeout, and the security reasoning (read-only
`.git` mounts, `os.Root` for run files, CSP sandbox, loopback-only, secrets never on argv) is
careful and written down. Tests cover the hard parts: 155 test functions, 48 of them on the review
lifecycle. Comments say why, not what.

The one third-party dependency is Wails. Zero for everything else.

What needs work, in order of payoff:

1. The concurrency contract for the tracker is a comment, and it has already been broken (one
   data race, one flaky test).
2. Two UIs render the same data model twice, in Go and in JS. Every UI change lands twice.
3. `review.Reviewer` is the one place where responsibilities pile up, and it is wired to the rest
   through four post-construction callbacks.

Everything else is polish.

## 1. Concurrency: the tracker's "ticking goroutine" contract

*Done 2026-10-27: `Tracker` locks, `Service.do` runs actions directly, `finish` records last.*

`tracker.Tracker` has no lock. Its methods say "Call it on the ticking goroutine". `app.Service`
upholds that by marshalling every mutation through `do()`, which hands the function to the loop
between ticks. Two things break it.

**A real race, caught by `go test -race`** in `TestUIRunEvents`:

```
Write at tracker.go:189 by (*Tracker).update ← Refresh ← Service.do ← Reviewer.execute
Previous read at tracker.go:42 by (*Tracker).Snapshot ← Service.RunOnce
```

`Service.do` falls back to running the function inline when `looping` is false
([service.go:372](../internal/app/service.go:372)). The test (and `-once`) never start the loop,
so the reviewer's `Refresh` runs on the reviewer goroutine while `RunOnce` reads state on another.
Production has the same window: in `Service.Run`, `go s.rev.Run(ctx)` starts before
`s.looping.Store(true)` ([service.go:343](../internal/app/service.go:343)). A job dispatched in
that gap refreshes inline while the first tick starts.

**A flaky test** (`TestStackStartsBeforeReviewAndStopsAfter`, fails roughly 1 in 6 runs under
`-race`) points at an ordering problem in `Reviewer.finish`
([review.go:1172](../internal/review/review.go:1172)): the run's final status is recorded first,
then the stack is stopped, then the worktree lock and the active slot are released. Observers see
"succeeded" while cleanup (up to `stackStopTimeout`, 2 minutes) is still running and the worktree
is still locked. The test asserts on that intermediate state.

**Recommendation.** Pick one of these; the first is the smaller diff.

- Give `Tracker` a `sync.Mutex` around `Tick`, `Refresh`, `Snapshot`, `IgnoredPRs`, `Forget`,
  `Untrack`, `Retrack`. Drop the inline fallback in `Service.do` and the `looping` flag: `-once`
  and tests then call `RunOnce` on the same goroutine as nothing else, which the mutex makes safe
  anyway. The "call it on the ticking goroutine" comments go away.
- Or keep the single-goroutine design and make it structural: `do` always goes through the loop,
  `RunOnce` becomes an op, and `-once` runs the loop for one tick.

Either way, in `finish`, stop the stack and release the lock before recording the final status
(or record `Finished` last), so a run is "done" only once it has let go of everything.

Related, lower priority: `do()` ops (Reset, Wipe, Untrack) run on the loop goroutine and can
block it for minutes on `docker compose down`. The next tick waits, and the UI shows nothing
running. Acceptable now; worth a `ponytail:` note naming the ceiling.

## 2. Two UIs, one data model, rendered twice

*Done 2026-10-28: the window loads the dashboard; `frontend/` and the `UI` bindings are gone.*

`web/` is server-rendered Go templates with a 15-second poll that reloads the page.
`frontend/index.html` is a 379-line JS single-page app for the Wails window that re-implements
the same views. The same helpers exist in both languages:

| Go (`http.go`, `view.go`) | JS (`frontend/index.html`) |
|---|---|
| `rel`, `ago`, `stamp` | `rel`, `ago`, `stamp` |
| `app.Outcome` | `outcome` |
| `reasonLabel` | `REASONS`, `reason` |
| `runDuration` | `dur` |
| `inlineMarkdown` | `md` |
| chip template | `chip`, `runChip` |

Git churn agrees: `http.go` and `ui.go` are in the top five most-changed files, and every UI
feature in the log touched both. The `UI` struct in `ui.go` is a second API surface mirroring
`app.Service` for the window alone.

**Recommendation.** The Wails window is a WebView. Point it at the dashboard it already serves
(`URL: dashURL` instead of `/`), and delete `frontend/`, the `UI` bindings, `wailsAssets`, and
the `go:embed frontend` line. What the window loses: push updates via `Events.Emit` (the dashboard
already polls `/api/status` every 15 s and streams runs and logs over SSE) and `OpenRun` in Finder
(one `POST /api/prs/…/runs/{run}/open` endpoint restores it). Net: roughly 500 lines gone, one
renderer, one place to change a view. If the HTTP listener fails, the window should show the
error instead of a blank page.

If the SPA stays, the reverse holds: serve it from the HTTP server too and drop the templates.
Keeping both is the one option to rule out.

## 3. `review.Reviewer` as the gathering point

The `review` package is 2,200 lines across six files, and nearly everything is a method on
`Reviewer` because it needs `r.mu`, `r.st`, or `r.sh`: queue and dispatch, container lifecycle,
re-attach after restart, log streaming and parsing, posting, compose stacks, the 5-hour budget,
the debug shell, and eval hooks. `follow` is 118 lines with five exit paths; `spec` is 108.

The package is also wired to its neighbours through four exported function fields set after
construction (`GitHub`, `Kick`, `Refresh`, `OnChange`), each nil-checked at every use, and
`tracker.Tracker` has two more (`OnSave`, `Events`). `app.NewService` is where the cycle closes:
Service sets `rev.Refresh` to a closure that calls `Service.do`, which calls `Tracker.Refresh`.

**Recommendations**, each independent:

- Replace the three callbacks `Kick`, `Refresh`, `OnChange` with one small interface the
  Reviewer takes in `New`, implemented by `Service`. The nil checks go, and the contract between
  the two is one type instead of three comments.
- `review.Job.PR` embeds the whole `tracker.Tracked` and persists it into `reviews.json` and every
  `run.json`. A review needs the ref, head, branch, worktree path, title, URL and author. A small
  `review.PR` struct with those fields stops tracker state from being copied into review state
  (the `Author` backfill comment in `tracker.update` is the first symptom of that drift).
- `stack.go` only needs `sh`, the stack config and a place to remember each PR's project and port
  offset. It could be its own package with its own small state. Worth it only when the next
  stack feature lands; not before.
- Leave `stream.go` and `post.go` where they are. They are already clean seams.

## 4. `main` is doing four jobs

`http.go` (546), `ui.go` (186), `main.go` (226) and their tests put the HTTP API, template
functions, SSE streaming, Wails bindings, and the `-reset`, `-shell`, `-eval`, `-judge` subcommands
in `package main`. `newHandler` is 127 lines of route closures.

After item 2 this shrinks on its own. What remains worth doing: move `http.go` and `web/` to
`internal/web` with a `Server` struct, and keep `main.go` as flag parsing and wiring. Low priority.

## 5. Tests

Good: real git repos for worktree and tracker tests, a faithful `container.Fake` with log replay,
tunable timers as package vars, the race detector passes everywhere except the one case above.

Gaps and nits:

- `internal/jsonfile` and `internal/proc` have no tests. One test for `Save`'s rename-on-crash
  behaviour is worth having; `proc` can stay untested.
- `stream.parseLine` has three tests for a function that decodes every Claude Code event type.
  Add a fixture-driven table from a real run log.
- `http_test.reviewServer` builds the full stack (git repo, tracker, reviewer, service) by hand
  with a polling loop. `testkit` only builds trackers. Extend it so `http_test`, `ui_test` and
  `service_test` share one constructor.
- `make test` runs without `-race`. Add it, and a `make lint` target.
- The review package takes 26 s under `-race` because tests wait on real timers. Fine for now.

## 6. Code quality, smaller items

*Done 2026-10-28 with item 4, except as noted in the revision log.*

Linters: `gofmt` clean, `go vet` clean, `golangci-lint` reports 50 `errcheck` hits (almost all
deferred `Close` and writes to an `http.ResponseWriter`) and 2 `staticcheck` nits
([review.go:1032](../internal/review/review.go:1032) capitalised error string,
[main.go:184](../main.go:184) De Morgan). There is no `.golangci.yml`. Add one that enables
`errcheck` (excluding `Close`, `Flush`, `ResponseWriter.Write`), `staticcheck`, `govet`, and
`errorlint`, so the signal stays usable.

- **Stranded comment** at [tracker.go:250](../internal/tracker/tracker.go:250): the doc comment
  for `forgetMissing` sits above `Untrack`.
- **Dead generality** in `github.CLI.Candidates` ([github.go:68](../internal/github/github.go:68)):
  a loop over a one-element filter slice with a `seen` map. One call, no map.
- **`sort` vs `slices`**: `rules.go` and `github.go` use `sort.Slice`/`sort.Strings`; everything
  else uses `slices`. Pick `slices`.
- **Magic strings** for `PRStatus.State` (`""`, `"queued"`, `"running"`), `Job.Reason` (`"new"`,
  `"commits"`, `"replies"`, `"manual"`), and `PRView.Health` are compared in `service.go`,
  `view.go`, `http.go` and the JS. `view.go` already has `Tone*` and `Group*` constants; give the
  other three the same treatment. Constants, not new types.
- **Migration code in hot paths**: `Reviewer.Status()` backfills `PostNote` on every call
  ([review.go:1279](../internal/review/review.go:1279)); `tracker.update` backfills `Author` every
  tick. Pre-1.0 with `make reset` available: delete both, or do them once at load.
- **Duplicated worktree creation** in `eval.runCase` ([eval.go:176](../internal/eval/eval.go:176))
  repeats `worktree.Create`'s `wtp add` plus last-line parsing. Add a `CreateAt(repo, branch, sha)`
  to `worktree` and call it.
- **Company-specific defaults in code**: `config.defaults()` hard-codes `org: ethyca` and the full
  `fidesplus` setup script, env, cache dirs and stack. For a foundation, defaults should be
  generic and that block should live in a `config.example.json` the README points at.
- **`pr-review-queue.sh`** (10 KB, pre-sisyphus) at the repo root is referenced only by a comment
  and the original plan. Delete it.
- **`docs/superpowers/plans/2026-10-20-waterboy.md`** is the original build plan under the old
  name. Keep or drop; it is not current.
- **State file writes**: `reviews.json` (every PR × 10 runs, each with a full `Tracked` copy) is
  re-marshalled and renamed on every `record()`, which fires per rate-limit line in the log. Fine
  at review-queue scale; add a `ponytail:` note so nobody is surprised when it grows.

## Suggested order

1. Tracker mutex + drop the `do()` fallback; reorder `finish`. Fixes the race and the flake.
   Small, contained, testable.
2. Collapse the Wails window onto the dashboard. Largest deletion, ends the double rendering.
3. Reviewer host interface and `review.PR` struct. Medium, mostly mechanical.
4. `.golangci.yml`, `make lint`, `make test` with `-race`, the small items in section 6.
5. `internal/web` extraction and `config.example.json`, when convenient.

## Revision log

- **2026-10-27**: item 1 landed (`08c12f0`). Verifying it surfaced two more pre-existing flakes
  (`TestHTTPWipe`, `TestMissingEnvFileHoldsReviewUntilCreated`); fixed with a corrected wipe
  assertion and `Reviewer.Wait` in test cleanup.
- **2026-10-28**: item 2 landed (`dfc0479`, `0a373db`). As planned, plus: status is pushed over
  `GET /api/status/stream` instead of a 15 s poll; external links carry `target="_blank"` in
  browsers and go through `POST /api/open` (GitHub only) in the window. Unplanned: `make start`
  now re-renders the LaunchAgent for the checkout it runs in (`54fb0ae`), found when the repo
  rename left the plist pointing at the old path.
- **2026-10-28**: item 4 landed on `chore/lint-and-cleanups`. `.golangci.yml` (standard linters
  plus errorlint; errcheck skips best-effort closes and response writes), `make lint`, `make test`
  with `-race`. The 20 findings left after that config were fixed rather than excluded: cleanup
  errors are now logged (a shared `removeContainer`), and two errors that formatted a possibly-nil
  error with `%v` were split. Section 6: `slices` over `sort`, `Candidates` without its one-item
  loop, `Reason*`/`State*`/`Health*` constants, `worktree.CreateAt` for evals, a ceiling note
  on `saveLocked`. Both backfills were deleted outright, since live state needed neither.
  Changed from the plan: `pr-review-queue.sh` stays, because it is an untracked local script, not
  part of the repo; only the comment citing it changed. Company defaults and `docs/superpowers`
  are untouched (item 5, and the user's call).
