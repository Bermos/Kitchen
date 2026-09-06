# Spike — the missing edge between projects, and what environments owe it

*September 2026. A design spike: no platform code changed, and nothing here is
built. It exists to decide the shape of one feature — a project consuming
another project's service — and to record three things found while looking,
which have to be fixed for that feature to be honest.*

Kitchen's grouping is Project → Environment → Release, with `ResourceClaim`
for what a project depends on and `Addon`/`Connection` for what the platform
provides. That holds up for one team with a handful of applications. It stops
answering the moment a second team runs something the first one wants to call:
an internal API, a shared cache nobody wants two of, a DuckDB container one
team maintains and four teams query.

The conclusion, before the working:

- **The gap is an edge between projects, not a box around them.** Project is
  still the right unit — it is exactly the unit of independent deploy, access
  and build. What is missing is the ability for one to *offer* something and
  another to *claim* it.
- **The edge costs no new CRD.** The provider side is a list on `Project`; the
  consumer side is a new value of `ResourceClaim.spec.type` plus a shape in the
  `config` it already carries.
- **Environments are inferred, not declared**, and that is the load-bearing
  problem underneath the whole feature. There is no route that creates one;
  `EnvironmentType` has two values, so a staging environment is typed
  `production`; and every non-preview environment of a project resolves to the
  same hostname.
- **Every environment is published on the public Gateway, and nothing can opt
  out.** An internal service is currently a service on the internet.
- **Two of the four grouping objects people ask for are not needed**, and the
  two that might be are different objects doing different jobs. Neither is
  needed first.
- **The monorepo fan-out has an exact answer that is not a path filter**: skip
  on an unchanged git tree object, and record the skip.

## The questions this had to answer

1. Several teams, many projects. One team's project is an internal service the
   others consume.
2. An application that is genuinely several services.
3. A team hosting a DuckDB container another team's application queries.
4. Whether a bigger structure belongs around Project, and whether the current
   grouping is still correct.
5. Publishing an OpenAPI specification so other teams can develop against it,
   and pinning a version of it.
6. Whether any of that produces a topology, and whether the topology can drive
   NetworkPolicies.

## Finding 1 — the quadrant that does not exist

Two axes decide where a dependency lives: who owns it, and who consumes it.

| | Consumed by its owner | Consumed by other projects |
|---|---|---|
| **Owned by a project** | `ResourceClaim` | **nothing** |
| **Owned by the platform** | `Connection`, `Addon` | `Connection`, `Addon` |

Every question above is the empty cell.

It is worth being precise about why the Addon catalogue does not already fill
it. [docs/api/addons.md](../api/addons.md) refuses to become a general chart
installer, and the reason is the grant: the install job binds to cluster-admin,
so a request naming what to install would make that grant unbounded and reduce
its audit record to "the platform installed something". **That argument does
not reach the empty cell at all.** A project offering a service runs it in its
own namespace, under its own quota, as its own workload, with no platform
grant of any kind. So the fix is not a wider addon catalogue — that would be
the exact mistake the catalogue is compiled in to avoid — it is letting a
Project be a provider.

## Finding 2 — environments are inferred, and it shows

There is no way to create an Environment. `ensureEnvironment`
([internal/controller/build_controller.go:1951](../../internal/controller/build_controller.go))
creates one on the first build for its target, and that is the only path.
Three consequences, in increasing order of how much they matter:

**An environment cannot be set up before it is deployed into.** `spec.Owners`,
`spec.Requirements`, `spec.DataClass`, `spec.Criticality`, `spec.RTO`/`RPO` are
documented as the environment owners' declaration, deliberately not the
deploying team's — that is segregation of duties written as a schema
([api/v1alpha1/environment_types.go:93](../../api/v1alpha1/environment_types.go)).
But the object those fields live on does not exist until somebody deploys into
it, so the bar can only ever be raised *after* the first release has already
landed. The one moment the requirement matters most is the one moment it cannot
be expressed. `promoteOrFlip` is explicit about this and treats it as fine —
"a fresh one declares no requirements, so the fast path is also the right one"
— which is a reasonable local reading and the wrong global one.

**`EnvironmentType` is a binary, so "staging" has nowhere to live.** The two
values are `production` and `preview`
([api/v1alpha1/environment_types.go:29](../../api/v1alpha1/environment_types.go)),
and `promoteOrFlip` creates a promotion stage's environment as
`EnvironmentProduction` regardless of what the stage is called. Every
non-preview environment on the platform is a production environment as far as
the type says. `PromotionStage` carries a `Name` that is a DNS label precisely
so it "can appear in generated names", and then nothing generates a name from
it.

**Every non-preview environment of a project resolves to one hostname.**
`hostname()` special-cases previews and otherwise returns
`projectHost(project, baseDomain)` — `<project>.<baseDomain>`
([internal/controller/environment_controller.go:1774](../../internal/controller/environment_controller.go)).
A project with a `staging` stage and a `production` stage therefore produces
two Environments, each applying an HTTPRoute named after itself into the same
namespace, both claiming the same hostname on the shared Gateway. Gateway API
resolves that conflict by rule age, so one of them silently wins. *Read from
the code, not reproduced* — but if it holds, a staged pipeline does not
currently publish two addresses, and that should be confirmed with a test
before anything is built on top of it.

Taken together: **the promotion pipeline is a real feature whose environments
were never modelled.** That has been survivable because a claim binds to its
own project's environments, so nothing outside asks an environment what it is.
The moment another project binds to one, it does.

## Finding 3 — an internal service is currently on the internet

`applyHTTPRoute` publishes every Environment on the shared Gateway. The only
thing that suppresses it is a preview refusal (capacity, forks), and the
`publicURL` comment says as much: it is "empty exactly when the environment
gets no route, which is a preview the platform will not publish."

So today a project that exists purely to be called by other applications still
gets `<project>.<baseDomain>` on the public Gateway. In `tls.mode: acme` it
also gets a publicly trusted certificate for it. Production is not gated —
the preview gate is for previews — so the only thing between the internet and
an internal service is whatever authentication the application implements
itself. For a DuckDB container, or a team's internal API that assumed it was
behind the cluster boundary, that is nothing.

**There is one exception, and it is the seed of the fix.** Only the `web`
process is routed: the Environment's Service selects on
`kitchen.bermos.dev/component: web`, and the HTTPRoute names that Service.
Every other *addressed* process gets a Service of its own and an address handed
to its siblings as `KITCHEN_SERVICE_<PROCESS>`
([internal/controller/processes.go:487](../../internal/controller/processes.go),
[api/v1alpha1/process_types.go:575](../../api/v1alpha1/process_types.go)).
So a process that is not `web` is **already internal-only and already
addressable in-cluster** — it is simply only addressable from inside its own
environment. The intra-project half of service topology is built. What is
missing is the cross-project half and the ability to say a whole project is
internal.

## The proposal — an offering, and a claim that names it

### The provider side is a list on Project

```yaml
kind: Project
metadata: {name: pricing}
spec:
  offers:
    - name: pricing-api
      process: api          # defaults to web
      protocol: http        # http | tcp
      auth: gate            # none | gate | oidc
      visibility: request   # request | open
      contract:
        openapi: openapi.yaml   # path in the repo, resolved per release
```

A list beside `spec.processes`, `spec.files` and `spec.access`, not a CRD of
its own. It has no independent lifecycle: an offering is a statement a project
makes about itself, it dies with the project, and there is nothing to reconcile
until somebody claims it.

**The shape may come from `kitchen.json`; the visibility may not.** The repo
config already carries `processes`, `volumes`, `files`, `env`
([internal/repoconfig/file.go](../../internal/repoconfig/file.go)) — which
process serves an offering, on what port, and where its spec lives are facts
about the application, and belong there with the rest. Who may bind to it is a
grant, and a grant that anyone with push access could widen is not a grant. So
`visibility` is a project setting, written through the API by a project admin,
and `kitchen.json` declaring it is a validation error. This is the same split
the platform already draws everywhere else: the repository says what the
application *is*, the platform says who may.

### The consumer side is a claim, and needs no schema change

`ResourceClaim` is already the consumption verb — `projectRef + type +
config → a Secret binding → environment variables` — and `spec.config` is a
`RawExtension` keyed by type, exactly as `{"postgres": {...}}` is today. So:

```yaml
kind: ResourceClaim
spec:
  projectRef: {name: checkout}
  type: service
  config:
    service:
      project: pricing
      offering: pricing-api
```

One new value in the `ClaimTypes` table, one `claimContract` implementation,
and the CRD's enum grows by a word. CLAUDE.md's rule — *a claim type is a new
value in an enum plus a reconcile path or it is nothing* — is satisfied by a
single reconcile path: resolve the offering, check the grant, mint whatever the
auth mode needs, write the binding Secret, write the NetworkPolicy.

The binding reaches the application as `KITCHEN_SERVICE_<NAME>_URL` and
friends. That deliberately reuses the prefix a sibling process already gets:
from inside the application, another team's service and a sibling process are
the same thing — an address it did not have to know. The cost is a collision
when a binding and a process share a name, and the answer is to refuse it at
admission rather than disambiguate silently.

### The authorization ladder decides whether DuckDB works

The auth mode is what makes this one feature rather than three, because a
Go service that can validate a JWT and a DuckDB container that speaks its own
wire protocol need different amounts of help:

| Mode | The consumer gets | The provider must | Works for |
|---|---|---|---|
| `none` | the address, plus a NetworkPolicy that permits the edge | nothing | any protocol — DuckDB, a queue, a raw port |
| `gate` | the address of a forward-auth proxy that fronts the offering | nothing | HTTP only, but no application change at all |
| `oidc` | client id and secret, audience = the offering | validate a JWT | services wanting per-consumer identity |

`gate` is the interesting rung and it is already built: `internal/previewgate`
is a forward-auth proxy the platform runs, which serves every preview on the
platform without knowing about any of them in advance, because the Gateway
hands it the upstream in a header. The same shape keyed to *service* identity
rather than a human session is how a container with no authentication of its
own acquires authorized consumers. `oidc` is nearly free too — `oidcClient` is
an existing claim type that mints clients at the identity provider.

**`none` is not a weak rung once NetworkPolicy is enforced.** Reachability
becomes the authorization, which is the honest model for a database that was
never going to check a bearer token.

### The DuckDB case, concretely

A Project whose `web` process is duckdb-over-HTTP, `exposure: internal` so it
gets no route, an offering with `auth: gate`, `visibility: request`. Consumers
claim it, are approved by the owning team, receive an address and a policy
edge. Nothing about it is special-cased; it is a project like any other that
happens to serve something other than a browser.

### Microservices: many projects, and the correction

Many Projects, not one Project with many processes — the unit of independent
deploy has to be the unit of independent deploy, and `spec.processes` gives
one build, one image, one release, one access list.

But the split is less stark than it first looks, and the reason is Finding 3:
addressed processes already get their own Services and are already handed each
other's addresses within an environment, and only `web` is published. So a
handful of services that release together, share an access list and are
previewed as one unit is *correctly* one Project with several processes today,
and the preview story for that shape is already right — the comment at
`environment_controller.go:344` is explicit that a preview's web process
reaches the preview's own API, "which is the whole of what makes a preview of
a multi-workload unit a preview of the unit rather than of one quarter of it
pointed at production."

The line is release cadence and ownership, not count. Services that ship
together are processes; services that ship apart are projects.

## Environments, properly

The feature above cannot be built on inferred environments, because a binding
has to name one and a preview has to be told which one it may reach. Three
changes, in dependency order:

**1. An Environment can be created before anything deploys into it.** A route
and a screen, operator- and environment-owner-gated like the requirements
endpoint already is. `ensureEnvironment` stays exactly as it is for the
environments nothing declared — the platform's existing behaviour is the
default, not a thing to be removed.

**2. The type vocabulary grows a middle.** `production | preview` becomes
something like `production | stage | preview`, with a promotion stage's
environment created as `stage`, and `hostname()` deriving
`<project>-<environment>.<baseDomain>` for a stage. That fixes the hostname
collision in Finding 2 as a side effect, and it is a breaking change to
generated URLs for anybody already running a staged pipeline — which pre-1.0
is the right time to take.

**3. An environment declares who may bind to it.** This is the answer to the
preview-binding question, and it belongs to the environment's owners for
exactly the reason the requirements do:

```yaml
kind: Environment
spec:
  serves:
    consumers: [preview]     # which classes of consumer environment may bind here
```

- A production environment declares `[production]`, so a preview can never
  bind to it.
- The staging environment a team keeps for integration declares `[preview]`,
  or `[production, preview]`.
- An environment that declares nothing serves nothing — an environment nobody
  has rated must not be one that whoever deploys into it can point previews at.

The claim then picks among the environments that admit it, and the default is
the one the offering names. This is the same shape as `Requirements`: the
environment's owners set the ceiling, the deploying team chooses beneath it and
cannot raise it. The existing `DataClass` refusal composes on top unchanged —
a classified project's preview binding to an environment rated below it is
already a refusal the policy engine knows how to make.

## Internal exposure

`Project.spec.exposure: public | internal`, defaulting to `public`, inherited
by every environment of the project. `internal` means no HTTPRoute, no
hostname on the shared Gateway, and no certificate. Consumers reach it through
the address in their binding.

Four things it touches, all of which need deciding rather than discovering:

- **`KITCHEN_URL`** is documented as empty exactly when an environment gets no
  route. An internal environment gets no route but is not unreachable, so
  either the variable stays empty and the sentence acquires a second meaning,
  or it carries the in-cluster address. The second is more useful and the
  first is more honest; the in-cluster address is also what a consumer's
  binding already carries, so the variable is not the only way to learn it.
  Leaning empty, and saying why.
- **`oidcClient` claims pre-register redirect URIs from `projectHost`** before
  the Environment exists. An internal project has no browser flow and no
  redirect URI; the contract must not fail on one, and probably should refuse
  the combination outright.
- **Custom `Domain`s must be refused** on an internal project rather than
  quietly creating the route `exposure: internal` exists to prevent.
- **Scale to zero probably does not survive it.** The KEDA HTTP interceptor
  routes on the visitor's `Host` header, which works because everything in
  front of it preserves that header. A consumer connecting straight to a
  Service address never passes through the interceptor, so an internal
  environment reached that way cannot be woken by traffic. It may be possible
  to point the binding at the interceptor with the right Host — that is
  untested and should be treated as unknown, not as a plan. Until then, the
  honest position is that an internal offering does not idle, and the platform
  says so rather than parking a service nothing can wake.

## NetworkPolicy

[docs/SCOPE.md](../SCOPE.md) deprioritises inter-application policies on
trusted-team grounds, and as a *security* argument that reasoning is sound and
does not need overturning. The argument for doing it now is a different one:

**A declared topology that is not enforced is aspirational.** If A can reach B
without a binding, then "who consumes my service" is a guess, "what breaks if
I delete this project" is a guess, and the approval flow above is theatre —
a team could deny a binding and be called anyway. Enforcement is what makes
the graph *true*, and the graph is the product here. That is a legibility
argument, not a threat model, and it survives everybody trusting each other.

Shape:

- Plain `networking.k8s.io/v1`, as the platform namespace's policy already is.
  Cilium is a prerequisite and enforces it. `CiliumNetworkPolicy` would allow
  enforcing the OpenAPI's own paths at L7, which is the ceiling this could
  reach later; it ties the platform to Cilium CRDs and is not worth it yet.
- **Per environment, not per project.** A preview is a different set of edges
  from production — a preview binds to staging where production binds to
  production — and a project-scoped policy cannot express that.
- Default-deny between application namespaces, allow per declared edge, plus
  the platform edges that already exist (the Gateway in, the telemetry
  receiver and object store out).
- `observe | enforce`, per project. Not because a long migration is needed —
  it is not, at this size — but because "my application cannot reach X" is
  precisely the class of question that ends up on the operator's desk, and
  `observe` plus the diff below is the screen that answers it without one.

## Topology

The declared topology is the edges: claims to resources, bindings to
offerings, projects to environments, environments to domains. It is a graph
read off objects that already exist plus the new edge, and needs no object to
hold it.

The observed topology is the interesting half, and it is nearly free: the
platform already runs an OTLP collector into ClickHouse. Diffing the two gives
two findings worth a screen each:

- **Undeclared edges** — A calls B with no binding. Today this silently works.
  Under enforcement it breaks. Showing it before enforcing is the whole
  migration, and afterwards it is the debugging surface.
- **Dead edges** — a binding nothing has used in thirty days. Blast radius
  that can be deleted, which is the only kind of blast radius anybody actually
  removes.

## The OpenAPI specification, and #187

The specification is a **release artifact**, not a project field. A field on
the Project drifts from what is running; an artifact attached to the Release is
true by construction, because the Release is what is running.

That makes it the right first case for
[#187](https://github.com/Bermos/Kitchen/issues/187) — publishing a project's
documentation with the release it was written for — rather than a separate
mechanism that later has to be reconciled with it. It earns the role on two
counts:

- **It has the strictest freshness requirement of anything #187 would carry.**
  A README that is a release behind is untidy; a specification that is a
  release behind is wrong, and other teams are writing code against it.
- **It has a machine-checkable shape and a concrete consumer.** The service
  catalogue renders it, so the mechanism is proved by something that breaks
  visibly rather than by a documentation page nobody opens.

Extraction is a path in the repository resolved at build time — an input to the
build record, reproducible, and attributable to a commit — rather than scraped
from a running environment, which would make it an observation of the platform
instead of a fact about the source.

## Deferred — contract versioning

Shelved deliberately. A team that versions its API properly does not break
consumers on deploy, and the platform enforcing what a team is already doing
correctly buys little.

Recorded because the hook is left in place for free: once the specification is
a release artifact, the diff between the serving release's spec and a candidate
release's spec is computable, and a removed path or a narrowed type is a
mechanically detectable breaking change. The enforcement point, when it is
wanted, is the *provider's* promotion gate rather than the consumer's pin —
unlike a library, a service cannot be pinned by its consumer, because only the
provider can serve two versions at once. `EnvironmentRequirements`, the
promotion stages and `Exception` (with the consumers as the natural approvers)
are already the right machinery. Nothing needs to be built now to keep that
door open; it is enough not to put the specification somewhere it cannot be
diffed.

## The monorepo fan-out

`internal/receiver/receiver.go:754` matches every project on a
connection-and-repository pair, so a push to a monorepo of eight services
builds and deploys all eight. `rootDirectory` exists, so the build side is
correct; the trigger side has no filter.

The compliance instinct — rebuild everything so the platform is never out of
sync with the repository — is not wrong, but it does not survive contact with
the record it is trying to protect. Seven of the eight releases changed
nothing, and a release history where seven rows in eight are no-ops is a
*worse* audit trail, not a better one: the question "when did this service last
change" stops being answerable by looking.

**The exact answer is neither "always build" nor a path filter.** A path
filter has to be maintained by hand and lies whenever a shared library outside
`rootDirectory` changes. Git already holds the fact:

> The tree object at `<commit>:<rootDirectory>` is the identity of the source
> this project builds. If it equals the one the last build used, the source is
> byte-identical, and there is nothing to build.

That is exact, needs no reproducible builds, costs one `rev-parse`, and cannot
drift because it is derived rather than declared — the same instinct as
reading the installed chart versions off the install job every pass rather than
remembering them.

The skip is **recorded, not silent**: a `Build` in a `Skipped` state naming the
commit and the tree object it matched. That is a stronger record than a rebuild
produces, because it is an assertion about the source ("this commit changed
nothing here") rather than an artifact that may or may not be byte-identical
for reasons nobody controls.

It should be opt-in per project at first (`build.skipUnchanged`), because a
monorepo whose services genuinely share code outside their root directories
wants the fan-out, and the platform cannot tell which it is looking at.

## Where it lands in the dashboard and the CLI

**Fleet scope, as a subsection.** A service catalogue is cross-project but
developer-facing, so it cannot go in Platform scope — that is the operator's,
and [docs/UI.md](../UI.md)'s rule is that what is on a screen is decided by
what the screen is about, not by who is reading. Fleet is already "everything
across the projects you can see". No fifth scope:

- `/services` — the catalogue. What exists, who owns it, what it serves, and a
  Request access action on the ones a project is not bound to.
- `/services/:project/:name` — the offering: its specification at the release
  serving each environment, its consumers, its edges.
- `/topology` — declared against observed.
- The provider's own project screen grows the pending requests, approved by a
  project admin. A requested binding is a claim in a `PendingApproval` phase,
  so approval is a claim transition rather than a second approval system beside
  `Exception`.

**CLI**, per the third-client rule: `kitchen services`, `kitchen services show
<name> --spec`, `kitchen bind`, `kitchen topology --json`. The topology is the
one worth designing rather than deriving — an edge list as NDJSON, one object
per edge, is what makes it pipeable into anything.

## What to build first

1. **Environments become declarable**, with a middle type and per-environment
   hostnames. Nothing below is honest without it, and it fixes a hostname
   collision that exists today.
2. **`spec.offers` plus `ResourceClaim{type: service}` with `auth: none`**,
   delivering address variables and a NetworkPolicy edge. `Environment.serves`
   decides what a preview may bind to.
3. **`exposure: internal`**, which is what makes an offering a service rather
   than a public website with extra steps.
4. **Default-deny between application namespaces**, `observe` first, `enforce`
   immediately after.
5. **The specification as a release artifact**, with #187, and the Fleet-scope
   catalogue that renders it.
6. **Observed topology from ClickHouse**, diffed against the declared one.

## Open questions

- **Whether any grouping object is needed at all, and when.** Two are
  plausible and they are different objects: a **Team**, which is ownership and
  permission and which [docs/API.md](../API.md) already has an entry for under
  Open (the identity provider's organizations plugin is the implementation); and
  a **System**, which is topology and lifecycle — a set of projects staged and
  previewed together. A system spans teams; a team owns systems that never
  speak. Collapsing them gives either a team that cannot share or a system
  whose permissions are wrong. Neither is needed for anything above, because
  the edges carry the weight; the case for **System** gets strong exactly when
  somebody wants a preview of one service against previews of the other seven,
  which is the one thing an edge cannot express.
- **Whether a project with consumers can be deleted.** The project finalizer is
  the garbage collector and the dashboard confirms by typing the name; it now
  needs "three projects bind to this" as a refusal or an explicitly confirmed
  cascade. Same rule, larger blast radius.
- **Whether an offering can name an environment other than its default per
  consumer.** Two consumers wanting different environments of one offering is
  either a real need or a smell; unknown, and cheap to add later.
- **Whether the interceptor can be addressed by a binding**, which decides
  whether an internal offering can idle. Untested.
- **Whether the staging/production hostname collision is real.** Read from the
  code, not reproduced. It should be a test before it is a fix.
