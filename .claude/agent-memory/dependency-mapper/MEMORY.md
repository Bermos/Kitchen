# Dependency-mapper memory

Collision hotspots and hidden dependencies seen in bermos/kitchen. Dated, one line
each. Merge duplicates; do not drop.

## Files that collide between concurrent branches

- 2026-09-06: `docs/api/claims.md` and `docs/api/environments.md` — any two claim or environment issues append to them. Expect a one-page rebase; do not hold a wave for it.
- 2026-09-06: `.github/workflows/test-e2e.yml`, the "Several workloads on kind" job — every issue that adds a kind case adds it there. Two such issues in one wave is a guaranteed rebase; three is a bad idea.
- 2026-09-06: `internal/cli/internals_test.go` — both sides add tests; the resolution is "keep both", so it is cheap.
- 2026-09-06: `internal/api/policy.go` and `docs/API.md`'s route table — two issues adding routes conflict on the table row order. Cheap, but note it.
- 2026-09-07: `internal/controller/build_controller.go`'s `Reconcile` — at the gocyclo limit; two issues that both add a step to it will each have to extract a helper and will conflict.
- 2026-09-07: `internal/controller/resourceclaim_service.go` is the serial spine of #489's tail — #495's grant, #496's auth branch and #497's allow-edge all land in it. Never two of them in one wave.
- 2026-09-07: `api/v1alpha1/offering_types.go` — the `auth`/`protocol`/`visibility` enums plus a coming `contract` field; #496 and #498 both grow it, and two issues growing different enums still conflict on the file.
- 2026-09-07: `ui/src/routes.ts` — two issues adding Fleet-scope screens both append to the fleet block. Cheap, but note it.
- 2026-09-06: Generated files never count (`zz_generated.deepcopy.go`, `charts/kitchen/templates/crds.yaml`, `config/crd/bases/*`, `ui/src/lib/policy.generated.ts`, `docs/schemas/kitchen.schema.json`); their sources do.

## Dependencies that the issue text did not state

- 2026-09-07: Anything that adds a claim condition or binding key depends on the claim's screen work being in (#470 split the project page; earlier screens no longer exist under their old names).
- 2026-09-07: Anything that reads `spec.exposure` or `EnvironmentType: stage` depends on #492 and #490; the #489 sub-issues all do.
- 2026-09-07: Anything about buildpacks configuration depends on the platform-directory mechanism from #508; a `BP_*` variable set any other way does not arrive.
- 2026-09-07: #497's `observe` mode has no NetworkPolicy implementation — plain `networking.k8s.io/v1` has no audit mode — so it must be computed from observed flow data (#499). #497 therefore depends on #499 despite the parent issue ordering them the other way.
- 2026-09-07: A NetworkPolicy in an application namespace cannot name the shared Gateway (Cilium's reserved `ingress` identity is unnameable by a v1 peer), so a default-deny there only bites unpublished ports; `charts/kitchen/templates/networkpolicy.yaml` already documents the shape.
- 2026-09-07: `GET /api/v1/offerings` (`acrossProjects`, with a `mine` flag) already answers a cross-project catalogue list, and `GET /api/v1/traffic` + `clickhouse.TrafficEdges` already aggregate namespace-to-namespace Hubble flows. A "new" catalogue or observed-graph route is usually an extension, not a route — check before sizing.
- 2026-09-06: A write route depends on its reconciler existing (CLAUDE.md: a write surface waits for its reconciler), so an API issue and its controller issue are one wave item, not two.
- 2026-09-06: A release cut freezes the queue: a fix merged mid-cut moves the release PR's head and its runs start over. Schedule fixes before the cut, features after.

## Kinds of issue that need a decision first

- 2026-09-07: Moving a credential, key or CA across a namespace boundary (#468 5(a)); a default that changes behaviour for existing installations (#430, #505); a route's role (#403); anything the issue body says "needs a human yes".
- 2026-09-07: An issue that says "build the general mechanism from #N and make this its first user" needs the maintainer to say how much of #N lands here; the answer moves the size a whole grade (#498/#187).
- 2026-09-06: A new claim type or provider: the maintainer decides tenancy (one server per environment vs per project) before the reconciler is built (#394).

## Sizing

- 2026-09-06: A "fix" that turns out to need a CRD field and a screen is M, not S; an issue whose body has a table of steps is L and should be split by its own headings.
- 2026-09-07: Four concurrent implementers exhaust a five-hour usage window in about two hours; two or three is sustainable.
