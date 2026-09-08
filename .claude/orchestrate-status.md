# Orchestration checkpoint — 8 September 2026, 05:40 UTC

Written by the orchestrator at the end of a usage window, per the usage guard
in `.claude/skills/orchestrate/SKILL.md`. Whoever resumes reads this first and
deletes it once the state is back in the plan.

## Landed since the last note

- v0.39.0 is live (#524 requested bindings, #528, #535, #536, fix #537 for #534).
- After the cut, on `main` and waiting for the next one: #539 (fix #529,
  extraConfig by subPath), #541 (test(e2e), the Read == Push fallback asserted),
  #538 (fix #531, store.disk judges the volume), #542 (feat #349, the
  maintainer's own platform credential), #540 and #543 (agent memory).

## Open work

- **#532** (alerts overlay the current reading): the implementer was killed by
  the session limit a minute in. Its partial edits are pushed as an untested WIP
  commit d7ba455 on `claude/alerts-current-reading-532` (worktree `wt-532` in
  the session scratchpad). Decision taken: the issue's option 2, `listAlerts`
  overlays the current round's title and detail on the stored open row, plus a
  line saying when the reading is from; do not rewrite the open row. Resume with
  "verify against the issue before you trust the previous work".
- **#530** (system.* log TTLs): nothing written. Decisions taken: TTLs and
  `logger.level: information` are fixed chart defaults, not values, and
  `kitchen.retentionDays` does not govern them; the file is mounted by subPath
  per #539's PR body; the reclaim of the renamed old tables is the operator's
  (one-shot, recorded in status), with a README `### Upgrading` fallback.
- **#533** (grow a platform volume): not started; it was held behind #531
  because both touch `internal/api/platformstorage.go` and
  `PlatformStorageView.vue`. Shape: the operator orphan-deletes and recreates
  the StatefulSet and patches the claim, surfaced on `/platform/storage`; the
  floor is a chart guard naming the procedure plus docs.
- **#498** (OpenAPI as a release artifact, Fleet catalogue): WIP at 4f96b3d in
  `wt-498`, `make test` not run.
- **#489 tail** (#499 → #496 → #497 → #501) waits on D13–D15 (see below).

## Decisions open for the maintainer

- D13 (#499): the observed graph from Hubble flows rather than spans (recommended).
- D14 (#497): ship `enforce` together with `observe`, off by default (recommended).
- D15 (#496): reuse the existing preview gate for `auth: gate` (recommended).

## Next in order

#532 (resume) and #530 in parallel, then #533; reviewer on each; cut 0.40.0
(#542 is a feat). Then #489's tail once D13–D15 are answered, then #481, #445,
#436's second half, #413, #518, #519, #377, #370.
