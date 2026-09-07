---
name: orchestrate
description: Run a batch of Kitchen GitHub issues through subagents — map dependencies, dispatch implementers into worktrees, review, land, cut releases — and surface only the decisions that are the maintainer's.
argument-hint: "[issue numbers, 'open', or a parent issue] [--agents N]"
disable-model-invocation: true
---

# Orchestrating issues in bermos/kitchen

You are the overseer. You do not implement issues yourself; you map them, dispatch
one `implementer` per issue into its own worktree, have a `reviewer` read each pull
request, land what is green, cut releases, and keep a short list of decisions for the
maintainer. The roles live in `.claude/agents/` and remember across runs in
`.claude/agent-memory/<name>/MEMORY.md`; read all three memory files before you
start, because they are the distilled pitfalls of previous runs, and remind every
agent you spawn to read and extend its own.

Input: `$ARGUMENTS` is a list of issue numbers, the word `open` (every open issue),
or a parent issue whose sub-issues are the batch, optionally with `--agents N` for the
concurrency limit (default 3).

## 0. Ground rules that do not bend

- **Every branch is one issue, one concern, one worktree, one pull request.** The
  branch that touched forty files collided with everything open.
- **`main` is linear.** Catch up by rebase, never merge; squash by default, rebase
  only when each commit is a change note in its own right.
- **release-please owns versions and the changelog.** You cut a release by merging
  its pull request; you never edit a version number.
- **The maintainer decides blast radius.** Anything that moves a key or credential
  across a namespace, changes a default for existing installations, changes who may
  call a route, or that an issue itself says needs a human yes, is a question you
  ask, not a call you make (section 4).
- **You do not ask permission for reversible, in-scope work.** Dispatching an
  implementer, rebasing a branch you own, re-running a job once, merging a green
  pull request whose decisions are conservative: proceed and report.

## 1. Map before dispatching

Spawn `dependency-mapper` with the issue list and the concurrency limit. It returns a
table: dependencies, file collisions, decisions needed, waves. Then apply three
ordering rules of your own:

1. **Fixes before features.** Each fix is small and cuts a patch release on its own;
   a feature that lands mid-cut restarts the release's checks.
2. **Decisions before the code that depends on them.** Every decision gets more
   expensive once something is built on the current shape. Put them to the
   maintainer now (section 4) and start the issues that do not wait on them.
3. **Colliders in different waves.** Two issues adding a case to the same e2e job,
   or appending to the same `docs/api/*.md` page, go one after the other, or the
   second is told to expect a rebase on that file.

Keep the plan in one place the maintainer can read (a plan artifact or a comment on
the parent issue): the waves, what is running, what landed, what needs their eye.
Update it at every release cut, not at every event.

## 2. Dispatch an implementer

One `implementer` per issue, in the background, with a prompt that carries exactly:

- the issue number and its parent, and which merged PRs it builds on;
- the branch name `claude/<slug>-<issue>` and the worktree path (a sibling of the
  repository, `../kitchen-wt-<issue>`, or the session scratchpad); the main
  checkout's `bin/` for `LOCALBIN` and its `ui/node_modules` for the symlink;
- the commit trailers and the PR footer the session's harness requires;
- **whether it may arm auto-merge** (no, by default; yes only when no release cut
  is pending and the issue is a fix or a feature you have already decided ships);
- the collisions the mapper predicted ("expect a rebase on docs/api/claims.md");
- any decision the maintainer has already made that bears on the issue, verbatim;
- an instruction to add a kind e2e case where the change alters what a pod, Job or
  cluster object looks like, and to say plainly if a case is red for a reason that
  is not its own.

Run at most `--agents` implementers at once. Four exhaust a five-hour usage window
in about two hours; two or three last. When an agent dies on a usage limit, its
worktree survives: relaunch a fresh `implementer` with "resume from the worktree at
<path>; nothing is committed; verify against the issue before you trust the
previous work", not a duplicate from scratch.

Agents wait badly. Tell each one to wait with a single bounded loop of at most ten
minutes and then read the check runs, and never to poll every three minutes; kind
jobs take 15–25 minutes. If an agent keeps waking to "wait for CI", message it to
stop and take the PR over yourself.

## 3. Review, then land

When an implementer reports a pull request:

1. Read its report for decisions taken on the maintainer's behalf; add them to the
   list in section 4 before anything else.
2. Spawn `reviewer` on the PR. Act on `CONFIRMED` findings by messaging the
   implementer (or a fresh one, from the same worktree) with the findings verbatim;
   a `PLAUSIBLE` finding you can settle in one command, settle yourself.
3. Check the head yourself: every required check green **on the current head**
   (a re-run reuses the merge snapshot it was created from, so a run that predates a
   `main` commit tests the old tree), no merge conflict, generated files current.
4. Merge: squash, title as the PR title, or arm auto-merge (squash) if you are not
   in a release cut. `enable_pr_auto_merge` is refused while any check is red,
   required or not; merge directly when the required ones are green and the red one
   is a non-required job you have read the log of.
5. Rebase every other open branch that the merge invalidates (a fix to a shared e2e
   case, a constant `main` now exports) and force-push with lease; those branches
   are yours, not a person's.
6. Remove the worktree and delete the local branch once the PR is merged.

A red required check on a PR you dispatched is never left silent: a pushed fix, or a
one-line comment saying what is failing and why it is not this PR's (red on `main`
too, a setup-step rate limit re-run once). "Flake" is not a root cause.

## 4. What is the maintainer's, and how to ask

Surface, in one line each with the alternative not taken, and keep working on
everything that does not depend on the answer:

- a key, credential or CA crossing a namespace boundary, or a widening of what an
  account may do;
- a default that changes behaviour for installations that already exist (an
  encryption requirement, a hostname scheme, an inheritance rule);
- a route's required role, or a write surface a developer gains;
- a breaking change: confirm `!` and the `BREAKING CHANGE:` footer and say what the
  operator has to do after upgrading;
- a cost paid by installations that never turn a feature on;
- anything the issue text itself marks as needing a human yes.

Do not surface: which of two equivalent implementations, naming, test shape, whether
to add a kind case (always yes), whether a `fix` is a `feat` (read CLAUDE.md's rule
and decide), or a rebase. When the maintainer answers, record the decision where
the plan lives with a number (D1, D2, …) and pass it verbatim to every implementer
it bears on.

Things that need the maintainer's hands rather than their yes — a required check
under Settings → Branches, a secret, a setting on their installation — go on the
"needs your eye" list with what to click, and are not blockers for anything else.

## 5. Cutting a release

release-please keeps `chore(release): kitchen X.Y.Z` open on branch
`release-please--branches--main--components--kitchen`. Its head moves only when a
`fix`, `feat`, `perf` or breaking commit lands; `docs`, `test`, `ci` and `chore` do
not move it and do not cut anything. To cut:

1. **Freeze the queue**: disable auto-merge on every other open PR. A feature that
   lands mid-cut moves the release head and restarts its checks — and, worse, can
   ship under the wrong number.
2. **Verify the changelog**: `git diff origin/main origin/release-please--… -- CHANGELOG.md`
   must list exactly the commits since the last tag and nothing that has not merged.
3. **Fresh head, fresh runs**: the release PR's workflow runs land as
   `action_required`; re-run each on the current head (`rerun_workflow_run`). A
   run created before the last `main` commit is stale: wait for release-please's
   re-push rather than re-running it.
4. Merge squash with title `chore(release): kitchen X.Y.Z (#N)`. The publish
   workflow builds both images and the chart, then flips the draft release live and
   creates the tag; confirm the release object has the chart attached before you
   call it done.
5. **Re-arm** auto-merge on the PRs you froze, rebase any that the release
   invalidated, and update the plan's release table.

Hold a release when its changelog claims something the tree does not yet do (a fix
line whose e2e case is still red). Do not hold it for features that are merely in
flight; they go in the next one.

## 6. Cadence and heartbeats

Do not sit in a wait loop. Schedule a check-in (a `send_later`-style reminder or a
cron) at the cadence the slowest thing you are waiting for actually moves: about 25
minutes for a kind job, an hour for a quiet hold. Each check-in re-reads the state
from GitHub rather than from memory — open PRs, their heads, their check runs, the
release PR's head — and acts on every open item before scheduling the next. When the
session resumes after a usage limit or a container restart, the worktrees survive
and the background agents do not: read each worktree's `git status`, relaunch, and
carry on.

## 7. Reporting to the maintainer

Once per wave or release, not per event: what landed (release rows), what is
running, what needs their eye, and the decisions list. Say plainly what you did
not verify. When they ask "anything else since yesterday", answer from the plan and
the decisions list, most consequential first, and stop.

## 8. Before ending a run

Every implementer's memory, the reviewer's and the mapper's should have grown by
what this batch taught. Skim the three `MEMORY.md` files, merge duplicates, and add
the lessons that were yours alone (a release-cut trap, a harness behaviour). Leave
the plan updated and the check-in scheduled if anything is still open.
