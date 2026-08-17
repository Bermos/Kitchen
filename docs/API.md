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
| PATCH | `/projects/{name}` | Change its settings — branch, previews, build, env vars, runtime |
| DELETE | `/projects/{name}` | Delete it, and everything derived from it |
| GET | `/projects/{name}/builds` | That project's builds, newest first |
| POST | `/projects/{name}/builds` | Build a commit — a rebuild |
| GET | `/projects/{name}/releases` | That project's releases, newest first |
| GET | `/projects/{name}/environments` | That project's environments |
| GET | `/builds` | Every build. `?project=` filters |
| GET | `/builds/{name}` | One build |
| POST | `/builds/{name}/cancel` | Stop it — the Build stays, phase `Cancelled` |
| GET | `/builds/{name}/logs` | That build's output |
| GET | `/releases` | Every release. `?project=` filters |
| GET | `/releases/{name}` | One release |
| GET | `/environments` | Every environment. `?project=` filters |
| GET | `/environments/{name}` | One environment |
| PATCH | `/environments/{name}` | Move it to another release — promotion and rollback |
| DELETE | `/environments/{name}` | Tear down a stuck preview. Previews only |
| GET | `/environments/{name}/logs` | That environment's runtime logs |
| GET | `/environments/{name}/workload` | What it is running: replicas, restarts, uptime, resources, pods |
| GET | `/environments/{name}/metrics` | What it *has been* running: CPU, memory, replicas and restarts over a window |
| GET | `/environments/{name}/requests/summary` | The golden-signal header: traffic, error rate and latency over a window |
| GET | `/environments/{name}/requests/series` | The same signals over time — the charts |
| GET | `/environments/{name}/requests/routes` | One row per route template, sortable — the per-path breakdown |
| GET | `/environments/{name}/requests` | The requests themselves, newest first. Filterable, and live-tails like logs |
| GET | `/environments/{name}/diagnostics` | The crash report: everything about the last abnormal termination, assembled |
| GET | `/environments/{name}/objects` | The Kubernetes objects the operator materialized for it |
| GET | `/logs` | The whole logs table, filtered by a query. `?q=`, `?where=` |
| GET | `/logs/histogram` | The same selection counted over time — the shape of the window |
| GET | `/logs/facets` | The same selection's distinct values per field, with counts |
| GET | `/logs/patterns` | The same selection's messages collapsed into templates |
| GET | `/logs/saved` | Saved queries — selections someone kept under a name |
| POST | `/logs/saved` | Keep the current selection under a name |
| DELETE | `/logs/saved/{name}` | Forget one |
| GET | `/events` | The platform's recent activity, newest first. `?project=` and `?limit=` filter |
| GET | `/metrics/overview` | The dashboard's numbers, pre-aggregated. `?project=` narrows |
| GET | `/traffic` | The service map: aggregated flow edges. `?project=`, `?since=`, `?until=` |
| GET | `/traces` | Traces in a window. `?project=`, `?environment=`, `?service=`, `?errors=1`, `?minDuration=` |
| GET | `/traces/{traceId}` | One trace's spans, oldest first — the waterfall |
| GET | `/status` | The platform as it is running: cluster, tunnel, build queue, components |
| GET | `/settings` | The platform's settings — the `Kitchen` singleton |
| PATCH | `/settings` | Change the build and telemetry defaults |
| GET | `/updates` | The platform's own version, what it can upgrade to, and every upgrade it has attempted |
| POST | `/updates` | Upgrade the platform |
| GET | `/updates/{name}` | One upgrade |
| GET | `/connections` | Every connection (never their credentials) |
| POST | `/connections` | Create one — the credential goes in, and never comes back out |
| POST | `/connections/test` | Try a credential against its provider, storing nothing |
| GET | `/connections/{name}` | One connection |
| PATCH | `/connections/{name}` | Rotate the credential, change the config, or both |
| DELETE | `/connections/{name}` | Delete it, unless something still uses it |
| GET | `/domains` | Every custom domain. `?environment=` filters |
| POST | `/domains` | Attach one — the response carries the DNS record to create |
| GET | `/domains/{name}` | One domain, verification instructions included |
| DELETE | `/domains/{name}` | Detach it; the operator removes its certificate |
| GET | `/claims` | Every resource claim. `?project=` filters |
| POST | `/claims` | Ask a database-capable connection to provision one |
| GET | `/claims/{name}` | One claim |
| DELETE | `/claims/{name}` | Delete it — what happens to the data is its `deletionPolicy`'s call |

### Creating a claim

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-db", "project": "shop", "connection": "neon", "type": "postgres", "previewBranching": true}' \
  https://kitchen.apps.example.com/api/v1/claims
```

A claim asks a Connection with the `database` capability to provision a
resource for a project; the reconciler writes the credentials into a binding
secret that `Project.spec.env`'s `fromClaim` references, and the API never
reads them back. `previewBranching` gives every preview environment its own
database branch. `deletionPolicy` (`Retain`, the default, or `Delete`) decides
what deleting the claim later does to the provisioned database — `Retain` is
the default because destroying data has to be asked for, never implied.
Deleting a claim answers `202`: the operator's finalizer still has branches,
binding secrets and — under `Delete` — the database itself to remove.

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

### Changing a project's settings

`PATCH /projects/{name}` edits what a project already is. Every field is
optional and absent ones keep their value:

```json
{"productionBranch": "trunk", "previews": true, "previewsProtected": false,
 "buildStrategy": "dockerfile", "dockerfilePath": "build/Dockerfile", "rootDirectory": "apps/shop",
 "port": 8080, "replicas": 3, "cpu": "250m", "memory": "512Mi",
 "env": [
   {"name": "PUBLIC_URL", "value": "https://shop.example.com", "previewValue": "https://preview.invalid"},
   {"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}},
   {"name": "DATABASE_URL", "fromClaim": {"name": "shop-db", "key": "url"}}]}
```

`env`, when present, replaces the whole list — read the project, edit, write
back. A variable is a literal `value` (with an optional `previewValue` used in
previews), a `fromSecret` reference, or a `fromClaim` reference; naming more
than one source is a `400`. `cpu` and `memory` are Kubernetes quantities and
set request and limit alike; an empty string clears one. The repository and the
two connections are deliberately not editable: rebinding a project to another
repository is a different project.

Settings land in the next release's snapshot — what is already running keeps
the configuration it was released with until the next deploy.

### Deleting a project

```sh
curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop
```

Answers `202`: the operator's finalizer deregisters the git webhook, tears
down the project's environments (production included), garbage-collects its
builds, releases, domains and claims, and removes the application namespace.
There is no undo, which is why the dashboard makes you type the project's name
first.

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

### Cancelling a build

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/builds/shop-bld-abc123def456-xk2p9/cancel
```

The BuildKit job is deleted, pod and all; the `Build` itself stays, phase
`Cancelled`, with who cancelled it in its condition — Builds are the history of
who asked for what, so cancellation never removes one. A build that already
finished answers `409`.

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

### Deleting a stuck preview

`DELETE /environments/{name}` tears a preview down — its Deployment, Service
and route go with it, and a new build for the pull request recreates it.
Previews only: the production environment is the project, torn down with it
and never on its own, so asking is a `400`. Answers `202` while the finalizer
works.

### Connections

A connection is a plugin instance — a git provider, a registry, a database
provisioner — and its credential is the reason these endpoints are shaped the
way they are: **the API never reads credentials back.** Writing one means the
operator stores it in a Secret it manages, and every response is the same
credential-free view `GET` answers.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "gh", "provider": "github", "credential": {"token": "ghp_…"}}' \
  https://kitchen.apps.example.com/api/v1/connections
```

`github`, `gitlab`, `gitea` and `neon` authenticate with `credential.token`.
A `dockerRegistry` takes `credential.username` and `credential.password`, plus
the registry in `config.url` — the prefix images are pushed under, whose host
is what builds authenticate against:

```json
{"name": "harbor", "provider": "dockerRegistry",
 "config": {"url": "harbor.example.com/kitchen"},
 "credential": {"username": "robot$kitchen", "password": "…"}}
```

`config` is the provider's own configuration and passes through as given — a
self-hosted GitHub names its API endpoint as `{"apiUrl": "https://github.internal/api/v3"}`.

A `github` token registers the repository's webhook, reads the repository, and
posts the commit status, the deployment and the pull-request comment. As a
fine-grained token that is **Contents: read-only**, **Webhooks: read and
write**, and **Commit statuses**, **Deployments** and **Pull requests: read and
write**; as a classic one the `repo` scope covers all of it, or `public_repo`
where every repository is public. A token short of the reporting permissions
still builds and deploys — the connection carries a warning saying what it
cannot post, and nothing goes red. A `neon` credential is an API key that can
create projects.

`POST /connections/test` runs that credential past the provider **without
storing anything**: no Secret is written and no connection is created, so a
token that turns out to be wrong leaves nothing to clean up. It takes the same
`provider`, `config` and `credential` a create does — or just the `name` of a
connection that exists, to re-check the credential already stored for it:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"provider": "github", "credential": {"token": "github_pat_…"}}' \
  https://kitchen.apps.example.com/api/v1/connections/test
{"reachable": true, "credentialChecked": true, "credentialValid": true,
 "message": "authenticated as octocat (token scopes: admin:repo_hook, repo)"}
```

The verdict comes in the same three parts the `Connected` and
`CredentialsValid` conditions are written from, because it is the same probe
the `ConnectionReconciler` runs: a provider that is down
(`reachable: false`), one that answered without ruling
(`credentialChecked: false` — including a provider the platform does not
implement yet), and one that refused the credential are three different
answers. `message` is the provider's own words and never contains the
credential. A malformed request — a provider nothing knows, a token provider
given a username — is a `400`.

A credential the provider accepted can still be short of a permission, which
comes back as `warnings` rather than as a failure, and rides along in the
`CredentialsValid` condition's message so an existing connection reports it
too:

```json
{"reachable": true, "credentialChecked": true, "credentialValid": true,
 "message": "authenticated as octocat (token scopes: admin:repo_hook)",
 "warnings": ["this token cannot post commit statuses on builds: add the repo:status scope"]}
```

That is read from GitHub's `X-OAuth-Scopes` header, which only a classic token
sends: a fine-grained token reports no permissions and GitHub offers no way to
ask, so it is never warned about — the form's guidance is what gets those
right.

`PATCH /connections/{name}` rotates the credential (`credential`), replaces
the config (`config`), or both; fields left out keep what is stored. The
provider is not editable — a connection to a different kind of system is a
different connection.

`DELETE /connections/{name}` refuses with `409` while any project or claim
still references the connection, naming what does. The stored credential is
deleted with it — but only when the platform wrote the Secret; a credential
something else manages (an Infisical sync, a hand-written manifest) is left
in place. Answers `204`.

### Custom domains

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"hostname": "shop.example.com", "environment": "shop-production"}' \
  https://kitchen.apps.example.com/api/v1/domains
```

A domain is a hostname in a zone *you* control — names under the platform's
base domain are refused, because they are generated and routed already — and
the environment it should reach. `tls` is optional: `acme`, `cloudflared` or
`none`, inheriting the platform's mode when absent. The `name` defaults to
the hostname with dots turned into dashes. A hostname already attached is a
`409`; an environment that does not exist a `400`.

Answers `201`, but creating the object changes no traffic by itself: the
domain has to be **verified** first, and the next move is the caller's. `GET
/domains/{name}` (and the create response, once the reconciler has run)
carries `verification` — the exact TXT record and value to create, or the
CNAME that both proves ownership and points traffic at the platform. The
`Verified` condition says which of the real failure modes applies: record
absent, record present with the wrong value, or a lookup that failed.
`CertificateReady` and `RouteProgrammed` report the rest of the journey; in
`acme` mode issuance runs over HTTP-01 through the shared Gateway, so it
finishes only once the hostname resolves to the platform.

`DELETE /domains/{name}` answers `202`: the operator's finalizer still has
the domain's certificate and secret to remove, and the Gateway drops the
hostname as the reconcilers catch up. The DNS records in your zone are yours;
the platform never touches them.

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

### What an environment has been running

`GET /environments/{name}/workload` answers the instant. `GET
/environments/{name}/metrics` answers the history, out of the telemetry store,
which is a different question and needs a different source: the API server
keeps no record of what a pod used ten minutes ago, so "was it always using
this much memory" and "did it get OOMKilled overnight" are unanswerable from
the cluster's current state.

```json
{"start": "2026-08-16T09:00:00Z", "end": "2026-08-16T10:00:00Z",
 "bucketSeconds": 60, "rollup": false,
 "cpuLimitCores": 0.5, "memoryLimitBytes": 536870912,
 "restarts": 1, "oomKills": 1,
 "points": [{"start": "2026-08-16T09:00:00Z", "cpuCores": 0.24, "cpuPeakCores": 0.41,
             "memoryBytes": 201326592, "memoryPeakBytes": 233123840,
             "replicas": 3, "restarts": 0, "oomKills": 0}]}
```

`?since=`/`?until=` bound the window (an hour ending now by default) and
`?points=` asks for a resolution, which is rounded up to a rung of a fixed
ladder so that panning does not restripe the chart. Every bucket in the window
is present, including the empty ones: a gap is a scaled-to-zero environment or
a collector that was not running, and both are worth seeing.

CPU and memory are summed across the environment's containers, which is what
"what is this environment using" means; the peaks are the sum of each
container's peak inside the bucket, a ceiling rather than a coincident total.
`replicas` is how many distinct pods reported in the bucket — the same number
an autoscaler works on, and the only way to see an environment idle to zero
and come back. `restarts` and `oomKills` are events in that bucket rather than
lifetime counters, because a counter bucketed in a store loses every
transition that lands on a boundary.

`rollup` says the five-minute rollup answered rather than the raw samples,
which is why a wide window comes back coarser than the resolution asked for.

The endpoint answers `503` where the installation has no telemetry store.
Switching `Kitchen.spec.observability.metrics` off stops the operator's half
alone: CPU, memory and the replica count keep arriving, because they are read
off the pods the node collector scraped, while restarts, OOM kills and the
limits the usage is drawn against go uncollected. What was never collected
draws nothing, deliberately, rather than a flat line at zero, which would
claim the environment used nothing.

### What the internet asked of an environment

`…/workload` and `…/metrics` answer what the platform ran. These four answer
what was asked of it, and they cost the application nothing: every request to
every Kitchen application crosses the shared Gateway's proxy, so an application
nobody instrumented still has traffic, error and latency numbers. They are the
four golden signals, minus saturation, which `…/metrics` already answers.

All four take the same scope:

| Parameter | Meaning |
|---|---|
| `since` / `until` | RFC 3339 bounds on the window. An hour ending now by default |
| `route` | One route template, spelled as the route table spells it — what clicking a row filters the rest by |

`GET /environments/{name}/requests/summary` is the header:

```json
{"environment": "shop-production", "edge": {"routed": true},
 "since": "2026-08-16T09:00:00Z", "until": "2026-08-16T10:00:00Z", "rollup": "1m",
 "requests": 3600, "requestsPerSecond": 1, "errors": 36, "errorRate": 0.01,
 "p50Ms": 12, "p95Ms": 240, "p99Ms": 900}
```

`since` and `until` are the window that was *answered*, not the one that was
asked for: these numbers are read off pre-aggregated buckets, a bucket is
indivisible, and a window that starts inside one takes the whole bucket. The
start therefore comes back snapped to `rollup`'s resolution — `1m` or `1h` —
and `requestsPerSecond` is per the window reported here. `errors` is answers of
500 and above; a 4xx is the caller's fault and belongs in the route table's
breakdown rather than in the number that says the service is broken.

`GET /environments/{name}/requests/series` is the same signals over time, for
the charts. `?buckets=` asks for a resolution (60 by default, capped at 480),
rounded up to a rung of a fixed ladder so that panning does not restripe the
chart:

```json
{"environment": "shop-production", "edge": {"routed": true},
 "start": "2026-08-16T09:00:00Z", "end": "2026-08-16T10:00:00Z",
 "bucketSeconds": 60, "rollup": "1m",
 "points": [{"start": "2026-08-16T09:00:00Z", "requests": 60, "requestsPerSecond": 1,
             "errors": 1, "errorRate": 0.0166, "p50Ms": 11, "p95Ms": 230, "p99Ms": 880}]}
```

Every bucket in the window is present, including the empty ones: a gap is an
environment that served nothing, which on a traffic chart is the most
interesting shape there is.

`GET /environments/{name}/requests/routes` breaks the window down per route
template — the per-path view, which works because the set of templates is
bounded at ingest:

```json
{"environment": "shop-production", "edge": {"routed": true},
 "items": [{"route": "/checkout/:id", "requests": 400, "requestsPerSecond": 0.11,
            "errors": 4, "errorRate": 0.01, "p50Ms": 30, "p95Ms": 310, "p99Ms": 900}]}
```

`?sort=` is one of `requests` (the default), `errors`, `errorRate` or `p95`,
and `?limit=` how many rows come back (100 by default, capped at 500). The sort
is a query rather than a presentation detail, because it decides which rows
survive the limit: the ten busiest routes and the ten slowest are not the same
ten. A sort nobody offers is a `400` naming the ones that exist.

A path is templated where it is collected, not here: `/users/12345` is
`/users/:id`, a UUID is `:uuid`, a content-hashed asset is `*.js`. Each
environment gets 300 templates, and everything past that is recorded as the
overflow route `/…` — a row that says the classifier missed an identifier
scheme, rather than a rollup quietly growing a series per user id.

`GET /environments/{name}/requests` is the requests themselves, newest first:

```json
{"environment": "shop-production", "edge": {"routed": true},
 "items": [{"timestamp": "2026-08-16T09:59:58.412Z", "host": "shop.apps.example.com",
            "method": "POST", "path": "/checkout/9182", "route": "/checkout/:id",
            "status": 503, "durationMs": 12.5, "protocol": "HTTP/1.1", "source": "gateway"}]}
```

| Parameter | Meaning |
|---|---|
| `method` | One verb. Case-insensitive; the follower stores them canonicalised |
| `status` | A *class* of answer — `5xx`, or plainly `5`. One exact code is not offered |
| `errors` | `1` keeps what the signals count as an error (500 and above). Composes with `status` |
| `limit` | Rows to return, default 200, capped at 5000. The newest are kept |

`path` is the raw path with its query string already gone, and `route` is what
it was templated to; both are kept, because the template is what groups and the
raw path is what makes a mis-templated route diagnosable. The list streams when
asked to, exactly as the log endpoints do — `Accept: text/event-stream` answers
the current page and then every request that arrives after it, one `data:`
event per row. A plain GET on the same URL still answers the bounded page.

Raw rows are kept for at most seven days, while the aggregates the other three
endpoints read are kept for the platform's whole retention and the hourly ones
for far longer — so a listing reaches back less far than a summary of the same
window does, and a wide window is cheap for the three and expensive for this
one.

#### What a request row cannot tell you

The vantage point is the platform's ingress, which sees everything that enters
and nothing that stays inside. Four consequences, none of which these endpoints
paper over:

- **No build and no release.** The edge routes to a Service, not to a pod, and
  during a rollout both revisions answer under one route. Rows carry project
  and environment, never a build or a release — correlate with the activity
  feed's deploy entries by time instead.
- **No query strings.** They are stripped before the row is written and never
  stored: privacy and path cardinality settled in one move.
- **gRPC errors are not counted.** A failed gRPC call is an HTTP 200 with a
  `grpc-status` trailer the edge does not read, so `errors` and `errorRate` are
  transport-level for an HTTP/2 service. The route table's `protocol` is where
  that shows.
- **Nothing east-west.** Service-to-service calls inside the cluster never
  cross the Gateway; `/traffic` sees them as L4 edges, and no request row
  exists for them.

#### Environments the golden signals do not fit

A worker, a cron job, an environment whose route is withheld: not everything
the platform runs is on the edge, and four charts of zeroes would describe the
platform rather than the application. Every one of the four answers carries
`edge`, which is what tells that case from a quiet hour:

```json
{"edge": {"routed": false, "message": "no HTTP traffic reaches this environment through the platform's edge: …"}}
```

`routed: false` says nothing publishes this environment on the shared Gateway,
so there is nothing there to observe — the screen says so and leads with what
is real for such a workload: its logs, its resource usage against the release's
limits, and its restarts. `routed: true` with `requests: 0` is the other
answer: it is on the edge, and nothing was asked of it in this window.
`routed` is false only where the platform is *sure*; a route that could not be
read (Gateway API CRDs a version behind, a ClusterRole that may not read them)
leaves it true with a `message` saying the check did not happen, because
declaring an application off the edge on the strength of a failed read is the
loud way to be wrong.

### The crash report

`GET /environments/{name}/diagnostics` answers everything the platform knows
about a container that died, assembled — which is the whole point of it. The
parts exist separately already: the termination is on `…/workload`, the lines
are in the log store, the memory series on `…/metrics`, the cluster's warnings
and the edge's requests in their own tables. What nobody has is the join, and
finding those five things in five places, each with its own window, is the work
this endpoint deletes.

```json
{"environment": "shop-production", "namespace": "kitchen-shop",
 "crashed": true, "restarts": 12,
 "report": {
   "crash": {"pod": "shop-production-7d9f4", "container": "app",
             "reason": "OOMKilled", "oomKilled": true, "exitCode": 137,
             "startedAt": "2026-08-16T09:57:11Z", "finishedAt": "2026-08-16T09:58:02Z",
             "restarts": 12, "previous": true,
             "waiting": "CrashLoopBackOff: back-off 5m0s restarting failed container"},
   "since": "2026-08-16T09:28:02Z", "until": "2026-08-16T10:03:02Z",
   "logs": [{"timestamp": "2026-08-16T09:58:01.902Z", "message": "heap limit reached", "…": "…"}],
   "resources": {"memoryLimitBytes": 536870912, "oomKills": 1, "points": ["…"]},
   "events": [{"timestamp": "2026-08-16T09:58:02Z", "reason": "OOMKilling",
               "message": "Memory cgroup out of memory", "…": "…"}],
   "requests": [{"timestamp": "2026-08-16T09:58:02.113Z", "method": "POST",
                 "path": "/import", "status": 502, "durationMs": 30012, "…": "…"}]}}
```

`oomKilled` has its own field beside `reason` because "the kernel killed it for
using too much memory" and "it crashed" are different problems with different
fixes and the same exit code. `previous` says the termination ended the run
*before* the current one, which is the ordinary shape of a crash loop — by the
time anyone looks, the container is either serving again or waiting out its
backoff, and `waiting` is that backoff. Init containers are read too: a
workload that never starts because its init container dies is invisible in the
app container's status.

`since` and `until` bound the assembly, and the sections do not all use the
whole span. The lines and the resource series stop at the termination instant,
because they are what *led up to it* — the lines are the dead container's own,
not the environment's, so that two healthy replicas do not bury the fifty that
matter. The events run past it, because a crash loop keeps announcing itself
and the `BackOff` is the cluster naming the loop. The requests are the ±30
seconds around it, the same width the correlated-logs view uses: a 502 there is
the edge noticing the pod go, and a slow 200 just before it is the load that
preceded it. `resources` carries the memory series read against the limit the
release set, and per bucket the restart trajectory — where in the window the
restarts happened, rather than how many there have ever been.

`?logs=` and `?requests=` size the two listings (50 each by default). This is
one assembled screen rather than a search; `/logs` and `…/requests` are where
someone goes when fifty is not enough.

Nothing having crashed is an answer, not an empty report:

```json
{"environment": "shop-production", "namespace": "kitchen-shop", "crashed": false,
 "restarts": 0, "message": "no container in this environment has terminated abnormally, …"}
```

An environment with no pods at all says that instead, and points at the
`Environment`'s own conditions, which are where "nothing was ever materialized"
is explained. A container that exited zero is not a crash — a completed job's
pod, a sidecar told to stop — and calling it one would make the report cry wolf
on every rollout.

The report is all-or-nothing: one half failing fails the request and names the
read that failed, because a section that silently came back empty would be read
as "nothing was logged" or "no warning was raised". `503` where the
installation has no telemetry store, like every other endpoint that reads it.

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
 "components": [{"name": "collector", "kind": "DaemonSet", "healthy": false,
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

Build and runtime logs come from ClickHouse, where the node collector ships
every container line it tails — so a build's output survives the build pod,
and a preview's logs outlive the preview.

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
`{timestamp, source, project, environment, build, pod, container, stream, level, message, fields}`.
`level` is the collector's best-effort read of the line's severity, folded to
lower case (`trace`/`debug`/`info`/`warn`/`error`/`fatal`) so that `error` is
one value however the line spelled it, and empty when the line said nothing.
`fields` is what the line itself said, when it was JSON: the object is
flattened with dots (`{"http": {"status": 500}}` is `http.status`), every value
is stringified, and the field is left out entirely for a line that was not
JSON.

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

All four are containers. The node's own system logs are deliberately not
collected — see [SCOPE.md](SCOPE.md) for why the collector cannot read a
journal it has no `journalctl` for, and why Talos has none to read.

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
selection applied to every new line. A plain GET on the same URL
still answers the bounded page, so nothing changes for callers that do not
ask. The UI tails builds and the observability view this way and falls back
to polling when the stream drops.

### Querying logs

`/logs` and its three companions all take the same *selection*: what to match,
and over what window. The four are views of one question — the lines, when they
happened, what else is in them, and what they are actually saying — so they take
the same parameters and are meant to be asked together.

| Parameter | Meaning |
|---|---|
| `q` | Kitchen's log query language. The front door |
| `where` | A ClickHouse boolean expression, evaluated as written. The escape hatch |
| `since` / `until` | RFC 3339 bounds on the window |

Both query parameters are optional and compose with `AND`. **Asking for nothing
selects everything in the window** — the window and the limit are the bounds,
and there is no sentinel expression to type. (`where=1 = 1` used to be that
sentinel, and it is gone.)

```
GET /logs?q=level:error service:shop&since=2026-08-13T10:00:00Z
GET /logs/histogram?q=level:error service:shop&since=2026-08-13T10:00:00Z&buckets=60
GET /logs/facets?q=level:error&fields=level,service,container
GET /logs/patterns?q=service:shop&limit=20
```

#### The query language

A term is `field:value`, a bare word searches the message, and terms next to
each other are `AND`ed:

| Written | Means |
|---|---|
| `timeout` | The message contains `timeout`, case-insensitively |
| `"connection refused"` | The same, as a phrase |
| `level:error` | The `level` column is exactly `error` |
| `level:error,fatal` | Either of them |
| `pod:shop-*` | `*` and `?` are wildcards |
| `message:/GET \/works\?page=/` | A ClickHouse regular expression |
| `http.status:>=500` | Numeric comparison — `>`, `>=`, `<`, `<=` |
| `trace_id:*` | The field is present and non-empty |
| `-source:cluster` | Negation. `NOT` and `!` are the same |
| `a OR b`, `(a b) OR c` | Alternation and grouping |

Columns are `source`, `project`, `environment`, `build`, `namespace`, `pod`,
`container`, `node`, `stream`, `level`, `message`, `traceId` and `spanId`, plus
the aliases `service`/`app` for `project`, `env` for `environment`, `msg` for
`message` and the usual spellings of the two id columns (`trace_id`,
`trace.id`, `span_id`). `timestamp` is deliberately not addressable: the window
is `since`/`until`, and a query that could move it would let the lines and the
histogram disagree about what they are showing.

Those are the query language's names, not the table's. The store is written by
a stock OTel exporter whose column names are not Kitchen's to rename, so
`level` and `message` read `SeverityText` and `Body` underneath; the
translation lives in the operator rather than in `ALIAS` columns, which keeps
the table the standard shape any OTel-aware tool expects. It only shows through
in `where` below.

Anything that is not a column is a **structured field** of the line, so
`http.status:500` reads `LogAttributes['http.status']`. `labels.tier:web`
reaches the pod's Kubernetes labels instead. This is the one place a typo goes
quiet: `levl:error` asks for a field nothing writes and matches nothing rather
than being refused.

Every value travels to ClickHouse as a bound parameter, never as query text.

#### The ClickHouse escape hatch

`where` is a real ClickHouse expression over the table's columns, evaluated as
written — the query language is a front door, not a cage:

```
GET /logs?where=match(Body, 'GET /works\?page=\d+') AND environment = 'shop-production'
```

Its vocabulary is `otel_logs`'s own, which is the price of a store any
OTel-shaped tool can read: `Body`, `SeverityText`, `LogAttributes['…']` where
the query language says `message`, `level` and a field name. Kitchen's own
columns — `project`, `environment`, `build`, `source`, `namespace`, `pod`,
`container`, `node` — are real columns here and mean what they mean everywhere
else, because they are what the table is ordered by.

It reaches ClickHouse as query text, which is the point — and why it runs pinned
read-only (`readonly=2`: no writes, no DDL) under an execution cap, as the
operator's own database user. What that user can read is the telemetry database;
per-caller scoping arrives with scopes and RBAC ([open item](#open)).

A query either side refuses — a bracket that never closes, an unknown column —
answers `400` carrying the diagnostic that says how to fix it: Kitchen's parser
for `q`, ClickHouse's own for `where`.

#### The histogram

`GET /logs/histogram` counts the selection into buckets:

```json
{"start": "...", "end": "...", "bucketSeconds": 60, "total": 14021,
 "buckets": [{"start": "...", "count": 210, "errors": 4, "warnings": 12}]}
```

`?buckets=` is how many bars are wanted (default 60, capped at 480); the width
is rounded up to a rung of a fixed ladder — 1s, 2s, 5s … 1h, 6h, 1d, 1w — so
that panning the window does not restripe the chart. Every bucket in the window
is present, including the empty ones, because a gap is information. A selection
with no `since` is bucketed over what the matching lines actually span, read
from the store, rather than over an assumed day.

#### Facets

`GET /logs/facets` counts each field's distinct values **over the whole
window**, not over the page of lines `/logs` returned — which is the point of
asking the store rather than counting in the browser:

```json
{"items": [{"field": "level", "distinct": 4,
            "values": [{"value": "info", "count": 8021}, {"value": "error", "count": 42}]}]}
```

`?fields=` names them, defaulting to `level`, `source`, `project`,
`environment`, `container`, `stream`. A field that is not a column is resolved
the way the query language resolves it, so `fields=http.status` facets over a
structured field. `?limit=` bounds the values per facet (capped at 20). All of
them are one query, so a sidebar costs one round trip however many it shows.

#### Patterns

`GET /logs/patterns` collapses the messages into templates: the variable parts
— identifiers, addresses, timestamps, numbers — are replaced with placeholders
and what is left is grouped and counted, so a spike of 14,021 lines reads as the
handful of shapes it is.

```json
{"items": [{"pattern": "GET /works?page=<n> <n>", "count": 14021, "level": "info",
            "sample": "GET /works?page=7 200", "firstSeen": "...", "lastSeen": "..."}]}
```

Normalising is a regular expression per line rather than a columnar scan, so it
runs over the newest `?scan=` matching lines (default 20,000, capped at
200,000) rather than the whole window — the shape of the newest lines is the
shape. `?limit=` is how many templates come back (default 20, capped at 200).

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
`preview.created`, `preview.removed`, `project.created`, `project.deleted`,
`claim.created`, `claim.deleted`, `claim.bound`, `claim.failed`.

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
- `projects`: per-project 24h traffic (requests, 5xx, p95, hourly series),
  from the request pipeline

The `projects` rows and the platform totals above them therefore come from two
different sources, and where they disagree the rows are the ones to trust. The
totals are aggregated from flows, which are attributed by the *destination*
endpoint: a protected preview's traffic is credited to the forward-auth gate
and an idling environment's to the KEDA interceptor, both of which live in the
platform's own namespace — so both used to vanish from the project that served
them. A request row is attributed by the `Host` header instead, which is the
one thing every hop preserves and the only thing the interceptor routes on.
Every project gets a row, at zero if nobody visited it: this is a list of
projects with numbers on it, not a list of numbers.

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

### Traces

`GET /traces` lists the traces in a window, newest first; `GET
/traces/{traceId}` reads one trace's spans, oldest first, which is the order a
waterfall is drawn in.

```json
{"items": [{"traceId": "9d8d0f…", "timestamp": "2026-08-16T10:00:00Z",
            "name": "GET /checkout", "service": "shop", "environment": "shop-production",
            "durationMs": 420.5, "spans": 7, "errors": 1, "services": 3, "httpStatus": 500}]}
```

`?errors=1` and `?minDuration=` are the two filters anyone opens a trace list
for. Both are over the *trace*, not over its spans: one slow span makes a slow
trace, and filtering the rows would drop the healthy half of a failed one.
`?project=`, `?environment=` and `?service=` narrow the emitter, and
`?since=`/`?until=` the window (the last hour by default).

Reading one trace takes no window on purpose. A trace id arrives from a log
line or from the list, and requiring the caller to also know when it happened
would break the one link that makes traces worth collecting. A trace nothing
was kept for answers `404` and says retention is the likely reason, rather
than an empty list that reads as "this request did nothing".

Spans come from the applications themselves, over the node collector's
OTLP/HTTP endpoint — see [SCOPE.md](SCOPE.md) for why nothing here is derived
from the flow data. Every environment the operator deploys is handed that
address through OTLP's own environment variables, so instrumenting an
application is adding its language's SDK and nothing else. Nothing about that
changed when the receiver moved out of the operator and into the collector:
same Service name, same port. The Service is `internalTrafficPolicy: Local`
now, so one stable name means the agent on the caller's own node — and on a
node where no agent is Ready, an export is dropped rather than sent to
another.

### Saved queries

`GET /logs/saved` lists the selections someone kept under a name; `POST`
saves the current one and `DELETE /logs/saved/{name}` forgets it. A saved
query is the observability view's own URL state — `query`, `where`,
`rangeMinutes`, `limit`, `view`, `includeCluster` — with a `title` on it.

```json
{"name": "checkout-500s", "title": "Checkout 500s",
 "query": "level:error service:shop", "rangeMinutes": 60, "limit": 500,
 "view": "patterns", "savedBy": "grace@example.com", "createdAt": "2026-08-16T10:00:00Z"}
```

The object name is derived from the title, so nothing has to be invented; a
second query that derives the same name answers `409` in the platform's words
rather than the API server's. The window is stored as a duration and never as
an absolute range: a saved "the spike last Tuesday" is a screenshot rather
than a question, and the retention deletes it out from under its own name. The
query is compiled before it is stored, because a saved query that cannot be
run is found later, by someone who did not write it, at the moment they needed
an answer.

## Status codes

| Code | When |
|---|---|
| `200` / `201` | Fine |
| `202` | Accepted — the operator's finalizers finish the work after the response |
| `204` | Deleted, nothing left to describe |
| `400` | The request cannot be carried out as written |
| `401` | No valid token — including when the platform has no identity provider |
| `403` | The operator's own service account may not do this |
| `404` | No such object, or no such endpoint |
| `409` | Someone else changed the object first, it already exists, it already finished, or something still uses it |
| `503` | A capability this endpoint needs is not installed |

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Token validation | Stateless, against the issuer's JWKS | No session state in the operator; the identity provider stays swappable |
| Token audience | The API's own URL (`resource=`), or the issuer | A resource server should be able to tell a token meant for it from a token meant for everything |
| CI tokens | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's own credential; keeping key lookup at the issuer keeps the operator's request path stateless |
| Response shapes | The API's own vocabulary, not raw custom resources | A stable contract for the UI, and freedom to change how state is stored |
| Write surface | The full project, connection and claim lifecycle, rebuild and cancel, promote/rollback, preview teardown, and the settings' runtime defaults | Nothing a user does in the platform's normal running should need `kubectl`; domain writes wait for their reconciler, because a write over objects nothing reconciles only looks like it works |
| Credentials | Write-only: the operator stores them in Secrets and never echoes them | "Credentials never leave the operator" survives the API growing a write surface |
| Introspection shapes | Kubernetes' own vocabulary — replicas, restarts, manifests | The exception that proves the rule above: these endpoints exist to explain the platform's machinery, and a reader comparing them against `kubectl` should not have to translate |
| Empty request surfaces | `edge.routed` beside the numbers, on every request answer | "Nothing reaches this environment through the edge" and "nothing was asked of it in this window" are both zeroes and different sentences; four empty charts would describe the platform rather than the application |
| The crash report | One endpoint that joins five sources, all-or-nothing | Troubleshooting should be reading rather than hunting, and a section that failed silently would be read as evidence that nothing happened |
| Pods and nodes | Read uncached, straight from the API server | Serving them from the manager's cache would mean an informer over every pod in the cluster, kept warm for a question only an open dashboard asks |
| The dashboard | Served by the same process, outside `/api/` | The SPA is public, stateless files plus `/config.json` (issuer, client id, audience — the same values every login redirect shows); everything with state stays behind the token check |
| OTLP ingest | The node collector's own unauthenticated in-cluster port, never on the Gateway | Spans come from workloads already inside the cluster; an OTLP endpoint on the public Gateway would be an unauthenticated write surface on the telemetry store |
| Saved queries | A `SavedQuery` object with no reconciler | The rule that a write waits for its reconciler is about objects that do nothing until something acts on them; a saved query has its whole effect by existing |
| Webhook receiver | Stays signature-authenticated, not OIDC | A provider proving a payload is genuine is a different question from a caller proving who they are |

## Open

- **Scopes and RBAC.** Tokens carry their scopes and the API records who asked
  for what, but nothing is enforced beyond "the issuer vouches for you". Teams
  and per-organisation roles land with the organizations plugin, and the token
  shape follows them.
- **Paging.** Collections answer in full. `{"items": …}` is an object rather
  than a bare array so a cursor can be added without breaking clients.
