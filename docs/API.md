# Kitchen — Operator REST API

The surface the Kitchen UI, the CLI and CI talk to, served by the operator
alongside the git webhook receiver and published on the shared Gateway at
`https://kitchen.<baseDomain>/api/v1/`.

It is a view onto the same custom resources the controllers reconcile — there
is no second copy of the platform's state. Listing projects reads `Project`
objects; rolling an environment back writes one field of an `Environment`.

## Authentication

Every endpoint is behind the platform's identity provider ([AUTH.md](AUTH.md)).
There is no unauthenticated mode, no local-admin escape hatch, and no
read-only exception: a request without a valid bearer token gets `401` with a
`WWW-Authenticate: Bearer` challenge. An installation running with
`auth.enabled=false` has no issuer, so every endpoint answers 401 — the API
does not fall open when the thing that guards it is missing.

Validation is stateless. The operator fetches the issuer's JWKS once, verifies
the signature, the issuer, the expiry and the audience, and keeps no session.
Key rotation needs no restart: an unknown key id refetches the JWKS.

Both the issuer and the accepted audiences come from the `Kitchen` singleton,
so nothing has to be configured twice:

| | |
|---|---|
| Issuer | `spec.auth.host`, defaulting to `auth.<baseDomain>` |
| Accepted audiences | the issuer, and `spec.api.externalURL` (defaulting to `https://kitchen.<baseDomain>`) |

`--api-audiences` adds more, for installations that mint tokens under another
name.

### Getting a token

**A person, through the UI.** The UI is an OAuth client (Authorization Code +
PKCE, client id `kitchen-ui`), registered by the chart on the first start of
the auth service. It asks the token endpoint for a token for the API by name:

```
POST https://auth.<baseDomain>/oauth2/token
grant_type=authorization_code&client_id=kitchen-ui&code=…&code_verifier=…
&resource=https://kitchen.<baseDomain>
```

The `resource` parameter is what makes the access token a JWT with the API as
its audience. Without it the provider issues an opaque token, which the
operator cannot validate and will refuse.

**CI, with an API key.** API keys are the identity provider's (better-auth's
api-key plugin) — see the decision below. The key is exchanged for a
short-lived JWT at the issuer, and the API sees only the JWT:

```sh
TOKEN=$(curl -sS -H "x-api-key: $KITCHEN_API_KEY" \
  https://auth.apps.example.com/token | jq -r .token)

curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects
```

That token's audience is the issuer, which the API accepts. The key itself
never reaches the operator, so a leaked API key is revoked in one place and
the operator has nothing to invalidate.

## Endpoints

All paths are relative to `/api/v1`. Collections answer `{"items": [...]}`;
errors answer `{"error": "..."}` with a message meant to be read by whoever
sent the request.

| Method | Path | Does |
|---|---|---|
| GET | `/projects` | List projects |
| POST | `/projects` | Create a project |
| GET | `/projects/{name}` | One project |
| GET | `/projects/{name}/builds` | That project's builds, newest first |
| POST | `/projects/{name}/builds` | Build a commit — a rebuild |
| GET | `/projects/{name}/releases` | That project's releases, newest first |
| GET | `/projects/{name}/environments` | That project's environments |
| GET | `/builds` | Every build. `?project=` filters |
| GET | `/builds/{name}` | One build |
| GET | `/builds/{name}/logs` | That build's output |
| GET | `/releases` | Every release. `?project=` filters |
| GET | `/releases/{name}` | One release |
| GET | `/environments` | Every environment. `?project=` filters |
| GET | `/environments/{name}` | One environment |
| PATCH | `/environments/{name}` | Move it to another release — promotion and rollback |
| GET | `/environments/{name}/logs` | That environment's runtime logs |
| GET | `/environments/{name}/workload` | What it is running: replicas, restarts, uptime, resources, pods |
| GET | `/environments/{name}/objects` | The Kubernetes objects the operator materialized for it |
| GET | `/logs` | The whole logs table, filtered by a ClickHouse expression |
| GET | `/events` | The platform's recent activity, newest first. `?project=` and `?limit=` filter |
| GET | `/metrics/overview` | The dashboard's numbers, pre-aggregated. `?project=` narrows |
| GET | `/traffic` | The service map: aggregated flow edges. `?project=`, `?since=`, `?until=` |
| GET | `/status` | The platform as it is running: cluster, tunnel, build queue, components |
| GET | `/settings` | The platform's settings — the `Kitchen` singleton |
| PATCH | `/settings` | Change the build and telemetry defaults |
| GET | `/updates` | The platform's own version, what it can upgrade to, and every upgrade it has attempted |
| POST | `/updates` | Upgrade the platform |
| GET | `/updates/{name}` | One upgrade |
| GET | `/connections` | Every connection (never their credentials) |
| GET | `/connections/{name}` | One connection |
| GET | `/domains` | Every custom domain. `?environment=` filters |
| GET | `/domains/{name}` | One domain |
| GET | `/claims` | Every resource claim. `?project=` filters |
| GET | `/claims/{name}` | One claim |

Creating connections, domains and claims is not here yet: those are the flows
the UI will drive, and they are worth designing against a UI rather than ahead
of one. Until then they are `kubectl apply`, the same as they were.

### Creating a project

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop", "repo": "acme/shop", "connection": "gh", "registry": "harbor"}' \
  https://kitchen.apps.example.com/api/v1/projects
```

A project is a name, a repository in the provider's `owner/name` form, and the
two Connections it builds and stores images with — `connection` needs the
`gitSource` capability, `registry` needs `imageStore`. Optional fields with
their defaults:

```json
{"productionBranch": "main", "previews": true}
```

The name has to work as a DNS label of at most 46 characters, because
everything the platform derives from it — the application namespace, release
names, generated hostnames — has to fit Kubernetes' 63-character limit.
Naming a Connection that does not exist, or one without the needed
capability, is a `400`; a Connection the operator has not assessed yet is
accepted, and the project's own conditions report whether it fits. A name
already in use is a `409`.

Answers `201` with the new project. The operator takes it from there:
namespace, webhook, and — once the first build of the production branch
lands — the production environment.

### Triggering a build

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/builds
```

An empty body rebuilds the commit the project built last — a rerun after a
flaky build or a changed secret. To build a particular commit:

```json
{"sha": "abc123def456789", "branch": "main"}
```

The branch may be left out for a commit that has been built before; for one
that has not, it falls back to the project's production branch. Builds are
immutable, so a rebuild is always a new `Build` with a generated name
(`shop-bld-abc123def456-xk2p9`) rather than a mutation of the old one.

Answers `201` with the new build.

### Rolling back

Rollback is not a special operation. A `Release` is an immutable snapshot of an
image digest and the configuration it runs with, so pointing an `Environment`
at an older one puts back exactly what was running:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"release": "shop-rel-41"}' \
  https://kitchen.apps.example.com/api/v1/environments/shop-production
```

The release has to belong to the same project as the environment; anything else
is a `400`. Promotion is the same call with a newer release.

Each move is remembered. The environment's `history` lists the releases that
stopped being current, newest first: which release, when it was current
(`from`/`to`), how it stopped (`reason`) and who moved the environment off it
(`by` — the authenticated caller for API moves, the promoting build for
automatic ones):

```json
{"history": [{"release": "shop-rel-42", "from": "2026-08-13T09:12:00Z",
  "to": "2026-08-14T10:30:00Z", "reason": "rolledBack", "by": "ada@example.com"}]}
```

`reason` is `promoted` when a fresh build's release was auto-promoted over it,
`rolledBack` when the environment was moved back to an older release, and
`superseded` when another release replaced it any other way.

### What an environment is running

An `Environment`'s phase says whether it is live. `GET
/environments/{name}/workload` says what "live" is made of, read out of the
cluster at request time:

```json
{"environment": "shop-production", "namespace": "kitchen-shop",
 "deployment": "shop-production", "image": "registry.example.com/shop@sha256:…",
 "replicas": {"desired": 3, "ready": 3, "available": 3, "updated": 3},
 "restarts": 2, "startedAt": "2026-08-14T09:12:00Z",
 "resources": {"cpuRequest": "100m", "memoryRequest": "128Mi", "memoryLimit": "256Mi"},
 "pods": [{"name": "shop-production-7d9f…", "phase": "Running", "ready": true,
   "restarts": 2, "node": "node-1", "startedAt": "2026-08-14T09:12:00Z"}]}
```

`replicas` is the "3 of 3" the dashboard shows; `restarts` sums every restart
across the environment's pods, which is the number that tells a crash loop
from a slow start. `startedAt` is the oldest *running* pod, so uptime is the
workload's rather than the `Environment` object's, and a pod replaced a minute
ago does not reset it. A pod that is not serving carries the reason in
`message` — the waiting reason (`CrashLoopBackOff`, `ImagePullBackOff`) or the
exit that ended its last run, which is the line `kubectl describe` is usually
opened for.

An environment with nothing materialized — a preview whose route is withheld,
one the reconciler has not reached — answers `200` with no `deployment` and a
`message` saying so. That is a state, not an error; the environment's own
conditions carry why.

### The objects the operator materialized

`GET /environments/{name}/objects` answers with the Kubernetes objects behind
an environment — the `Deployment`, the `Service` and the `HTTPRoute` — as the
API server holds them. It is the dashboard's operator mode surfacing what the
reconciler did, so the objects are deliberately *not* translated into the API's
own vocabulary: whoever opens this wants the manifest, and a summary would send
them to a terminal anyway.

```json
{"environment": "shop-production", "namespace": "kitchen-shop", "objects": [
  {"kind": "Deployment", "apiVersion": "apps/v1", "name": "shop-production",
   "namespace": "kitchen-shop", "present": true, "manifest": {"kind": "Deployment", "…": "…"}},
  {"kind": "HTTPRoute", "apiVersion": "gateway.networking.k8s.io/v1", "name": "shop-production",
   "namespace": "kitchen-shop", "present": false, "message": "not materialized"}]}
```

Every expected object is listed whether or not it exists: `present: false` is
the answer to most of the questions this endpoint gets asked — a preview with
no route, an environment stuck before its Service. `manifest` keeps `status`,
which is where the Gateway says whether it accepted the route, and drops the
bookkeeping (`managedFields`, the last-applied annotation) no reader of a
manifest wants.

### Platform status

`GET /status` is the platform as it is *running*, where `/settings` is the
platform as it is *configured*. It is one request because it answers one
question — the dashboard's status bar:

```json
{"cluster": {"name": "chef", "nodes": 8, "readyNodes": 8},
 "tunnel": {"enabled": true, "connected": true, "message": "cloudflared is available"},
 "builds": {"running": 1, "capacity": 2, "queued": 0},
 "gateway": {"address": "203.0.113.7", "programmed": true},
 "components": [{"name": "logs", "kind": "DaemonSet", "healthy": false,
   "available": 0, "desired": 3, "message": "0 of 3 pods available: …"}]}
```

`cluster.name` is `spec.clusterName` on the `Kitchen` singleton, falling back
to the first label of the base domain — Kitchen owns the cluster it is
installed into, so naming it names the installation. `builds` is what the build
controller's concurrency gate is weighing: builds running against
`spec.builds.concurrency`, and how many are waiting for a slot. `components` is
the operator's own survey of every workload labelled
`app.kubernetes.io/part-of: kitchen`, which is the only place a workload whose
pods were refused at admission shows up at all — it has no pods to look at.

A node count the operator's ClusterRole does not allow comes back as zero with
the reason in `cluster.message`, rather than failing the request: an
installation upgraded from before this endpoint should not lose its whole
status bar over the one line it cannot fill in.

### Settings

`GET /settings` is the `Kitchen` singleton as a view: the base domain, the
derived API and issuer URLs, the gateway's address and conditions, and the
defaults the platform builds and retains telemetry with.

`PATCH /settings` changes the fields that are safe to change at runtime:

```json
{"buildStrategy": "auto", "buildConcurrency": 2, "logRetentionDays": 30}
```

Fields left out stay as they are. Everything else on the singleton — the base
domain, the issuer, the ingress — shapes URLs and credentials the platform has
already handed out, so changing those stays a deliberate kubectl operation.

### Updating the platform

`GET /updates` answers what the installation is running, what has been
published since, and what it has already attempted:

```json
{
  "enabled": true,
  "currentVersion": "0.2.0",
  "latestVersion": "0.3.0",
  "available": true,
  "upgradableTo": ["0.2.2", "0.2.1"],
  "allowMinor": false,
  "items": [{"name": "update-0-2-1-h4k9c", "version": "0.2.1", "phase": "Succeeded", "fromVersion": "0.2.0"}]
}
```

`upgradableTo` is what this installation would actually accept, so it is not
simply everything newer: `latestVersion` here is `0.3.0` while the offer stops
at `0.2.2`, because `allowMinor` is false and pre-1.0 the minor is where
breaking changes land. `enabled` is false on an installation whose chart was
not installed with `selfUpdate.enabled=true`, and `reason` then says so — the
running version is still reported, because that is the first thing anyone
asks. An installation that cannot reach the chart registry gets
`discoveryError` and no candidates, and can still be given a version by hand.

`POST /updates` starts one:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"version": "0.2.1"}' \
  https://kitchen.apps.example.com/api/v1/updates
```

A version is the only field, and an unknown field is a `400` rather than
something ignored. That is not tidiness: the job that runs the upgrade holds
cluster-admin, so an endpoint that forwarded helm arguments would be a way to
apply anything at all with it. The operator builds the whole `helm upgrade`
invocation itself.

The answer is `201` with the created upgrade; watch it with
`GET /updates/{name}` — or watch the version in the sidebar, which changes when
the new operator comes up. `409` means self-update is not enabled on this
installation. Everything else the platform refuses — a downgrade, a version it
is already on, a minor crossing without `selfUpdate.allowMinor`, a second
upgrade while one is in flight — is accepted here and refused by the operator,
which records the reason on the `PlatformUpdate` rather than losing it: the
checks are about the state of the cluster at the moment the job would start,
not about the request.

Requests are attributed: the caller's name is annotated onto the object and
reported as `requestedBy`.

See [Letting the platform update itself](../charts/kitchen/README.md#letting-the-platform-update-itself)
for what enabling it grants.

### Logs

Build and runtime logs come from ClickHouse, where the collector has been
shipping them since the log pipeline landed — so a build's output survives the
build pod, and a preview's logs outlive the preview.

```
GET /builds/{name}/logs?limit=200&search=error&since=2026-08-13T10:00:00Z
GET /environments/{name}/logs?limit=200&container=app
```

| Parameter | Meaning |
|---|---|
| `limit` | Lines to return, default 200, capped at 5000. The *newest* lines are kept |
| `since` / `until` | RFC 3339 bounds |
| `search` | Case-insensitive substring of the message |
| `container` | One container of the pod |

Lines come back oldest first — a log reads forwards — as
`{timestamp, source, project, environment, build, pod, container, stream, level, message}`.
`level` is the collector's best-effort read of the line's severity
(`trace`/`debug`/`info`/`warn`/`error`/`fatal`, parsed out of JSON logs and the
common text spellings), empty when the line says neither.

`source` says whose the line is. The collector tails every container on every
node, so this is a real distinction and not a formality:

| `source` | What it is |
|---|---|
| `build` | A build job's output |
| `runtime` | A deployed app, or anything else in a project's namespace |
| `platform` | Kitchen's own components, in `kitchen-system` |
| `cluster` | Everything else the cluster runs — the CNI, CSI sidecars, whatever was installed alongside |

`cluster` lines are collected deliberately: a node whose storage or networking
is failing is exactly when Kitchen looks broken, and the answer is in someone
else's pod. The dashboard scopes them out by default and offers a switch.

An installation without a telemetry store answers `503`: there are no logs to
read, which is a missing capability rather than a bad request.

### Following logs live

The same endpoints stream when asked to, negotiated by Accept:

```
GET /builds/{name}/logs
Accept: text/event-stream
```

The answer is Server-Sent Events: the query's current page first, then every
line that arrives after it as its own `data:` event (the same JSON shape as
above), until the client closes the connection. `/logs` streams too, with its
`where` expression applied to every new line. A plain GET on the same URL
still answers the bounded page, so nothing changes for callers that do not
ask. The UI tails builds and the observability view this way and falls back
to polling when the stream drops.

### Querying logs with ClickHouse syntax

The logs live in ClickHouse, and the API does not pretend otherwise. `/logs`
takes a real ClickHouse boolean expression over the table's columns and
evaluates it as written — the observability view's query bar is this endpoint:

```
GET /logs?where=project = 'shop' AND stream = 'stderr' AND message ILIKE '%timeout%'
GET /logs?where=match(message, 'GET /works\?page=\d+') AND environment = 'shop-production'
```

`limit`, `since` and `until` work as above and are applied on top of the
expression; `where=1 = 1` selects everything in the window. A refused
expression — a typo, an unknown column — answers `400` carrying ClickHouse's
own diagnostic, which is the message that says how to fix it.

The expression reaches ClickHouse as query text, which is the point — and why
it runs pinned read-only (`readonly=2`: no writes, no DDL) under an execution
cap, as the operator's own database user. What that user can read is the
telemetry database; per-caller scoping arrives with scopes and RBAC
([open item](#open)).

### The activity feed

`GET /events` answers what the platform did recently, newest first: builds
finishing, releases moving, previews coming and going.

```
GET /events?project=shop&limit=50&since=2026-08-13T00:00:00Z
```

Entries are
`{timestamp, type, project, environment, build, release, claim, message, actor, value}` —
the object fields name what the entry is about so a client can link to it,
`actor` is the authenticated caller for API-driven changes and `operator` for
things the reconcilers decided on their own, and `value` carries the one
number some events have (a finished build's duration in seconds). Types:
`build.succeeded`, `build.failed`, `release.promoted`, `release.rolledBack`,
`preview.created`, `preview.removed`, `project.created` (plus `claim.bound` /
`claim.failed` once claims bind).

The feed is written by the reconcilers and the API into the events table of
the telemetry store, under the same retention as the logs. Kubernetes Events
were deliberately not the source of truth: they expire in an hour and carry
machinery noise the feed would have to filter back out.

### Metrics

`GET /metrics/overview` answers the dashboard's numbers pre-aggregated, in
one shape:

- deploys over 7 days and a per-day series, plus the median build time, from
  the activity feed
- requests, error rate and p95 latency over 24 hours with per-hour series,
  from the flow pipeline — zeroes when no flow collector is configured
- log volume over 24 hours with a per-hour series
- the store's own size and ingest rate
- `projects`: per-project 24h traffic (requests, 5xx, p95, hourly series)

`?project=` narrows everything to one project and drops the `projects` join.
There is deliberately no raw metrics query surface: the raw material is the
logs, events and flows tables, and `/logs` already exposes the store's own
syntax for ad-hoc questions.

### Traffic

`GET /traffic` answers the service map: one edge per (source workload,
destination workload) pair the flow collector saw in the window
(`?since=`/`?until=`, defaulting to the last hour; `?project=` narrows to
edges touching the project's namespace).

Edges are
`{source, sourceNamespace, destination, destinationNamespace, protocol, flows, rps, errors, drops, p95Ms}`.
HTTP edges carry status-derived `errors` and `p95Ms`; edges without L7
visibility carry connection counts and drops alone. The data comes from
Cilium's Hubble Relay, which the operator follows when
`Kitchen.spec.observability.hubble.relayAddress` names it; without that the
endpoint answers an empty list and the traffic view explains what to enable.

## Status codes

| Code | When |
|---|---|
| `200` / `201` | Fine |
| `400` | The request cannot be carried out as written |
| `401` | No valid token — including when the platform has no identity provider |
| `403` | The operator's own service account may not do this |
| `404` | No such object, or no such endpoint |
| `409` | Someone else changed the object first |
| `503` | A capability this endpoint needs is not installed |

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Token validation | Stateless, against the issuer's JWKS | No session state in the operator; the identity provider stays swappable |
| Token audience | The API's own URL (`resource=`), or the issuer | A resource server should be able to tell a token meant for it from a token meant for everything |
| CI tokens | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's own credential; keeping key lookup at the issuer keeps the operator's request path stateless |
| Response shapes | The API's own vocabulary, not raw custom resources | A stable contract for the UI, and freedom to change how state is stored |
| Write surface | Create project, rebuild, promote/rollback, and the settings' runtime defaults | The writes the UI drives today; creating connections, domains and claims waits for the flows they belong to |
| Introspection shapes | Kubernetes' own vocabulary — replicas, restarts, manifests | The exception that proves the rule above: these endpoints exist to explain the platform's machinery, and a reader comparing them against `kubectl` should not have to translate |
| Pods and nodes | Read uncached, straight from the API server | Serving them from the manager's cache would mean an informer over every pod in the cluster, kept warm for a question only an open dashboard asks |
| The dashboard | Served by the same process, outside `/api/` | The SPA is public, stateless files plus `/config.json` (issuer, client id, audience — the same values every login redirect shows); everything with state stays behind the token check |
| Webhook receiver | Stays signature-authenticated, not OIDC | A provider proving a payload is genuine is a different question from a caller proving who they are |

## Open

- **Scopes and RBAC.** Tokens carry their scopes and the API records who asked
  for what, but nothing is enforced beyond "the issuer vouches for you". Teams
  and per-organisation roles land with the organizations plugin, and the token
  shape follows them.
- **Paging.** Collections answer in full. `{"items": …}` is an object rather
  than a bare array so a cursor can be added without breaking clients.
