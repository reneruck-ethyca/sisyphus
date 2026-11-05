---
name: handing-off-to-tlm
description: Use when a branch in ethyca/fidesplus or ethyca/fides-qa is finished and should become a PR, when the user says "ship it" / "open the PR" / "land this", when an orchestrator has finished branches or a PR stack to land, or when a TLM handoff notifies needs-human, merged or closed.
---

# Handing off to TLM

## Overview

`tlm` (the-last-mile, part of sisyphus) takes a finished branch and does the rest: opens the PR, requests
reviewers, watches CI and reviews, fixes CI failures and review comments with its own agents, rebases stacks.
**After a handoff, TLM owns the branch, the worktree and the PR.** You don't touch them, and you don't run
`gh` for that PR, until TLM hands it back (needs-human) or the handoff ends.

## Hand off, or not?

| Situation | Do |
|---|---|
| Finished, tests pass, user wants it out | `tlm handoff` |
| Repo isn't ethyca/fidesplus or ethyca/fides-qa | Normal PR flow; TLM refuses other repos |
| Unfinished / "draft so the team can see" | Normal draft PR; hand off once finished |
| User wants to read the diff before anyone sees it | Show them; hand off after they approve |
| Tests not run since the last change | Run them first |
| A PR for the branch already exists | `tlm handoff` anyway: TLM adopts the open PR |
| Touches migrations, lockfiles or `.github/workflows` | Hand off; TLM's agents may not edit those paths, so a fix there comes back as needs-human |

## Before handing off

The worktree must be clean (commit everything; untracked files are ignored) and ahead of `origin/<base>`.

## The command

```bash
tlm handoff --worktree ~/Dev/.worktrees/fidesplus/ENG-1234 --base dev \
  --jira ENG-1234 --body-file /tmp/pr.md \
  --autonomy fix-ci,address-reviews \
  --notify 'orca orchestration send --to orchestrator --message "TLM $TLM_EVENT $TLM_ID $TLM_PRURL $TLM_REASON"'
```

- `--autonomy` takes exactly these comma-separated tokens: `fix-ci`, `address-reviews`, `merge=off`, `merge=shadow`.
  Omit a token to keep that decision with a human. `merge=auto` is refused.
- `--title` defaults to the last commit's subject; `--jira` prefixes it with `ENG-1234: `.
- `--reviewers a,b` replaces the reviewer roster; leave it out to use the roster.
- TLM pushes the branch itself; don't push first.
- It prints the handoff id. Keep it. Every handoff gets a new id, a re-handoff of the same branch too.

## Getting told, not waiting

Use `--notify`; it runs through `sh -c` on `merged`, `needs-human` and `closed` (`--notify-on` changes the
set). The values come in two forms:
- env vars `TLM_ID`, `TLM_EVENT`, `TLM_REPO`, `TLM_BRANCH`, `TLM_PRURL`, `TLM_PRNUMBER`, `TLM_STATE`,
  `TLM_REASON`, `TLM_REQUESTER`: use these to build a message inside double quotes, as in the example.
- template fields `{{.ID}}`, `{{.Event}}`, … (same names): each renders as its own single-quoted word, so use one
  only as a whole argument (`notify-me {{.Event}} {{.PRURL}}`), never inside other quotes. Inside double quotes
  the protection breaks, and the reason text comes from GitHub.

Then end your turn or move on.

`tlm wait <id> --until merged,needs-human,closed` blocks for hours. Use it only in a script that must block,
never instead of `--notify`.

## Stacks

Hand off bottom-up, each child with `--stack-parent <parent handoff id or branch>` and the same `--base` as the
bottom PR. TLM targets the child's PR at the parent branch, and retargets and rebases it when the parent merges.

## When a handoff comes back

| Event | Do |
|---|---|
| `needs-human` | `tlm status <id> --json` gives the reason and snapshot. TLM has stopped acting on the PR, so you (or the user, if it needs a decision) may now work in the worktree: commit, push, reply. Then `tlm resume <id>` hands it back to TLM. |
| `merged` / `closed` | Done. Report it. |
| The user wants a change while TLM is active | Don't push into it. Wait for needs-human or approval, or `tlm cancel <id>`, make the change, commit, and `tlm handoff` again: TLM adopts the open PR. |
| CI red / new review comments while TLM is active | Nothing, if the handoff has `fix-ci` / `address-reviews`. TLM escalates what it can't fix. |

## Common mistakes

- Pushing to a handed-off branch while TLM is active: it races TLM's agents.
- Reading `tlm resume` as "take it back from TLM": it gives it back to TLM.
- Polling with `tlm status`, `tlm wait` or `gh pr checks` instead of `--notify`.
- Putting template fields inside quotes (`"TLM {{.Reason}}"`): use `"TLM $TLM_REASON"` instead.
