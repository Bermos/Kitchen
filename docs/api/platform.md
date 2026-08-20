# Kitchen — Platform status and the operator's screens

What the installation itself is doing. These are the operator's routes: the
component survey, the problems list, and the screens the dashboard builds from
them.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Platform status

`GET /status` is the platform as it is *running*, where `/settings` is the
platform as it is *configured*. It is one request because it answers one
question — the dashboard's status bar:

```json
{"cluster": {"name": "chef", "nodes": 8, "readyNodes": 8},
 "tunnel": {"enabled": true, "connected": true, "message": "cloudflared is available"},
 "builds": {"running": 1, "capacity": 2, "queued": 1, "oldestWaitSeconds": 1920,
   "waiting": [{"name": "shop-bld-abc123", "project": "shop",
                "queuedAt": "2026-08-17T03:14:00Z", "waitSeconds": 1920}]},
 "gateway": {"address": "203.0.113.7", "programmed": true},
 "components": [{"name": "collector", "kind": "DaemonSet", "healthy": false,
   "available": 0, "desired": 3, "message": "0 of 3 pods available: …"}]}
```

**It is the one payload that varies by role**, and that is deliberate: it is
the home page for both of the platform's people, so a second endpoint would
have doubled the surface for one body. `cluster.name` and `builds` are
everybody's — "why is my build waiting" is a developer's question. `tunnel`,
`gateway`, `components` and the node counts are the operator's, and a member
gets this instead:

```json
{"cluster": {"name": "chef"},
 "builds": {"running": 1, "capacity": 2, "queued": 0}}
```

**Withheld means absent, never zeroed.** `tunnel === undefined` is "you are not
allowed to know" and `{"enabled": false}` is "no tunnel is configured"; an
empty `components` would read as a healthy platform running nothing, so it is
not sent at all.

`cluster.name` is `spec.clusterName` on the `Kitchen` singleton, falling back
to the first label of the base domain — Kitchen owns the cluster it is
installed into, so naming it names the installation. `builds` is what the build
controller's concurrency gate is weighing: builds running against
`spec.builds.concurrency`, how many are waiting for a slot, and how long each
has waited — longest first, with `oldestWaitSeconds` repeating the head of the
list. The wait is the half worth reading: a queue's length says the platform is
busy, and only the wait says whether it is moving. Both are omitted when
nothing is queued.

**The queue's counts are everybody's; its names are the caller's own.**
`running`, `capacity`, `queued` and `oldestWaitSeconds` are the whole gate's,
because that is what answers "why is my build waiting" — the queue is busy, or
it has stopped moving. `waiting` is narrowed to the projects the caller can
see, like every other list this API answers across projects: an operator gets
the whole queue, and a member gets their own builds and a count for everyone
else's. Naming the rest would enumerate every project on the platform and its
build object names to any account with a token, thirty seconds at a time — and
each of those names then answers `404` from `GET /builds/{name}`, which is the
rule that a caller is not told an object they hold no role on exists.

`components` is
the operator's own survey of every workload labelled
`app.kubernetes.io/part-of: kitchen`, which is the only place a workload whose
pods were refused at admission shows up at all — it has no pods to look at.

A node count the operator's ClusterRole does not allow comes back as zero with
the reason in `cluster.message`, rather than failing the request: an
installation upgraded from before this endpoint should not lose its whole
status bar over the one line it cannot fill in.

## The operator's screens

`/status` answers the status bar. `/platform/*` answers the section behind it:
the platform seen across every project, which is a different question and —
one day — a differently authorized one.

**Everything platform-scoped lives under this prefix, and nothing
project-scoped does.** That is what makes the whole prefix one row of the
enforcement table: every `/platform/*` route requires `operator`, and a
platform-wide question never appears as a query parameter on a project-scoped
endpoint, however convenient that would be.

None of these adds a watch. They read the cluster through the same uncached
reader the introspection endpoints use and the store through the same client
the logs do, so a screen nobody has open costs the platform nothing.

Three of them can be answered only in part, and each says so in a field rather
than by drawing a zero:

| Field | Present when |
|---|---|
| `telemetryMessage` | the store could not be read, so freshness is unknown rather than fine |
| `usageMessage` | the saturation or volume-fill series could not be read — an installation with no telemetry store, or a query that failed |
| `eventsMessage` | the cluster's warnings could not be read, so a workload's refusal is missing its explanation |

### The problems list

`GET /platform/signals` is every finding currently firing anywhere on the
platform, worst first. It answers in exactly the shape
`/environments/{name}/signals` does — same catalogue, same fingerprints, same
`unreadable` list — narrowed to nothing instead of to one environment:

```json
{"evaluatedAt": "2026-08-16T10:00:00Z",
 "counts": {"critical": 2, "warning": 3, "info": 0},
 "items": [{"signal": "node.silent", "severity": "critical",
            "scope": {"kind": "node", "node": "node-b"},
            "fingerprint": "node.silent/node-b",
            "title": "no telemetry", "detail": "nothing received for 34m …",
            "since": "2026-08-16T09:26:00Z", "evidence": "/platform/nodes?node=node-b"}],
 "unreadable": []}
```

Rules that could not be evaluated are *not* in `items`: they are in
`unreadable`, named once each, because a store outage that darkened thirty
rules should be one sentence at the top of the screen and not thirty rows in
it. This screen is the alert inbox minus persistence — when background
evaluation lands it reads recorded transitions instead of evaluating on view,
and answers in this same shape.

### Nodes

`GET /platform/nodes` is what the cluster is made of, plus the column that is
the reason this screen exists:

```json
{"nodes": 3, "readyNodes": 3, "silentNodes": 1,
 "items": [{"name": "node-b", "ready": true, "schedulable": true,
            "roles": ["worker"], "kubeletVersion": "v1.34.1", "pods": 17,
            "allocatable": {"cpu": "8", "memory": "32Gi", "pods": "110"},
            "conditions": [{"type": "MemoryPressure", "status": "True", "reason": "…", "since": "…"}],
            "telemetry": {"silent": true}}],
 "usageMessage": "this installation has no telemetry store, so node saturation is absent rather than zero …"}
```

`telemetry` is when the store last received anything from this node's
collector. A node whose collector is dead — or was never admitted, which is the
Pod Security failure the platform namespace's own level exists to prevent —
reads healthy everywhere else: its conditions are True, its pods are Running,
and it simply stops contributing to every number the platform reports. Silence
is reported as an *absence* of `lastSeen` rather than as an old timestamp,
because that is the shape of the query behind it: it looks back an hour, and a
node that said nothing in that hour is not in the answer at all.

A freshness read that failed leaves every node neither fresh nor silent, with
`telemetryMessage` saying why. That distinction is load-bearing: a store nobody
could reach must not make the whole cluster look silent, which is the same
wrong answer this screen exists to prevent, arrived at from the other side.

`?node=` narrows to one, which is where the findings' evidence links point.
`usage` carries the node's CPU, memory and filesystem series, read out of
`host_metrics` over the same window and bucket width the `node.saturated` and
`node.disk-filling` rules fire on, so the screen and the problems list cannot
disagree about a number. An installation with no telemetry store has no series
to read, and a query that failed has none either: `usage` is then absent, with
`usageMessage` saying which — an unmeasured node and an idle one must not draw
the same chart.

### Workloads

`GET /platform/workloads` is every workload and every pod on the platform,
applications and platform components alike — and, more to the point, the
workloads that have *no pods at all*:

```json
{"workloads": 24, "unhealthy": 2, "withoutPods": 1,
 "items": [{"kind": "DaemonSet", "namespace": "kitchen-system", "name": "kitchen-collector",
            "component": "collector", "desired": 3, "ready": 0, "available": 0, "pods": 0,
            "healthy": false,
            "admission": {"reason": "FailedCreate", "count": 12, "at": "2026-08-16T09:00:00Z",
                          "message": "pods \"kitchen-collector-\" is forbidden: violates PodSecurity …",
                          "suspect": "Pod Security refused the pod: …"}}],
 "pods": [{"namespace": "kitchen-shop", "name": "shop-production-5c9f7d6b4-abcde",
           "workload": "ReplicaSet/shop-production-5c9f7d6b4", "project": "shop",
           "environment": "shop-production", "node": "node-a", "phase": "Running",
           "ready": false, "restarts": 3, "oomKilled": true,
           "message": "CrashLoopBackOff: back-off 5m0s restarting failed container"}],
 "totals": {"pods": 61, "running": 58, "pending": 2, "failed": 1, "notReady": 3,
            "restarts": 14, "oomKills": 1},
 "truncated": false}
```

The component survey's trick, applied cluster-wide: a workload whose pods are
refused at admission has nothing to show — the pod never existed, so nothing is
Pending and nothing is CrashLooping, and a listing of pods is a listing of the
healthy ones. `pods` on a workload row is how many exist, which is not
derivable from the replica counts beside it: zero available means pods that are
failing *or* pods that were never created, and only this tells them apart.
Where the two differ, `admission` carries the `FailedCreate` warning verbatim
out of the recorded event history, with `suspect` naming Pod Security where the
message betrays it.

Pods are credited to the object a reader recognises: a Deployment rather than
the ReplicaSet in between. `?namespace=` narrows both lists, `?limit=` bounds
the pod listing (500 by default, capped at 2000) and `truncated` says the cut
happened — the listing is sorted worst first, so what a limit drops is always
pods that are running normally.

### Edge

`GET /platform/edge` is the front door: what it served, across every project,
and whether the door itself is in one piece.

```json
{"requests": {"since": "…", "until": "…", "requests": 120000, "requestsPerSecond": 1.4,
              "errors": 240, "errorRate": 0.002, "p50Ms": 9, "p95Ms": 210, "p99Ms": 900,
              "unrouted": 340, "rollup": "1m"},
 "topRoutes": [{"key": "/api/:id", "project": "shop", "environment": "shop-production",
                "requests": 90000, "errorRate": 0.001, "p95Ms": 180}],
 "worstRoutes": [], "topHosts": [], "worstHosts": [], "latencyLeaders": [],
 "unrouted": [{"host": "old.example.com", "requests": 400, "requestsPerSecond": 0.11,
               "firstSeen": "…", "lastSeen": "…"}],
 "gateways": [{"namespace": "kitchen-system", "name": "kitchen", "class": "cilium",
               "addresses": ["203.0.113.7"], "programmed": true, "accepted": true,
               "listeners": [{"name": "https", "port": 443, "protocol": "HTTPS",
                              "attachedRoutes": 12, "programmed": true}]}],
 "tunnel": {"name": "kitchen-cloudflared", "desired": 2, "ready": 2, "available": 2,
            "restarts": 0, "healthy": true},
 "certificates": {"items": [{"namespace": "kitchen-system", "name": "kitchen-wildcard",
                             "dnsNames": ["*.apps.example.com"], "ready": false,
                             "notAfter": "2026-08-26T00:00:00Z", "daysToExpiry": 9.6,
                             "renewalTime": "2026-08-19T00:00:00Z",
                             "message": "Failed to wait for order resource …: DNS problem: NXDOMAIN"}]}}
```

`?since=`/`?until=` bound the traffic window (an hour ending now by default)
and `?limit=` how many rows each table carries (10 by default). The five
rankings are five reads rather than one sorted five ways, because the sort
decides which rows survive the limit — the ten busiest routes and the ten that
fail most are rarely the same ten. The two ranked by error rate drop rows with
too little traffic to rank, or the worst-performing host on the platform is
whichever scanner asked once and got a 404.

`unrouted` is the bucket of hosts that reached the edge which the platform
never published: a stale DNS record, a scanner, or a custom domain whose object
was removed while its record was not. The hostnames the platform's own routes
publish are subtracted from it — the dashboard and the identity provider are
served by routes that carry no project, so the store cannot attribute their
traffic either, and listing them here would say the platform never published
its own URL. The `unrouted` count on `requests` above still includes them,
because that number is what the edge served. `firstSeen`/`lastSeen` are what separate
those — a host asked for once an hour ago is noise, one asked for continuously
since a deploy is a route that stopped being published. It is read over its own
window rather than the screen's, because "still asking" is a question about a
stretch of time and not about wherever the chart was dragged to.

The certificate table is the other half of the screen, and `message` is the
most useful string on it: for a stuck ACME order it is the error the CA
returned, verbatim, which is the one thing that says what to fix. A healthy
certificate carries no message — cert-manager's "up to date and has not
expired" is what `ready` already said. `issuing` is set only while a renewal is
in progress, which is where a renewal that keeps failing reports itself: the
`Ready` condition stays true on the still-valid old certificate, so that is the
only place a stuck renewal says so. cert-manager not being installed is a
supported configuration (TLS mode `none`, or a certificate supplied by hand)
and answers an empty table with a message, not an error.

The traffic half needs the store; the edge's own objects do not. An
installation without telemetry still has a Gateway worth looking at, so the
answer degrades to the objects with `trafficMessage` set rather than to a
`503`.

An empty `gateways` is two different answers, and `gatewayMessage` is which.
Absent, the list is empty because the platform has no Gateway — the strongest
claim this endpoint makes, since nothing it publishes is then reachable.
Present, the list could not be read (the Gateway API kinds are not installed, or
the read was refused), and the emptiness proves nothing at all: the health strip
renders that as `unknown` rather than as the claim.

### Storage

`GET /platform/storage` is every volume the platform holds, what mounts it, and
the health of the one database Kitchen runs itself:

```json
{"volumes": 4, "unbound": 1, "filling": 0,
 "items": [{"namespace": "kitchen-shop", "name": "shop-data", "project": "shop",
            "phase": "Pending", "bound": false, "requested": "10Gi",
            "message": "this claim is not bound, so nothing that needs it can start; it names no storage class …"},
           {"namespace": "kitchen-system", "name": "data-kitchen-clickhouse-0",
            "phase": "Bound", "bound": true, "capacity": "50Gi",
            "pods": ["kitchen-clickhouse-0"]}],
 "store": {"bytesOnDisk": 5368709120, "capacityBytes": 53687091200, "usedFraction": 0.1,
           "claim": "data-kitchen-clickhouse-0", "rowsPerSecond": 42, "retentionDays": 30},
 "flows": {"events": 0, "notices": 0, "reconnects": 0, "windowSeconds": 3600, "lossless": true},
 "usageMessage": "this installation has no telemetry store, so how full each volume is is unknown rather than zero …"}
```

They are called volumes and not claims throughout, because `/claims` already
means something else in this API — a `ResourceClaim`, the platform's own kind
for a provisioned database — and two things called claims in one dashboard is
one too many.

An unbound volume names its own suspect: a claim Pending with no storage class
is waiting for the cluster's default, and a cluster without one is the
first-install hang the prerequisites warn about. Each row's `usage` is the
kubelet's own volume stats, read out of the store; where the store is absent or
the query failed, every row's usage is missing and `usageMessage` says so once
rather than a hundred empty bars saying nothing — and `filling` is a measured
zero only while that field is empty. `store` is the telemetry store's own size
against the volume underneath it, read from the same query the `store.disk`
signal fires on, so the screen and the finding cannot disagree about the number. `capacityBytes` is zero for an external store — the platform
does not own that disk and has no business judging it. `retentionDays` is the
one knob every table's TTL is derived from, which is the horizon past which the
store deliberately holds nothing.

`flows` is the loss the flow follower counted, and it is here as well as on
`/platform/ingest` because losing rows before they are written and running out
of disk to write them to are the same problem seen from two ends.

### Events

`GET /platform/events` is the cluster's Warning history — `FailedScheduling`,
`FailedCreate`, `FailedMount`, `OOMKilling` — which Kubernetes expires about an
hour after the fact and the operator records so that "what happened at 03:00"
has an answer. It is not the activity feed: `/events` is the platform's story,
written by the reconcilers about things Kitchen did; this is the cluster's,
about things that happened to it.

```json
{"items": [{"timestamp": "2026-08-16T03:14:00Z", "namespace": "kitchen-shop", "kind": "Pod",
            "name": "shop-production-5c9f7d6b4-abcde", "reason": "FailedScheduling",
            "message": "0/3 nodes are available: insufficient memory", "count": 12,
            "node": "node-b", "project": "shop", "environment": "shop-production"}],
 "facets": [{"field": "reason", "values": [{"value": "FailedScheduling", "count": 12}]},
            {"field": "kind", "values": []},
            {"field": "namespace", "values": []},
            {"field": "node", "values": []}],
 "truncated": false}
```

| Parameter | Meaning |
|---|---|
| `since` / `until` | RFC 3339 bounds. An hour ending now by default |
| `project` / `environment` | One application's events. Platform objects carry neither |
| `namespace` / `kind` / `name` / `reason` / `node` | The facets, as filters — and the deep link from any other screen |
| `search` | Full text over the message, case-insensitively |
| `limit` | Rows to return, default 100, capped at 1000 |

This is the one platform screen that is nothing but a store read, so it is also
the one that answers `503` on an installation without a telemetry store rather
than degrading — there is no half of it to serve.

The facets are counted over the rows that came back, not over the whole window,
which is what `truncated` is there to say: at the limit they describe the page.
That is the right trade at this size — the page is a thousand events at most,
and a second aggregate per field would be four more queries for a number nobody
sums. `count` on a row is Kubernetes' own repeat count for that event; the
facet counts are rows, so the two deliberately do not add up to each other.

### Ingest

`GET /platform/ingest` is whether the platform is still hearing from its own
collection layer, and what it knows it has lost:

```json
{"silentNodes": 1, "nodesWithoutCollector": 1,
 "items": [{"node": "node-b", "collector": "CrashLoopBackOff: back-off 5m0s …",
            "telemetry": {"lastSeen": "2026-08-16T09:26:00Z", "silent": true, "ageSeconds": 2040}}],
 "collector": {"present": true, "namespace": "kitchen-system", "name": "kitchen-collector",
               "desired": 3, "ready": 2, "available": 2},
 "flows": {"events": 4096, "notices": 3, "reconnects": 1,
           "windowSeconds": 3600, "latest": "2026-08-16T09:58:00Z", "lossless": false}}
```

Three readings of the same question, because each catches a failure the others
cannot. Per-node freshness catches a collector that stopped shipping. The
DaemonSet's own counts catch the one that never started — `desired: 3` with
nothing available and no pods on any node is admission refusing them, which
leaves nothing for a pod listing to show. And `flows` is the only evidence that
a *plausible* number is wrong: Hubble reports the events it dropped, so a
request count that under-reports says so here instead of looking like a quiet
hour. `lossless` is stated rather than left to be inferred from three zeroes,
and `windowSeconds` is how far back the counts reach — they are the follower's
trailing hour, not a total since start.

The counts come from whichever replica answers the request, and the follower
runs on the leader alone: a replica that never followed reports no loss because
it did no following.

### Backup

`GET /platform/backup` is what an archive taken now would hold, before anybody
takes one:

```json
{"platformVersion": "0.9.0", "clusterName": "prod", "baseDomain": "apps.example.com",
 "resources": {"projects": 4, "releases": 31, "environments": 7}, "secrets": 9,
 "accounts": {"available": true, "database": "kitchen"},
 "excluded": ["telemetry: logs, metrics, traces and flow data in ClickHouse are not backed up …"],
 "snapshots": {"supported": false,
               "message": "the VolumeSnapshot API is registered but no VolumeSnapshotClass exists …"},
 "filename": "kitchen-backup-prod-2026-08-19T090000Z.tar.gz"}
```

`excluded` is served rather than written into the dashboard, so the screen and
the archive's own manifest cannot come to disagree about what is missing.
`accounts.available` distinguishes the two reasons an archive carries none: an
installation with no identity provider has none to take, and one whose database
cannot be reached has accounts it is not backing up — a difference that would
otherwise only surface at restore time. `snapshots` is checked rather than
assumed, because a cluster can run a snapshot controller with no CRDs
registered, where a `VolumeSnapshot` is accepted by nothing and nobody is told.

`POST /platform/backup` answers the archive itself: `application/gzip`, with a
`Content-Disposition` naming the installation and the day. It is a POST and not
a GET because the body is every credential the platform holds — not something
to leave in a browser history or a proxy cache — and because it is recorded in
the audit log as an `export` against the `Kitchen` object. The headers go out
before the archive is built, so a failure part-way truncates the stream rather
than becoming a JSON error; a truncated archive is what a restore refuses, and
the operator's log carries the reason.

There is no restore route, and its absence is the design. A restore happens
into a cluster whose accounts database is gone, so the credentials to
authenticate here are inside the archive and there is nobody left to call it.
The chart renders a Job for it instead — see
[docs/BACKUP.md](../BACKUP.md).
