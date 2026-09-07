# Orchestration checkpoint — 7 September 2026, 13:00 UTC

Paused by the maintainer mid-wave. Whoever resumes reads this first, then deletes it
once the state is back in the plan.

## Landed today

- v0.36.0 (08:00 UTC) and v0.37.0 (09:48 UTC), both live with chart and images.
- #513 (agents, memory, `/orchestrate`), #515 (usage guard), #508, #510, #511, #512.
- #468 closed. #500, #493 closed by their PRs.

## Open pull requests

- **#517** `claude/environment-binding-policy-494` (closes #494). Second review verified all
  eleven first-round findings fixed on b378e55. Third round (five small items) was in
  progress when paused: gate the Serves editor off preview environments, a shared
  `dataClassRefusalBetween` helper, a `resolveEnv` regression test for the inverted case,
  the PR body's no-requeue justification, and the retitle
  `feat(environments)!: an environment declares who may bind, and existing service bindings stop until it does`.
  The implementer was told to commit WIP and push; check the branch head against that
  list. **Merge waits on the maintainer**: lock existing bindings (built, breaking, `!`)
  or grandfather them. At merge, pass the `BREAKING CHANGE:` footer explicitly as the
  squash commit body.
- **#520** `claude/correlation-ladder-472` (Refs #472). Two reviews; F1–F5 of the second
  fixed on 4926bbe. One more found by the orchestrator (F6): `internal/notify/notify.go`
  reads `transition.Tier` directly, so with paging off a `minTier: page` subscription still
  receives page events; the notifier must apply `policy.Deliver`. Head 4926bbe is clean and pushed; F6 is not started
  (notifier imports `internal/signals` without a cycle, confirmed). Merge when F6 is done, checks green, one more look at the diff.
  **Do not close #472** until the maintainer says: the per-project override is deferred
  to #519.
- **#521** `claude/kitchen-project-plan-kx2gnm`: this wave's agent-memory growth. Merge
  squash any time (chore).
- **#516** release-please 0.37.1 for the docs-only #515. Do not cut alone; it becomes
  0.38.0 once #520 (feat) lands.

## Worktrees

`scratchpad/wt-494`, `scratchpad/wt-472` under
`/tmp/claude-0/-home-user-Kitchen/bc2e134f-c197-5bd3-b3e9-944480ea7646/`. Any
`wt-review-*` left behind belongs to a finished reviewer and can be removed.

## Decisions waiting on the maintainer

1. #517: lock or grandfather existing service bindings; `!` either way is recommended.
2. #472: close with #519 deferred, or leave open.
3. #520: one `signal_transitions` aggregate per `/platform` load on the API's on-request
   path (the loop no longer queries per round); bound it or accept.

## Next in order once resumed

#495 (needs #517 merged; touches `resourceclaim_service.go`, `docs/api/claims.md`,
`docs/api/environments.md`, the e2e workloads job), then #496–#499, #501, then #481,
#443, #445, #436 second half, #413, Valkey TLS, #377 alone, #349 with #370.

## Timers

None. The 13:22 UTC check-in was cancelled at pause.
