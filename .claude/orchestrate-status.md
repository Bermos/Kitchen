# Orchestration checkpoint — 7 September 2026, 18:00 UTC

Paused by the maintainer at a convenient point. Whoever resumes reads this first,
then deletes it once the state is back in the plan.

## Landed today

- v0.36.0 (08:00), v0.37.0 (09:48), v0.38.0 (17:17 UTC), all live with chart and images.
- #517 (#494, breaking: an environment declares who may bind), #520 (#472, the
  correlation ladder and signal policy), #524 (#495, requesting and approving a
  binding), #513/#515 (agents, memory, `/orchestrate`, its usage guard), #521, #522,
  #523 (memory growth).
- Issues closed: #468, #472, #494, #495, #500, #493.
- Decisions recorded: D11 lock existing service bindings (breaking); D12 close #472
  with the per-project override deferred to #519; the one `signal_transitions`
  aggregate per `/platform` load accepted; the request queue is `admin` on the
  providing project (an identity crosses).

## Open pull requests

- **#526** `claude/kitchen-project-plan-kx2gnm`: agent-memory growth from this
  afternoon, auto-merge armed. This note rides on the same branch.
- **release-please 0.39.0**: will open (or has opened) after #524 merged (a `feat`).
  Not cut. Cut it on resume: verify `git diff origin/main origin/release-please--… --
  CHANGELOG.md` carries only #524, re-run its `action_required` workflows on the
  fresh head, freeze other PRs, merge squash `chore(release): kitchen 0.39.0 (#N)`,
  confirm the release object is live with the chart.

## In progress, pushed, no PR

- **#498** `claude/openapi-artifact-catalogue-498`, head 4f96b3d, one `chore(wip)`
  commit, worktree `scratchpad/wt-498`. Done: `OfferingContract` (`contract.openapi`)
  on `ServiceOffering`; `BuildStatus.releaseArtifacts[]` with a one-value
  `ReleaseArtifactType` enum; `internal/artifact` (OCI-referrer store modelled on
  `internal/attestation`, with a tag fallback); `contract` accepted on the offering
  settings write and served on `GET /api/v1/offerings`. Remaining: build-time
  extraction (a helper off `prepareBuild`, read through `gitprovider`, attach after
  the push, write the status rows), tests for `internal/artifact`, the per-environment
  contract link, two Fleet-scope screens plus `ui/src/routes.ts`, `docs/API.md` row,
  `docs/api/services.md`, `make ui-policy`, the CLI decision, a kind case. **`make
  test` and `make lint` have not been run on the branch.** Scope decisions taken
  (D1 narrow reading of #187; D2 grow `/offerings` rather than a new `/services`
  route) are for the PR body with alternatives.

## Worktrees

`scratchpad/wt-498` only, under
`/tmp/claude-0/-home-user-Kitchen/bc2e134f-c197-5bd3-b3e9-944480ea7646/`.
`wt-495` is removed (#524 merged).

## Questions waiting on the maintainer (recommendation first)

- **D13, #499:** the observed graph from Hubble flows already in ClickHouse
  (`TrafficEdges`, `GET /api/v1/traffic`; no instrumentation needed; removes the
  "unused vs uninstrumented" ambiguity) or from OTLP spans as the issue text says.
  Recommend flows; start #499 on flows if no answer.
- **D14, #497:** may `enforce` ship with `observe` (off by default), or does
  `enforce` wait for #499's diff screen? Recommend together. Note `observe` has no
  NetworkPolicy implementation and must be computed from #499's flow data, so #497
  waits on #499 and #496 regardless.
- **D15, #496:** is `auth: gate` the existing preview gate taught service identity,
  or a sibling deployment? Recommend the existing gate; the issue delegates this.

## Wave plan on resume (≤3 implementers)

1. Resume #498 from wt-498 (verify against the issue first; run `make test`); start
   #499 (flows unless D13 says spans; take `docs/api/topology.md`; defer the
   project-screen panel until after any open claims PR).
2. #496 alone on the claim reconciler (`resourceclaim_service.go` is the serial
   spine; never two of #495/#496/#497 in one wave), after #524 — which is now in.
3. #497 after #496 and #499.
4. #501 last (by #489's own statement). Then #481, #443, #445, #436's second half,
   #413, Valkey TLS, #377 alone, #349 with #370.

## Collision hotspots (from the mapper)

`internal/controller/resourceclaim_service.go`; the "Several workloads on kind" job
(one new step per wave); `api/v1alpha1/offering_types.go` (#496 grows the `auth`
enum, #498 added `contract`); `docs/api/claims.md` and `docs/api/projects.md`;
`internal/api/policy.go`, `docs/API.md`'s table and `ui/src/routes.ts` (#498 and
#499 both add Fleet-scope routes).

## Timers

None. The 18:01 UTC check-in was cancelled at pause.
