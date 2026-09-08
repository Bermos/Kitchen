# Orchestration checkpoint — 8 September 2026, 16:00 UTC

Written by the orchestrator at the maintainer's "finish the ongoing issues,
cut a release, then pause". Whoever resumes reads this first and deletes it
once the state is back in the plan.

## Landed

- v0.40.0 (PR #544, auto-merge armed on the re-run checks of 0f8fd71 at 15:25 UTC; confirm `v0.40.0` is live with the chart asset before the next cut): #542 platform credential (the maintainer's), #552 grow
  platform volumes (#533), and the fixes #546 (#532), #549 (#530), #539
  (#529), #538 (#531), #563 (#562), #550 (restore after #545), #560, #561.
- Every issue dispatched in this run is closed: #529, #530, #531, #532, #533,
  #534, #556, #562.

## Open work

- Nothing is in flight. All worktrees under the session scratchpad are
  finished; `wt-498` still holds the #498 WIP at 4f96b3d (`make test` not run).
- #489 tail (#498 resume → #499 → #496 → #497 → #501) waits on D13–D15.
- Filed and open, small: #547 (the reading overlay on `/platform/signals`
  and the environment conditions strip), #554 (a screen for
  `status.systemLogs`), #564 (SIGPIPE under `pipefail` in the e2e binding
  steps — `kubectl … | base64 -d`, awk `exit`).
- Then #481, #445, #436's second half, #413, #518, #519, #377, #370.

## Decisions open for the maintainer

- D13 (#499): the observed graph from Hubble flows rather than spans (recommended).
- D14 (#497): ship `enforce` together with `observe`, off by default (recommended).
- D15 (#496): reuse the existing preview gate for `auth: gate` (recommended).

## Decisions taken on the maintainer's behalf this run (all stated in PR bodies)

- #530: TTLs and `logger.level: information` are fixed chart defaults; the
  reclaim is a sweep on every reconcile, deletes the superseded tables with no
  grace period, and runs only where the connection secret says
  `systemLogsBounded`.
- #532: option 2 (overlay from the loop's memory; the durable row is never
  rewritten); title, detail, confidence, projects and correlates move,
  severity/tier/scope/since/fingerprint/openedAt do not.
- #533: the chart renders the claim template from the live StatefulSet via
  `lookup`; growth is monotonic (largest requested size wins), capped at 8× per
  step, confirmed by typing the claim name; `status.storage` rather than a
  `status.components` row; after a resize `helm rollback`, client-side
  `--dry-run` and GitOps syncs diverge until the values carry the size.

## Lessons written into the skill

- A checkpoint commit is pushed only after `git diff --stat HEAD^ HEAD` names
  the note and nothing else (#545 reverted 61 files; #550 restored them).
