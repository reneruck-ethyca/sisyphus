---
name: handing-off-to-tlm
description: Use when a finished branch in ethyca/fidesplus, fides-qa, kairos, fidesinfra, terraform-modules or internal-helm-charts should become a PR ("ship it", a PR stack to land), or when a TLM handoff comes back with needs-human, merged or closed.
---

# Handing off to TLM

`tlm` (the-last-mile, part of sisyphus) opens the PR, requests reviewers, watches CI and reviews, fixes
failures and review comments with its own agents, and rebases stacks. **After a handoff, TLM owns the branch,
the worktree and the PR**: don't commit, push or run `gh` on them until TLM hands it back (needs-human) or the
handoff ends.

## Hand off, or not?

| Situation | Do |
|---|---|
| Finished, committed, tests pass since the last change | `tlm handoff` |
| Repo isn't one of those in the description | Normal PR flow; TLM refuses other repos |
| ethyca/fidesinfra, terraform-modules or internal-helm-charts | Hand off with `merge=off` (or `shadow`): only the user merges these, from the dashboard; TLM refuses `merge=auto`/`approved` |
| Unfinished, or the user wants to read the diff first | Normal flow; hand off once finished or approved |
| A PR for the branch already exists | Hand off anyway: TLM adopts the open PR |
| Touches migrations, lockfiles or `.github/workflows` | Hand off anyway; TLM's agents can't edit those, so a fix there comes back as needs-human |

## The command

```bash
tlm handoff --worktree ~/Dev/.worktrees/fidesplus/ENG-1234 --base dev \
  --jira ENG-1234 --body-file /tmp/pr.md \
  --autonomy fix-ci,address-reviews \
  --notify 'orca orchestration send --to orchestrator --message "TLM $TLM_EVENT $TLM_ID $TLM_PRURL $TLM_REASON"'
```

- `--base` is `dev` for fidesplus, `main` for the others.
- `--autonomy` takes only `fix-ci`, `address-reviews` and one of `merge=off` (default), `merge=shadow`,
  `merge=auto` (merge once the merge policy is satisfied), `merge=approved` (merge once approved). A left-out
  token stays with a human; merge only when the user asked for it.
- `--title` defaults to the last commit's subject; `--jira` prefixes it with `ENG-1234: `.
- `--reviewers a,b` replaces the reviewer roster.
- TLM pushes the branch; don't push first.
- It prints a new handoff id every time, a re-handoff of the same branch too.

## Getting told

`--notify` runs through `sh -c` on `merged`, `needs-human` and `closed` (`--notify-on` changes the set).
Single-quote the whole value and build the message from `$TLM_ID`, `$TLM_EVENT`, `$TLM_REPO`, `$TLM_BRANCH`,
`$TLM_PRURL`, `$TLM_PRNUMBER`, `$TLM_STATE`, `$TLM_REASON`, `$TLM_REQUESTER` inside double quotes, as above.
Don't use the `{{.Field}}` template form: inside quotes it lets GitHub text run as shell.

Then end your turn or move on. Don't poll with `tlm status` or `gh`; `tlm wait <id> --until …` is only for a
script that must block.

## Stacks

Hand off bottom-up: each child with `--stack-parent <parent handoff id or branch>` and the bottom PR's
`--base`. TLM targets the child PR at the parent branch and retargets and rebases it when the parent merges.

## When a handoff comes back

| Event | Do |
|---|---|
| `needs-human` | `tlm status <id> --json` gives the reason. TLM has stopped, so you (or the user, for a decision) may work in the worktree: commit, push, reply. Then `tlm resume <id>` gives it back to TLM. |
| `merged` / `closed` | Done. Report it. |
| The user wants a change while TLM owns it | `tlm cancel <id>`, make the change, commit, `tlm handoff` again. |
| CI red / new review comments | Nothing, if the handoff has `fix-ci` / `address-reviews`. TLM escalates what it can't fix. |
