# Kitchen — Environments and releases

An environment is a release, running. Moving one to another release is the
whole of promotion and rollback; everything else here is a way of asking what
that environment is doing and why.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Rolling back

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

## The bar an environment sets

An environment may declare *requirements*: a policy bundle, pinned by digest,
that an artifact's attested evidence will be judged against before a release
lands here. Who may change that bar is written on the environment itself —
`owners`, a list of accounts in the same vocabulary as every access entry (an
issuer `sub`, or an email address, honoured only for a verified one) — and it
is deliberately not a project role. The team that deploys into an environment
and the people who decide what it demands are two lists, and neither grants
the other: that separation *is* segregation of duties, expressed as a schema
instead of a policy document.

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"bundleDigest": "sha256:4f6c…", "parameters": {"maxSeverity": "high"},
       "owners": ["risk-officer@example.com"]}' \
  https://kitchen.apps.example.com/api/v1/environments/shop-production/requirements
```

Every field is optional and absent means untouched, so one call can change the
bundle, the parameters or the owners without restating the rest. `parameters`
and `owners`, when present, each replace their whole list. An empty
`bundleDigest` removes the requirements — the environment goes back to
declaring no bar. The digest must be `sha256:` plus 64 hex characters; the CRD
refuses anything else at admission, because a bundle named loosely is a
decision that cannot be replayed.

Only the environment's owners or a platform operator may call this; anyone
else with a role on the project is answered `403` with who may. **An
environment naming no owners is locked, not open**: only operators may set its
requirements, which makes "nobody has owned this yet" the safe state rather
than the writable one.

Every change is recorded in the audit log before it is made, marked
privileged, with the previous bundle digest and the *names* of the parameters
that moved — so the log alone says what the bar was at any moment, and a
change can be walked back on paper.

## Whether a release clears it

`GET /environments/{name}/eligibility` answers how a release measures up
against the environment's requirements — by default the release the
environment currently runs, or any of the project's releases with
`?release=<name>`. The answer is a pure function of stored evidence: the
attestations attached to the release's artifact, read as they were recorded,
never a live check. Nothing is stored and nothing is decided — the promotion
pipeline is what will act on the comparison.

```json
{"environment": "shop-production", "project": "shop", "release": "shop-rel-42",
 "requirements": {"bundleDigest": "sha256:4f6c…", "parameters": {"maxSeverity": "high"}},
 "evidence": [
   {"predicateType": "https://slsa.dev/provenance/v1", "source": "builder", "verified": true},
   {"predicateType": "https://spdx.dev/Document", "source": "builder", "verified": true}],
 "eligible": true, "evaluated": true, "unmetRules": [],
 "message": "the release clears bundle sha256:4f6c…"}
```

The evaluation is the policy engine's — the same code path a promotion
decision comes from (see [decisions](decisions.md)): the environment's pinned
bundle over the materialized evidence, with `unmetRules` naming the specific
rules that fired as stable rule ids. A release is never refused with a
generic failure.

`eligible` is three-valued on purpose. An environment that declares no
requirements answers `true` — a bar of height zero, and the message says so.
An environment whose pinned `bundleDigest` cannot be resolved — the ConfigMap
is gone, the digest was mistyped — answers `null` with `evaluated: false`:
"not judged" and "passed" are different claims, and this endpoint will not
blur them.

The `evidence` list is the screen's half meanwhile: what the artifact carries
(by predicate type), whose claim each piece is (`platform`, `builder`, or
nothing for evidence something else attached), and whether it verified against
the platform's signing key. A registry that cannot be asked degrades to the
build's own evidence index, listed unverified, with the message saying so.

## Deleting a stuck preview

`DELETE /environments/{name}` tears a preview down — its Deployment, Service
and route go with it, and a new build for the pull request recreates it.
Previews only: the production environment is the project, torn down with it
and never on its own, so asking is a `400`. Answers `202` while the finalizer
works.

## What an environment is running

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

## What an environment has been running

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

## What the internet asked of an environment

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

### What a request row cannot tell you

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
  transport-level for a gRPC service — a screen showing them for one has to say
  so. The `protocol` on a request row (`HTTP/2`) is the only place the platform
  can tell you it is looking at one; the aggregates carry no protocol at all.
- **Nothing east-west.** Service-to-service calls inside the cluster never
  cross the Gateway; `/traffic` sees them as L4 edges, and no request row
  exists for them.

### Environments the golden signals do not fit

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

## The crash report

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
   "resources": {"memoryLimitBytes": 536870912, "oomKills": 1, "points": [{"…": "…"}]},
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
as "nothing was logged" or "no warning was raised". Assembling it needs the
telemetry store, so an installation without one answers `503` — but only when
there is something to assemble; whether anything crashed is read off the API
server, and that answer costs the store nothing.

## What is wrong with an environment

`GET /environments/{name}/signals` is the diagnostics strip at the top of the
environment page: *"2 problems: crash-looping (12 restarts in 30m), memory at
96% of limit"*, each linking to the screen that shows the numbers behind it.

```json
{"project": "shop", "environment": "shop-production",
 "evaluatedAt": "2026-08-16T10:00:00Z",
 "counts": {"critical": 1, "warning": 1, "info": 0},
 "items": [
   {"signal": "workload.crashloop", "severity": "critical",
    "scope": {"kind": "environment", "project": "shop", "environment": "shop-production", "name": "app"},
    "fingerprint": "workload.crashloop/shop/shop-production/app",
    "title": "crash-looping", "detail": "12 restarts in 30m; CrashLoopBackOff: back-off 5m0s …",
    "since": "2026-08-16T09:31:00Z",
    "evidence": "/environments/shop-production?section=workload"}],
 "unreadable": [{"input": "http_requests_1m", "reason": "the request series query failed: …"}]}
```

The rules are a versioned catalogue in the operator, evaluated when a screen
asks rather than on a timer: nothing is stored, and `evaluatedAt` is how fresh
the answer is. `fingerprint` is stable for the same underlying condition across
evaluations, which is what will let a later release diff rounds and record
transitions instead of re-announcing the same problem every interval — the
shape is designed for that and does not change when it arrives. `detail`'s
first clause is the headline number, so a strip can render `title (first
clause)` without knowing anything about the rule that produced it.

Findings here are the environment's own and its project's; a saturated node or
an unprogrammed Gateway belongs to the platform and is on the operator's list
instead. `unreadable` is the one field worth reading when `items` is empty: it
names each input the evaluation could not read, once, with the reason. An empty
`items` with an empty `unreadable` means nothing is wrong; an empty `items`
beside an unreadable `http_requests_1m` means nobody looked. The API never
conflates them, and neither should a screen.

An installation with no telemetry store is a third answer again: the rules that
read it do not arise, so they are silently absent from both lists rather than
reported as broken forever.

## The objects the operator materialized

`GET /environments/{name}/objects` answers with the Kubernetes objects behind
an environment — the `Deployment`, the `Service` and the `HTTPRoute` — as the
API server holds them. It is the dashboard's operator mode surfacing what the
reconciler did, so the objects are deliberately *not* translated into the API's
own vocabulary: whoever opens this wants the manifest, and a summary would send
them to a terminal anyway.

**It is `operator`-only, and a developer needing it is a bug.** The premise of
the platform is that a developer never needs a Deployment; and the manifest is
the materialized one, so a project's literal environment variables are in it.
If somebody has to open this to answer a question, the missing thing is a
product surface — file it, rather than widening the role.

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
