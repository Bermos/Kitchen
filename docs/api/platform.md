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
{"evaluatedAt": "2026-08-16T10:00:00Z", "source": "recorded",
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
it.

**`source` is which of two answers this is**, and both are the same shape:

- `recorded` — the operator's background evaluation loop ran a round, diffed
  it against the previous one and wrote the transitions; this is the set of
  conditions it holds open. `evaluatedAt` is when that round ran, not when the
  newest transition happened: a platform where nothing has changed for a day
  is current, not stale.
- `evaluated` — a round taken to serve this request, exactly as this endpoint
  has always answered. It is what comes back when the loop is switched off
  (`spec.observability.signals.enabled`), when the installation has no
  telemetry store to record into, when the last round is more than three
  intervals old, or when the history could not be read.

The fallback is not a transitional courtesy. An empty problems list is the
strongest claim this platform makes, and it must never be made because
detection was not running.

Findings from a recorded round carry one thing an evaluated round cannot know:
`since` is still what the objects can prove, but the platform's own record of
when it *first saw* the condition is in the store beside it.

That is the question [`GET /alerts`](alerts.md) asks, and it is why this
endpoint did not grow to answer it. This one answers **what is wrong**, one row
per (signal, scope), and is unchanged. That one answers **and what am I meant
to do about it**, one row per *delivery* — the pair (fingerprint, audience) —
with the tier its reader reads it at, how long it has gone unacknowledged, and
what anybody has done about it. A developer-audience condition is one finding
here and two deliveries there, which is why they are two endpoints rather than
one with more fields.

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

This screen counts the platform's own health checks, and that is the one place
they are counted: a project's traffic numbers leave them out (see
[telemetry](telemetry.md) and
[environments](environments.md#the-platforms-own-health-checks-are-not-traffic))
because a probe is not a visit, while this is the question "what crossed the
front door", whose honest answer includes every probe and every scanner.

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
            "pods": ["kitchen-clickhouse-0"], "expandable": true,
            "resize": {"statefulSet": "kitchen-clickhouse", "desired": "80Gi",
                       "phase": "Growing", "message": "growing from 50Gi to 80Gi"}}],
 "store": {"bytesOnDisk": 5368709120, "capacityBytes": 53687091200,
           "usage": {"usedBytes": 47781511168, "capacityBytes": 53687091200, "usedFraction": 0.89},
           "usedFraction": 0.89, "claim": "data-kitchen-clickhouse-0",
           "rowsPerSecond": 42, "retentionDays": 30},
 "flows": {"events": 0, "notices": 0, "reconnects": 0, "windowSeconds": 3600, "lossless": true},
 "usageMessage": "this installation has no telemetry store, so how full each volume is is unknown rather than zero …"}
```

They are called volumes and not claims throughout, because `/claims` already
means something else in this API — a `ResourceClaim`, the platform's own kind
for a provisioned database — and two things called claims in one dashboard is
one too many.

`expandable` is whether the volume's storage class admits expansion, which is
what decides whether growing it is an operation at all; it is absent, rather
than false, where nobody could tell — the classes were unreadable, or the claim
names one that is gone. `resize` is present only for the platform's own
volumes, because a project's claim is not one this platform grows: it carries
the StatefulSet the volume came from, the largest size anybody has asked for,
and a phase with the sentence behind it: `Settled`, `Growing` (the platform is
expanding the claims and rewriting the template), `Resizing` (it has done its
half and the storage driver has not finished its — the row's `requested` is
past its `capacity`, and the message carries the driver's own account),
`Blocked`, or `Unknown` where a read failed. `Blocked` is a fact about the
cluster and is never retried on a timer; `Unknown` always is.

### Growing a platform volume

`POST /platform/storage/claims/{name}/resize` asks for one of the platform's
own volumes to be bigger. The name is the claim's, in `kitchen-system`:

```json
{"size": "80Gi"}
```

```json
{"claim": "data-kitchen-clickhouse-0", "statefulSet": "kitchen-clickhouse",
 "current": "50Gi", "desired": "80Gi",
 "message": "the volume is being grown to 80Gi; kitchen-clickhouse is replaced with a matching claim template once the expansion is under way, and this screen reports the outcome"}
```

It answers `202`, and it writes a size and nothing else. A StatefulSet's
`volumeClaimTemplates` are immutable, so growing one of these volumes means
expanding every claim it made, orphan-deleting the StatefulSet and creating it
again with a template that matches — three things Helm cannot do and the
operator can, which is why the work is `KitchenReconciler`'s and the outcome is
read back off `GET /platform/storage` as the `resize` block and off the
`VolumesResized` condition on the Kitchen singleton.

The same size is also set from the chart, as `clickhouse.persistence.size` and
its three siblings, and neither overrules the other: **a volume is only ever
grown, to the largest size anybody has asked for.** That is what makes a `helm
upgrade` still carrying the old value harmless — it cannot undo a resize done
from here — and it is why carrying the number back into the values afterwards
is worth doing, so that the two agree. A volume larger than the chart asks for
says so in `resize.message`.

Five requests are refused rather than accepted and quietly abandoned:

- a size at or below what the claim already requests (`400`) — shrinking a
  volume means replacing it and restoring what was on it, which is not a
  resize;
- a size more than eight times what it requests now (`400`), naming the
  ceiling. Growing a volume cannot be undone, and a step that large is more
  likely a typed unit than a decision — grow it in steps instead;
- a claim that is not bound (`400`). The API server refuses a size change to
  an unbound claim outright, so the expansion has nowhere to happen yet;
- a volume in `kitchen-system` that no platform StatefulSet created (`400`) —
  storage somebody wrote for a project to mount, say. A claim in any other
  namespace is a project's and is not this route's at all, so it answers
  `404`, like everything else a caller may not act on;
- a volume whose storage class does not allow expansion (`400`), naming the
  class. That is the one refusal nothing on either side can work around.

There is no `kitchen` command for it: it is an operator's one-off against their
own installation, and `kitchen api POST
/platform/storage/claims/data-kitchen-clickhouse-0/resize --data '{"size":
"80Gi"}'` reaches it authenticated like any other route.

An unbound volume names its own suspect: a claim Pending with no storage class
is waiting for the cluster's default, and a cluster without one is the
first-install hang the prerequisites warn about. Each row's `usage` is the
kubelet's own volume stats, read out of the store; where the store is absent or
the query failed, every row's usage is missing and `usageMessage` says so once
rather than a hundred empty bars saying nothing — and `filling` is a measured
zero only while that field is empty. `store` is the telemetry store's own
health, and it carries **two** sizes because they answer two questions. `usage`
is how full the volume is, from the same kubelet stats every row above carries,
matched to the store's claim — it is everything written to that disk, and it is
what the `store.disk` signal fires on, so the screen and the finding cannot
disagree about the number. `bytesOnDisk` is what the telemetry itself occupies,
which is the number retention governs and not a fill level: anything else on the
same volume is in neither it nor `capacityBytes`, and dividing the one by the
other read 8.4% on a volume that was 89% full. `usage` is absent where nothing
measured the disk and `usageMessage` says why, because an unmeasured volume must
not render as an empty one. `usedFraction` is `usage.usedFraction` kept flat, so
that a reader of the field this endpoint has always had goes on working — but
**its meaning moved with this fix**: it is now how full the volume is, and no
longer `bytesOnDisk` over `capacityBytes`. It is absent, like `usage`, where
nothing measured the disk, so a zero there is a measured zero. `capacityBytes`
is the claim's nominal size and is zero for an external store — the platform
does not own that disk and has no business judging it. `retentionDays` is the one knob every table's TTL is derived
from, which is the horizon past which the store deliberately holds nothing.

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

### Retention

`GET /platform/retention` is how long each class of what the platform keeps is
kept, and — the half that matters — how far back each one actually goes:

```json
{"classes": [
   {"class": "containerLogs", "label": "Container logs",
    "description": "stdout and stderr from application, platform and cluster containers",
    "days": 14, "source": "retention", "enforced": true,
    "rows": 41203311, "oldest": "2026-08-10T04:11:02Z", "expired": 0},
   {"class": "buildLogs", "label": "Build logs", "description": "…",
    "days": 180, "source": "retention", "enforced": true,
    "rows": 88214, "oldest": "2026-02-26T12:00:41Z"},
   {"class": "audit", "label": "Audit log", "description": "…",
    "days": 365, "source": "compliance.audit.retentionDays", "enforced": true,
    "rows": 90112, "oldest": "2025-08-24T09:00:00Z"}],
 "auditFloorDays": 90, "auditFloorOverridden": false,
 "lastSweep": "2026-08-24T03:00:00Z"}
```

`source` is the field the number came from: `retention` when somebody set that
class, and the name of the knob it inherits otherwise
(`observability.clickhouse.retentionDays` for a telemetry class,
`compliance.audit.retentionDays` for the audit one). It is served because an
operator reading "30" wants to know whether anybody chose it.

`oldest` is the claim retention actually makes — nothing of this class is older
than this — and it is a *measurement*, taken by the daily retention sweep,
rather than a restatement of `days`. `expired` counts rows still on the wrong
side of the horizon at that moment; a small number is normal (a day-partitioned
table keeps at most the partition the horizon falls inside) and a number that
stays large is the store holding data past its date, which is a thing this API
reports rather than hides. Both are absent until a sweep has run, and
`enforced` is false while they are: what the platform has *decided* to keep is
answered from the moment it is configured, and what it is *doing* is answered
once something has looked.

`auditFloorDays` is served rather than assumed by the client, so the dashboard
is not a second copy of the number.

`PATCH /platform/retention` changes any subset of the classes. Every field is
optional and an absent one is left alone, so a form can send only what moved:

```json
{"buildLogs": 180, "flows": 7}
```

Zero is refused rather than interpreted: there is no value meaning "keep
nothing", and the way a class goes back to inheriting is `kubectl`-free but not
this route's — it is clearing the field on the singleton.

**The audit floor is the one refusal worth reading.** An audit retention under
90 days without an override is answered `400` with the field, the number, the
floor and the way past it, by this route *and* by a CEL rule on the CRD — so a
`kubectl apply` behind the platform's back is refused too:

```json
{"audit": 60,
 "auditFloorOverride": {"reason": "demonstration cluster; holds no production data at all",
                        "approvedBy": "cto@example.com"}}
```

The override is read back in full by `GET`. That is deliberate and is not the
API reading a credential back: the whole value of the field is that somebody
outside the platform can see who signed off on keeping less evidence. A write
that uses it is recorded in the audit log under kind `Retention` with
`details.change` `audit-floor-override`, carrying the number, the floor, the
reason and the approver — which is what "the override is itself an audit
record" means. Removing it is `{"clearAuditFloorOverride": true}`, and clearing
one while the retention is still under the floor is refused for the same reason
setting the retention low without one is.

The daily sweep's own record is the other half of this surface, and it is in
the audit log rather than here: one record a pass, kind `Retention`,
`details.change` `retention-sweep`, carrying every class with the horizon it
was measured against and what the pass removed. Read it with
`GET /audit?kind=Retention`.

### Signal policy

`GET /platform/policy` is what this installation counts as worth hearing.

It is the operator's alone and it is installation-wide, which is the
distinction it exists to draw. [`/alerts → Routing`](alerts.md) is *who hears
about a condition*, and a project edits its own; this is *what makes something
a condition at all*, every number applies to every project, and the compliance
posture reads it.

**A project cannot yet tighten its own thresholds.** #472's design says it
should be able to — asking to be woken more often costs the operator nothing —
and that half is not built; it is
[#519](https://github.com/Bermos/Kitchen/issues/519). Until it lands these
values are the whole answer for every project, not a floor with overrides
above it.

```json
{"preset": "balanced", "modified": false,
 "correlatedProjects": 3, "correlationWindowMinutes": 15,
 "escalationWindowMinutes": 60, "untendedMultiple": 4,
 "maxSilenceHours": 720, "paging": true,
 "untendedAfterHours": 4,
 "provenance": "preset=balanced correlatedProjects=3 correlationWindow=15m0s escalationWindow=1h0m0s untendedMultiple=4 maxSilence=720h0m0s paging=on",
 "presets": [
   {"name": "strict", "description": "Two projects are a correlation, …",
    "correlatedProjects": 2, "correlationWindowMinutes": 30,
    "escalationWindowMinutes": 30, "untendedMultiple": 2,
    "maxSilenceHours": 168, "paging": true},
   {"name": "balanced", "description": "The platform's own judgement, …", "…": "…"},
   {"name": "homelab", "description": "One host and a handful of projects, …",
    "correlatedProjects": 2, "correlationWindowMinutes": 60,
    "escalationWindowMinutes": 240, "untendedMultiple": 6,
    "maxSilenceHours": 720, "paging": false}]}
```

The six numbers are the whole of what is configurable, and that bound is the
point. Which signals exist, what they compute and what each one asks of its
reader stay versioned code — see
[docs/OBSERVABILITY.md §9](../OBSERVABILITY.md) — because two installations on
catalogue v1 that disagreed about what a rule *is* would make the version
meaningless.

- `correlatedProjects` — how many projects must be degrading together before it
  is one platform problem rather than several application problems.
- `correlationWindowMinutes` — how far apart two failures may have started and
  still count as the same moment. It is also how far back the ladder's third
  rung looks for a change of the platform's own, clamped to the hour that
  timeline is actually gathered over — a window set wider searches a stretch
  nothing was read for, and the finding says which span it checked.
- `escalationWindowMinutes` — how long an owner-tier condition may sit
  unacknowledged before the operator is added to it. It repeats and adds a
  ticket; nothing becomes more urgent, because nothing is more broken at hour
  four than at hour one.
- `untendedMultiple` — how many of those windows it survives before it stops
  being an alert and becomes a line on the compliance posture.
  `untendedAfterHours` is the product, served so nothing has to multiply.
- `maxSilenceHours` — the longest silence a member may set on their own row.
- `paging` — whether the `page` tier is delivered as a page at all. False holds
  every paging condition down to a ticket, for every project on the
  installation, which is the homelab reading: nobody is on call for a house. It
  holds it down at both places a delivery is read — the alerts feed the screens
  render, and the `signal.firing`
  [subscriptions](notifications.md#signals-the-third-trigger-and-the-one-with-a-filter),
  where it is also what a `minTier` floor is compared against.

`preset` is the base in force and `modified` says whether somebody has moved a
number off it — "balanced" and "balanced, with two numbers moved" are different
sentences, and only the second warns that choosing the preset again would undo
something.

`provenance` is the string every finding evaluated under this policy carries,
served rather than paraphrased so a screen shows the reader the thing they will
later find on a finding. **A finding records the thresholds it was evaluated
against**, which is what keeps `v1 @ correlatedProjects=2` and
`v1 @ correlatedProjects=3` distinguishable after the fact — a compliance
posture that reads these numbers has to be able to say what they were at the
time, and an audit pack that cannot is not evidence.

`PATCH /platform/policy` changes any subset. Every field is optional and an
absent one is left alone, so a form can send only what moved:

```json
{"correlatedProjects": 2, "maxSilenceHours": 168}
```

Naming a preset is the exception in one direction: it **rebases**, clearing
every override the installation had, and a request that also sets a number is
choosing the preset and then moving that one.

```json
{"preset": "homelab"}
```

A number outside its bound is answered `400` naming the field, the range and
what the number is for. A write that changes nothing is answered `200` and
recorded as nothing — the log is for changes. A write that does change
something is recorded in the audit log under kind `SignalPolicy` with
`details.change` `signal-policy`, carrying what moved and the provenance on
both sides of it. Read it with `GET /audit?kind=SignalPolicy`.

Nothing in the CLI carries this surface as a command of its own, deliberately:
it is a decision an operator makes once and reads off a screen, not something a
pipeline sets. `kitchen api GET /platform/policy` and
`kitchen api PATCH /platform/policy --data '{"preset": "homelab"}'` reach it
authenticated, like any other route.

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
 "schedule": {"schedule": "0 3 * * *", "suspended": false, "timeoutMinutes": 30,
              "destination": {"type": "s3", "described": "s3://kitchen-backups/prod",
                              "bucket": "kitchen-backups", "prefix": "prod",
                              "region": "eu-central-1", "credential": "stored"},
              "encryption": {"mode": "aes256-gcm", "key": "stored"},
              "keepLast": 30,
              "lastSuccess": "2026-08-19T03:01:44Z",
              "lastSuccessArchive": "prod/kitchen-backup-prod-2026-08-19T030102Z.tar.gz",
              "lastSuccessBytes": 4718592, "archives": 30,
              "ready": true, "reason": "BackedUp",
              "message": "the last archive was written to s3://kitchen-backups/prod 6 hours ago"},
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

#### The schedule

`schedule` on that answer is the scheduled backup, and `schedule.lastSuccess`
is the field worth reading first. A screen that only said what an archive
*would* carry could not tell an operator that the last one was taken in March,
and six weeks of no archive that nobody noticed is a backup system's
characteristic failure — not a corrupt archive. The same object is served on
`GET /settings` as `backup`, so the settings screen and the backup screen
cannot come to disagree; `ready`, `reason` and `message` are the platform's own
`BackupReady` condition rather than a second opinion derived here.

`destination` describes where archives go and never how it authenticates.
`credential` says only *how*: `stored` is a key this platform holds in a
Secret, `ambient` is the credential chain the pod already has — IRSA, EKS Pod
Identity, an instance role — which is the better answer where it is available,
because there is then no long-lived key anywhere to leak.

The **schedule, the suspend and the retention** are ordinary settings and are
changed through `PATCH /settings`:

```json
{"backupSchedule": "0 3 * * *", "backupSuspend": false,
 "backupKeepLast": 30, "backupKeepDays": 90}
```

Every field is optional and a field that is absent is untouched. An empty
`backupSchedule` turns the scheduled backup off; `0` on either retention bound
removes that bound, which is the only way back to keeping everything. A
schedule with no destination is refused here as well as at admission — an
archive written to a volume on this cluster does not survive the loss of this
cluster, so there is deliberately no local destination — and so is a retention
with nothing to prune.

The schedule is a **five-field cron expression in UTC**, as every schedule on
this platform is. A quiet hour is the right answer: the accounts half of an
archive is taken through the identity provider's database.

#### The destination

`PUT /platform/backup/destination` is where archives go, and it has an address
of its own for one reason: **it carries a credential, and `PATCH /settings`
must never carry one.**

```json
{"type": "s3",
 "s3": {"bucket": "kitchen-backups", "prefix": "prod", "region": "eu-central-1",
        "endpoint": "https://minio.example.com", "forcePathStyle": true,
        "serverSideEncryption": "AES256",
        "accessKeyId": "…", "secretAccessKey": "…"},
 "encryption": {"key": "…"}}
```

`s3` is any S3-compatible store — AWS, MinIO, R2, Backblaze, Wasabi, Ceph,
Garage — because `endpoint` and `forcePathStyle` make those one code path
rather than six backends. `region` is wanted even by stores where it means
nothing; `us-east-1` is the conventional answer for those.

`endpoint` must be `https://`. Empty is the AWS endpoint, which is https either
way; anything else is `400` naming `allowInsecureEndpoint`, which is how an
installation whose store really is reached over a network it trusts says so.
The archive is every credential this platform holds, and this is the rule
notification webhooks already keep. A CEL rule on the CRD refuses the same
write from any other direction, and the run refuses it a third time — an object
written before the rule existed must not go on uploading over plain HTTP.

An `endpoint` on a `.svc` name over `https://` is a store inside this cluster,
and no public authority issues for a name nobody owns — so it is verified
against the platform's own internal CA, which the operator and every scheduled
run mount. That is what lets the bundled object store be a backup destination
now that it serves TLS (#382). Every other endpoint is verified against the
host's roots, as before; there is no value here that turns verification off.

The operator writes the key pair into a Secret carrying
`app.kubernetes.io/managed-by: kitchen`, patches the singleton to point at it,
and answers with the same view `GET /platform/backup` serves — **bucket and
prefix, and no key, ever**. The three things the credential half can say:

- `accessKeyId` **and** `secretAccessKey` store a new key. Half a pair is
  refused rather than discovered on the first run.
- `"ambientCredentials": true` moves the destination onto the pod's own
  credential chain and deletes the key this platform was storing. It is
  explicit because the API never reads a credential back, so a form
  redisplaying a destination cannot send the key it never received.
- Neither: the destination is rewritten and whatever credential is stored
  stays. An unmentioned key must survive an edit of the bucket's prefix.

`encryption` is what protects the archive **at** the destination, which is a
different question from `serverSideEncryption`: a store that encrypts at rest
decrypts for anybody it answers, so that rests the confidentiality of the
cluster's root credentials on the bucket's own configuration. `mode` is
`aes256-gcm` — the default, and what an unset mode means — or `none`, and
`key` is 32 bytes as base64 or hex.

- The key is **supplied and never generated here**, for the reason a
  notification subscription's signing key is: a key this platform minted and
  answered once would live in a shell history, a browser's memory and whatever
  logged the response, and the day it is needed is the day this cluster is
  gone. The dashboard's Generate button mints one in the browser.
- It is written into `kitchen-backup-encryption-key`, carries the same
  `managed-by` label, and **is never read back**. Leaving `key` out means
  "leave the stored one alone", exactly as it does for the bucket's credential.
- A destination whose archives would be neither encrypted nor deliberately
  unencrypted is **refused**: `400`, naming both ways out. There is no state in
  which this platform uploads the archive in the clear without having been
  asked to.
- The key is deliberately **not in the archive**, so a restore needs the copy
  whoever supplied it kept. See [docs/BACKUP.md](../BACKUP.md#what-protects-the-archive-at-the-destination).

`GET /platform/backup` answers `encryption` as `{"mode": …, "key": "stored" |
"absent"}` — whether there is a key, never what it is. An installation whose
mode is `aes256-gcm` with no key stored is one whose next run will refuse to
upload, and the `BackupReady` condition says so with reason
`EncryptionKeyMissing` rather than waiting for 02:00.

`DELETE /platform/backup/destination` removes the destination and, with it, the
Secret this API wrote — and only that one: a Secret something else put there
carries no `managed-by` label and is left where it is. The retention goes too,
because it prunes what is at the destination and with none it overrides
nothing. A destination still carrying a schedule is `409`, naming the field to
clear first, rather than handing back a CEL rule's message. The archive's
encryption key is deliberately *not* removed with it: the bucket's credential
opens a destination nothing writes to any more, and the key opens the archives
that are still sitting in it.

#### Runs

`GET /platform/backup/runs` is what the destination *actually holds*:

```json
{"destination": "s3://kitchen-backups/prod/",
 "objects": [{"key": "prod/kitchen-backup-prod-2026-08-19T030102Z.tar.gz",
              "size": 4718592, "modified": "2026-08-19T03:01:44Z", "archive": true}]}
```

It reads the bucket rather than the platform's own status on purpose: the
status says what the last run believed it did, and this says what is there now,
which is the only half a recovery can use. Objects that are *not* archives this
platform wrote are listed too, with `archive: false`, precisely so that nobody
has to wonder what retention would touch — pruning only ever considers keys
named the way this platform names an archive. A destination that cannot be
reached is `502` with the store's own message; a listing longer than 200
objects is cut newest-first and says `"truncated": true`.

`POST /platform/backup/runs` takes one now, to the destination, without
downloading anything. It answers `202` with the Job's name:

```json
{"job": "kitchen-backup-manual-x7k2p", "destination": "s3://kitchen-backups/prod"}
```

This is what makes a destination testable: press it once on the day it is
configured and find out whether the credential works, rather than at 02:00 six
weeks later. Nothing from the request reaches the Job — the pod template is the
scheduled CronJob's own, copied, with a TTL added because this run is owned by
nobody. It is recorded in the audit log as an `export` against the `Kitchen`
object, exactly as a download is, because an archive leaving the cluster is the
same event whichever direction it left in.

## Platform credentials

`GET`, `POST /platform/credentials` and `DELETE /platform/credentials/{name}`
are what this platform has handed to things that are not people — a scheduled
job, an agent — and what each of them may do
([#349](https://github.com/Bermos/Kitchen/issues/349)).

They are the project key routes one level up. A credential is created at the
identity provider with an account of its own to own it, because a key has no
`sub` of its own, and the grant that makes it useful goes on the `Kitchen`
singleton — where it carries **scopes** rather than a role. The whole of the
model, and why it is scopes rather than a fourth platform role, is
[AUTH.md, "Platform credentials"](../AUTH.md#platform-credentials); this is the
wire.

### Listing

```json
{"items": [
  {"name": "nightly", "subject": "user_01H8X…",
   "email": "nightly@platform.kitchen.local",
   "scopes": ["platform.read"], "expires": "2026-10-07T09:14:00Z",
   "expired": false, "prefix": "a3f19c",
   "created": "2026-09-07T09:14:00Z", "lastUsed": "2026-09-07T03:00:11Z"},
  {"name": "quarterly-evidence", "subject": "user_01H8Y…",
   "email": "quarterly-evidence@platform.kitchen.local",
   "scopes": ["compliance.read"], "projects": ["billing", "shop"],
   "expires": "2026-09-06T09:00:00Z", "expired": true,
   "prefix": "77b201", "created": "2026-06-08T09:00:00Z"}]}
```

**There is no credential value here and there never is one again.** It is
stored hashed at the identity provider, so a listing carries only `prefix` —
enough to tell two apart and useless as a credential.

`scopes` is read from the platform's grant rather than from anything stored on
the credential, so an **empty** list is a credential whose grant has been
removed: it still authenticates and can do nothing, and the listing says so
rather than hiding it. `expired` is answered rather than left to the reader to
compute, because a lapsed credential is the state the screen exists to make
visible — and the platform's own sweep will remove it within a few minutes,
along with the account behind it.

### Issuing

```sh
POST /platform/credentials
{"name": "nightly", "scopes": ["platform.read"], "expiresInDays": 30,
 "projects": []}
```

answers `201` with the listing's shape plus the credential itself:

```json
{"name": "nightly", "subject": "user_01H8X…",
 "email": "nightly@platform.kitchen.local", "scopes": ["platform.read"],
 "expires": "2026-10-07T09:14:00Z", "expired": false, "prefix": "a3f19c",
 "created": "2026-09-07T09:14:00Z", "key": "a3f19c…"}
```

**That is the only response that carries `key`.** Nothing stores it in a form
anything can read back, so a lost credential is revoked and reissued.

| Field | |
|---|---|
| `name` | Lowercase letters, digits and dashes, at most 32 characters — the same DNS-label rule a project and a CI key follow, because it is the local part of the account's address and a path segment here. One credential per name |
| `scopes` | Required, and at least one: there is no scope that is obviously the one somebody meant, and a credential with none would authenticate and be able to do nothing. An unknown scope is a `400` naming the vocabulary rather than a credential quietly narrower than the one that was asked for |
| `expiresInDays` | Defaults to 30, capped at 90. `400` outside that — a credential that has to outlive a quarter is one to reissue, which is an action somebody takes and the log records |
| `projects` | Optional. Narrows the scoped routes that are about one project — today that is `GET /projects/{name}/audit-pack` — to the ones named. Empty is every project, because that is what a platform scope means when nobody narrowed it. A name that is not a project is a `400`: the whole point of the list is that it narrows, so a typo in it would narrow to nothing |

Both halves land or neither. The credential is created at the issuer, the grant
is written on the singleton, and a grant that will not land takes the credential
back with it — a credential nothing has granted anything to is one nothing in
Kitchen lists.

### Revoking

`DELETE /platform/credentials/{name}` answers `204`. The credential goes first
and the grant comes off after: of the two ways this can end up half done, a
grant naming an account that no longer exists is a line to tidy up and a
credential that still works is not.

A grant whose credential is **already** gone at the issuer is this route's to
remove too — it is the half left behind by an interrupted revocation, and
leaving it would mean the only way to tidy it is `kubectl`.

### What issuing one is not

`POST /platform/credentials` requires the operator role and names **no scope**,
which is what stops a credential minting its own successors, outliving its own
expiry by issuing a fresh one, or granting itself scopes nobody chose. The same
holds for `PATCH /settings`, `/updates` and every connection write, and
`TestNoScopeReachesCredentialIssuance` is what keeps it true as the table grows.

### When the issuer has none

An installation federated to an issuer of its own answers `503` here, with the
sentence that says why: credentials are that issuer's to hand out, and this
platform has no endpoint at it to ask through. It is the same answer
`POST /projects/{name}/keys` gives for the same reason.
