# Kitchen — Observability Design

> Status: **implemented**, stages 0–4. This document is kept as the design it
> was, in the present tense it was written in, because the reasoning is the
> part worth keeping — what was rejected and why is not recoverable from the
> code. Stage 5 (background evaluation, `signal_transitions`, the inbox and
> delivery) is still designed-for and not built; §7's model is what makes it
> additive.
>
> Two things the implementation learned that this text could not, both about
> metrics the collector was assumed to emit and does not — see the verification
> record, which now records them.
>
> Stage 0 has since run against a real Cilium in CI and proved the one fact
> this design rests on, along with four of §9's five open questions. The
> readings are in the verification record.

Kitchen's observability serves two people. The developer who deployed an
application wants to click their project, click Logs or Metrics, and have the
answer be there — without having instrumented anything, configured anything,
or learned anything about the cluster underneath. The operator running the
platform wants to know that pods are failing, that pods are silently *not
being admitted*, that a node is filling its disk, that a certificate renewal
is stuck, that latency is rising across three projects at once — ideally
before anyone else notices, and with the evidence attached.

Both are served from **one collection layer and one store**. There is no
developer pipeline and operator pipeline; there is one set of tables in
ClickHouse and two sets of screens over them.

The scope boundaries set for this design: traces have no UI yet (the design
notes where it would attach and forecloses nothing); the Hubble flow *diagram*
is not a priority (Hubble as a data source very much is); background
evaluation and alert delivery are out of scope but designed for, so that
adding them later is an engine and an inbox, not a rework.

---

## 1. What the platform can and cannot know

Everything below follows from this line, so it comes first. Without the
application's cooperation, the platform can observe an application only from
the outside: what it prints, what the kernel accounts to it, what the API
server decided about it, and what its traffic looks like at points the
platform controls.

**Obtainable with zero application effort:**

- **Everything the application writes to stdout/stderr**, including structured
  fields of JSON lines. Already collected.
- **Every HTTP request that enters through the platform's edge** — method,
  path, status, duration — because the shared Gateway's Envoy proxies all of
  them. This is the load-bearing fact of the design; §3 is about it.
- **Container CPU and memory usage**, from the kubelet. Already collected.
- **Node saturation** — CPU, load, memory, disk fill, network. Already
  collected.
- **Everything the API server knows**: restarts, OOM kills, limits, replica
  counts (already sampled), plus pod phases, admission rejections, scheduling
  failures, node conditions, PVC states, certificate states, and the Warning
  events that carry the reasons (watched today only for the component survey;
  this design stores them).
- **Network flows** between workloads at L3/L4, and drops, from Hubble.
  Already collected.
- **Persistent volume usage**, from the kubelet's volume stats — obtainable
  and currently *not* collected; this design turns it on.

**Not obtainable without instrumentation, and never promised:**

- **Why a request was slow** — which database query, which downstream call,
  which lock. The edge sees one duration; the breakdown inside it belongs to
  traces, which need the application's SDK.
- **Exceptions and stack traces** the application did not log.
- **Requests that never reach the edge** — DNS failures, TLS handshake
  failures at a fronting CDN, connections that die before Envoy accepts them.
  The platform can observe *its* edge, not the internet.
- **Which release served a given request.** The edge routes to a Service, not
  to a pod; during a rollout both revisions answer under one route. Request
  rows therefore carry project and environment, never build or release.
- **East-west HTTP detail.** Service-to-service calls inside the cluster do
  not cross the Gateway, so they appear as L4 flows (bytes, connections,
  drops), not as requests. Honest today; §3.5 names the one credible way to
  close it later.
- **gRPC failure semantics.** A gRPC error is HTTP 200 with a `grpc-status`
  trailer; the edge vantage records it as a success. Flagged on the screen
  rather than silently wrong (§3.4).

Where optional instrumentation slots in when someone wants the other side of
the line: the OTLP endpoint every environment is already handed
(`OTEL_EXPORTER_OTLP_ENDPOINT`, spans land in `otel_traces` today). A request
row carries a `trace_id`, read off the flow's trace context where the request
arrived with one, so an instrumented application's requests link straight to
their traces. The column was reserved before anything could fill it, which is
why filling it cost no migration. Nothing else in this design depends on instrumentation.

---

## 2. Data sources, and what each one is uniquely able to answer

| Source | Mechanism | Uniquely answers | Status |
|---|---|---|---|
| Container log files | node collector, `file_log` | what the application said; crash forensics; build output | shipped |
| **Gateway Envoy, via Hubble L7 flows** | operator follows Hubble Relay | per-request method, path, status, latency for *all* inbound HTTP, uninstrumented | **new use of an existing pipe** |
| Hubble L3/L4 flows | same follower | who talks to whom; drops; liveness of non-HTTP traffic | shipped |
| kubelet (`kubelet_stats`) | node collector | container CPU/memory usage; **volume usage (to enable)** | shipped / new group |
| node (`host_metrics`) | node collector | node saturation: CPU, load, memory, disk, filesystems, network | shipped |
| API server (operator watches) | operator samples + watches | intent vs. reality: restarts, OOM, limits, replicas, admission refusals, scheduling failures, node conditions, PVC/certificate/route status, Warning events | partly shipped; events storage and node/PVC signals are new |
| Application OTLP (optional) | node collector, `otlp` | in-app truth: spans, custom metrics | shipped, opt-in by nature |
| ClickHouse `system.*` | operator queries | the store's own disk, parts, ingest rate | partly used |
| Collector presence (derived) | store vs. API server | "node X has reported nothing for 10 minutes" — the silent-loss detector | new |

Two sources were evaluated and deliberately not used, with reasons in §3 and
§4: the KEDA interceptor (partial coverage by construction) and eBPF
auto-instrumentation (pre-1.0; the right *later* answer for east-west, the
wrong foundation today).

---

## 3. Where request-level data comes from

This is the decision the rest hangs off. The four golden signals demand
latency, traffic and errors per request path with nothing asked of the user —
and an uninstrumented application emits none of that. Something in the path
has to observe it.

### 3.1 The candidates

**(a) The shared Gateway's Envoy, read through Hubble — chosen.** Every
request to every Kitchen application crosses the shared Gateway, which is
Cilium's embedded Envoy. Cilium's proxy streams its access records to the
agent over a Unix socket, and the agent publishes them as Hubble **L7 flow
events**: for each HTTP transaction, a `RESPONSE` flow carrying
`l7.http.method`, `l7.http.url`, `l7.http.code`, `l7.http.protocol` and
`l7.latency_ns`, with source and destination endpoints attached. Hubble Relay
already aggregates these cluster-wide, and the operator already follows Relay
today — the existing `internal/flows` collector ingests exactly these
response flows for the traffic view, keeping status and latency and
discarding method and URL on the floor.

So the chosen design is almost embarrassingly small: **the vantage point is
already plumbed**. The follower stops discarding the two fields the golden
signals need, normalises the path, attributes the request to an environment,
and writes a request row. No new component, no new dependency, no data-path
change, zero added latency — the proxy hop already exists because it *is* the
ingress.

Coverage: every inbound HTTP request, for every environment, including
protected previews (the gate is behind the same Gateway) and scale-to-zero
environments (the interceptor is too — with the welcome property that a cold
start is *visible* as the tail latency of the first request, because Envoy
times the whole wait). cloudflared changes nothing: the tunnel points at the
Gateway, so the vantage is identical.

What it cannot see: east-west traffic (stays L4), anything non-HTTP, and
requests that die before Envoy. Accepted; §1 draws the line and §3.5 names
the successors.

**(b) Envoy access logs — rejected as the foundation, but the fallback is
cheaper than this section first claimed.** The original reasoning was that
Cilium exposes no supported way to configure access logs for Gateway
listeners, so reaching them meant injecting a `CiliumEnvoyConfig` over
objects Cilium's controller owns — patching around a controller, breaking on
every upgrade, and the same thing the project already rejected once for
`ext_authz` (SCOPE.md, preview protection).

That premise is false at the version Kitchen pins. Cilium 1.20 ships
`CiliumGatewayClassConfig`, whose `spec.telemetry.accessLogs` configures
access logging for every Gateway using that class through a
`GatewayClass`'s `parametersRef` — supported, declarative, no CEC and
nothing patched. Envoy writes them **to stdout**, which is the pipeline
Kitchen's node collector already reads, so the fallback costs a
`CiliumGatewayClassConfig` and a parser rather than a component.

It is still not the foundation, for a reason unaffected by any of that:
the Hubble path is *already ingested*. The follower holds the Relay stream,
the leader election and the host map today, and the fields the golden
signals need are ones it currently discards — so choosing it is deleting a
`continue`, while choosing access logs is a new format to parse, a new
failure mode when a log line is dropped, and traffic numbers that depend
on log retention. Access logs also arrive with no endpoint identity
attached, which is what §3.2's attribution would otherwise key on.

What this changes is §9's risk: if stage 0 finds the L7 flows are not
there, the fallback is a day's work on a supported API, not a fight with
another controller.

**(c) Hubble L7 visibility for pod-to-pod traffic — rejected as the
foundation.** Cilium emits L7 flows for pod traffic only when a
CiliumNetworkPolicy with L7 rules routes it through the proxy. That would add
a proxy hop to traffic that has none today, couple *observability* to
*policy* (an L7 rule also restricts), and still see nothing a sidecar-less
edge doesn't already see for north-south. Not zero-cost, not zero-risk, and
redundant for goal 1.

**(d) The KEDA HTTP interceptor — rejected.** It only sits in front of
environments that idle; production environments that never scale to zero
bypass it entirely. A vantage point that sees a *subset* of traffic cannot be
the source of truth for traffic numbers. (Its queue metrics remain
interesting for a cold-start signal later.)

**(e) eBPF auto-instrumentation (OpenTelemetry eBPF Instrumentation, née
Grafana Beyla) — rejected for now, on maturity.** OBI produces RED metrics
and basic spans per process with no code changes, including east-west and
gRPC — genuinely the strongest future answer for what the edge cannot see.
As of mid-2026 it is beta (v0.8.x), pre-1.0. Kitchen should revisit it when
it GAs; the schema's `source` column (`gateway` today) is where a second
vantage point lands without redesign.

**(f) Injected sidecars / language agents — rejected.** Operationally heavy,
per-language, and against the grain of "the best part is a part we do not
build."

### 3.2 Consequence: what the follower writes

The `internal/flows` follower (leader-elected, already connected to Relay)
gains a second output. For each L7 HTTP `RESPONSE` flow it derives:

- **host** — the authority from `l7.http.url`. This, not the destination
  endpoint, is what attribution keys on, because for a protected preview the
  destination is the gate and for an idling environment it is the interceptor;
  the Host header is the one thing every hop preserves (it is what the
  interceptor routes on).
- **project, environment** — from the operator's own routing knowledge: it
  published every hostname (generated URLs and verified custom domains), so it
  maintains the host → environment map the way it already maintains routes. A
  host it cannot attribute lands in an *unrouted* bucket — which is itself an
  operator signal (§7), catching stale DNS entries and scanners. The bucket is
  wider than that signal: the platform's own surfaces (the dashboard, the API,
  the identity provider) are published by routes that carry no project, since
  their traffic belongs to none, so they land there too. The signal subtracts
  the hostnames the routes publish before it calls anything unrouted; the
  bucket keeps them, because a count that quietly dropped them would disagree
  with what the edge served.
- **method** — canonicalised against the known verb set; anything else
  becomes `OTHER` (a cardinality guard against garbage requests).
- **path and route** — the URL's path with the query string stripped and never
  stored (privacy and cardinality in one move; recommending Hubble's own
  `redact.http.urlQuery` in the prerequisites is defence in depth), and the
  normalised route template beside it (§3.3).
- **status, duration_ms, protocol** — as today, plus the flow's HTTP protocol.

It also gains what it should always have had: **server-side filtering**.
Today the follower asks Relay for *every flow in the cluster* and discards
~90% client-side. `GetFlowsRequest` accepts whitelist filters (event types,
verdicts, TCP flags); requesting only L7 flows, drops and SYNs cuts the
stream to what is kept. And it starts counting its own gaps: Relay reports
`LostEvent` notices in-stream when Hubble's per-node ring buffer overflows or
the consumer lags — the follower records them, so the ingest-health signal
can say "request counts under-report; N events lost in the last hour" instead
of silently showing fewer requests than happened.

Raw rows are kept, not just aggregates, because goal 1 says *hand them
everything it knows*: "show me the failing requests" is a row listing, and
the crash-window view (§6.1) joins request rows to log lines by time. Rollups
answer the charts (§5). Measured cost makes raw rows cheap: ~10 bytes per
request compressed (verification record) — a sustained 100 req/s costs under
1 GB per week of raw retention.

### 3.3 Path cardinality

`/users/12345` is not a path; per-path metrics without templating are ruinous
(the rollup's ordering key would grow a row per user id) and useless (no two
requests group). Normalisation happens **in the follower, at ingest**, not in
SQL: it needs per-environment state (the cap below), and the store should
never see the unbounded set.

Two layers:

1. **Segment classification**, stateless, mirroring the log-pattern
   normaliser that already exists (`loganalytics.go`): a path segment that is
   purely numeric → `:id`; a UUID → `:uuid`; hex of ≥ 8 characters, ULID- and
   KSUID-shaped tokens, and long high-entropy tokens → `:hash`; a filename
   whose stem contains a content-hash infix (`app.8f3ab2c1.js`) → `*.js`.
   Depth is capped (12 segments, the rest folded into `/…`), segment length
   is capped, and the raw `path` column is truncated at 512 bytes.
2. **A per-environment route budget**, stateful: the follower keeps an LRU
   set of distinct templates per environment, capped at 300. A template past
   the cap is recorded as the overflow route `/…` rather than minting a new
   series. 300 templated routes is beyond any hand-written API; hitting the
   cap almost always means the classifier missed an identifier scheme, which
   the overflow row makes visible instead of letting it quietly poison the
   rollup.

The raw path is kept on the raw row (7-day TTL), so a mis-templated route is
still diagnosable; the template is what the rollups and screens group by.

### 3.4 Workloads the golden signals do not fit

An environment with no HTTPRoute — or one whose route has served nothing —
must not show four empty charts. The requests surface renders only when edge
traffic exists for the environment; otherwise the screen says plainly *"No
HTTP traffic reaches this environment through the platform's edge"* and leads
with what is real for every workload: log volume and error-line rate (a
liveness proxy for workers), CPU/memory against limits, restarts, and its
L4 flow edges (a queue worker's connection to its broker is visible traffic,
just not HTTP). When cron jobs and background workers become first-class
project shapes (SCOPE "nice-to-haves"), their native signals are
runs/completions/failures — facts the operator already watches Jobs for; the
signals model (§7) accommodates them without touching the collection layer.

gRPC via the Gateway is HTTP/2 underneath: requests appear with their
`POST /pkg.Service/Method` paths (templating leaves them alone — they are
already templates), but status is transport-level. The error column footnotes
that gRPC application errors are not counted — honest, until header capture is
verified (§9).

*Correction:* an earlier draft of this section had the route table **label**
protocol `h2`/gRPC. It cannot: `protocol` is a column of the raw row and of
nothing else — the rollups in §5 aggregate by host, route, method and status,
and adding protocol to their ordering key would multiply every environment's
series to buy one adjective. So the label was dropped and the footnote kept,
which is the half that carries the warning. It is driven from the request
rows, which do have the protocol, and it says something about the
*environment* — whether these numbers can be counted on for it at all —
rather than about whichever rows are on screen. Where this paragraph and §5
disagree, §5 wins: it is the schema, and API.md says the same from the API's
side.

### 3.5 The line this draws, restated

Chosen: the platform's ingress is the platform's tracer. It observes 100% of
what enters, costs nothing extra in the data path, requires nothing of the
user, and was already half-ingested. Not chosen: making every pod's traffic
L7-visible (policy coupling), a second proxy layer (cost without coverage),
or eBPF agents (maturity). The `source` column and the honest east-west gap
are where the next vantage point lands when one earns its place.

---

## 4. Collection architecture

What runs where, after this design — **bold** marks what changes:

- **Per node: the OpenTelemetry Collector DaemonSet** (contrib, pinned tag) —
  unchanged pipelines for logs, kubelet/host metrics and OTLP, with one
  addition: **the `volume` metric group on `kubelet_stats`**, which is where
  per-PVC usage comes from. Off-the-shelf, configured not built. It remains
  the only door into the store for logs/metrics/traces.
- **In the operator (leader-elected singletons, all existing runnables):**
  - **the Hubble follower**, extended per §3.2 — request rows, server-side
    filters, loss accounting. This is the one place the design builds
    something, and the case is: no off-the-shelf Hubble → ClickHouse request
    pipeline exists (the collector has no Hubble receiver; Isovalent's
    `hubble-otel` experiment is unmaintained), and attribution needs the
    host → environment map only the operator has. ~Hundreds of lines beside
    code that already speaks both ends.
  - the usage sampler — unchanged (restarts, OOM kills, limits, replicas over
    OTLP; the restart-differencing argument in SCOPE.md stands).
  - **a Warning-event recorder** — a watch on `corev1.Event` (Warning only),
    deduplicated by count, attributed to project/environment via the involved
    object's namespace and labels, written to a new `k8s_events` table. The
    operator already reads these events one at a time for the component
    survey; this makes them a queryable history ("what happened at 03:00")
    instead of an hour-lived mystery. Built rather than deployed because the
    alternative — a second, Deployment-mode collector running `k8s_objects` —
    is a new workload whose output would still need reshaping, while the
    operator is an existing watcher with the attribution knowledge. The
    activity feed (`events` table) is untouched; that is the platform's
    story, this is the cluster's.
  - **the signals evaluator** (§7) — pure functions over the operator's
    informer caches plus a few store queries; no background loop yet.
- **ClickHouse** — single node, operator-owned schema, as today, plus the new
  tables in §5.
- **Explicitly not run:** Prometheus or any second metrics stack;
  kube-state-metrics (the operator watches the API server itself); a
  `k8s_cluster`-receiver collector (same reason); per-pod L7 proxying.

On "treat the current implementation as neither foundation nor constraint":
it was evaluated piece by piece against these goals, and most of it survives
on merit — the collector DaemonSet, the label→column enrichment, the
operator-owned DDL, log analytics, the OTLP hand-off to apps. What did not
survive: the follower's discard of method/URL, the flows-only notion of
"traffic" (its numbers misattribute protected previews to the gate and
idling environments to the interceptor — the request pipeline's host
attribution fixes both, and `/metrics/overview` switches source accordingly),
the unfiltered Relay subscription, and the absence of any ingest
self-observation. Nothing is deleted wholesale because nothing deserved it.

---

## 5. Store schema

Existing tables are untouched: `otel_logs`, `otel_traces` (+ id lookup),
the five `otel_metrics_*`, `metrics_5m`, `flows`, `events`. The stock
exporter keeps writing the OTel-shaped tables with `create_schema: false`;
the operator keeps every table's DDL and TTL. New tables follow the house
ordering-key rule — every product query is project-scoped, so keys lead
`(project, environment, …)`.

**`http_requests`** — one row per edge-observed request. Verified DDL
(ClickHouse 25.8; see verification record):

```sql
CREATE TABLE http_requests
(
    Timestamp     DateTime64(9) CODEC(Delta(8), ZSTD(1)),
    project       LowCardinality(String),
    environment   LowCardinality(String),
    host          LowCardinality(String),
    method        LowCardinality(String),
    path          String CODEC(ZSTD(1)),
    route         LowCardinality(String),
    status        UInt16,
    duration_ms   Float64 CODEC(ZSTD(1)),
    protocol      LowCardinality(String),
    source        LowCardinality(String),   -- 'gateway'; future vantage points get their own value
    trace_id      String CODEC(ZSTD(1))     -- the id the request carried, where it carried one (§1)
)
ENGINE = MergeTree
PARTITION BY toDate(Timestamp)
ORDER BY (project, environment, Timestamp)
TTL toDateTime(Timestamp) + toIntervalDay(7)
SETTINGS index_granularity = 8192, ttl_only_drop_parts = 1
```

The operator writes it the way it writes `flows` (JSONEachRow inserts,
batched; batch sizing raised to request volumes). `LowCardinality(route)`
is safe *because* §3.3 bounds the set.

**`http_requests_1m`** and **`http_requests_1h`** — the golden-signal query
targets, `AggregatingMergeTree` fed by two materialized views over the raw
inserts (deliberately both from the raw table; chaining MVs off aggregate
states is where the alias-shadowing class of bug lives):

```sql
CREATE TABLE http_requests_1m
(
    bucket        DateTime,
    project       LowCardinality(String),
    environment   LowCardinality(String),
    host          LowCardinality(String),
    route         LowCardinality(String),
    method        LowCardinality(String),
    status        UInt16,
    requests      AggregateFunction(count),
    duration      AggregateFunction(quantilesTDigest(0.5, 0.95, 0.99), Float64)
)
ENGINE = AggregatingMergeTree
PARTITION BY toDate(bucket)
ORDER BY (project, environment, bucket, host, route, method, status)
TTL bucket + toIntervalDay(30)          -- retentionDays
```

`http_requests_1h` is the same shape, `toStartOfHour`, partitioned by month,
retained 12 × retentionDays for year-scale views. The reading side is
`countMerge` / `countMergeIf(status >= 500)` / `quantilesTDigestMerge` —
all verified against a live server, including per-route p95 and error-rate
in one query over synthetic load.

Retention deliberately stays **one knob** (`retentionDays`, the existing
spec field): raw requests at `min(7, retentionDays)` days, the 1m rollup at
`retentionDays`, the 1h rollup at `12 × retentionDays`. Derived, not
configurable — a second knob is a second thing to explain, and these ratios
are not the kind of thing two installations need to disagree about.

**`k8s_events`** — the cluster's Warning events, written by the operator:

```sql
CREATE TABLE k8s_events
(
    timestamp   DateTime64(3, 'UTC'),
    project     LowCardinality(String),   -- '' for platform/cluster objects
    environment LowCardinality(String),
    namespace   LowCardinality(String),
    kind        LowCardinality(String),
    name        String,
    reason      LowCardinality(String),
    message     String,
    count       UInt32,
    node        LowCardinality(String)
)
ENGINE = MergeTree
PARTITION BY toDate(timestamp)
ORDER BY (project, environment, timestamp)
TTL toDateTime(timestamp) + toIntervalDay(30)
```

**`signal_transitions`** — *not created yet.* When background evaluation
lands, open/resolve transitions of §7's findings persist here (timestamp,
signal id, fingerprint, state, severity, scope, title, detail); the inbox
reads it and delivery routes from it. It is named now so the finding shape
is designed against it; creating it costs one `ensureTableTTL` call later.

Migration honesty: the schema mechanism is `CREATE TABLE IF NOT EXISTS` plus
TTL reconciliation — it never reshapes an existing table. Every table above
is new, so this design needs no migration machinery; the reserved `trace_id`
column exists precisely to avoid needing one later. (Known, pre-existing
debt worth recording: `events`' ordering key `(timestamp)` does not serve
its own per-project read pattern; not worth a migration alone, worth folding
into any future one.)

---

## 6. API and dashboard surfaces

The dashboard already scopes by project and has an operator mode; today that
mode is a client-side toggle and the API enforces authentication but not
authorization (an open item in AUTH.md). The surfaces below are designed to
the *intended* boundary — project-scoped screens read project-scoped
endpoints — so when RBAC lands, enforcement is a middleware, not a redesign.

### 6.1 For the application developer (project-scoped)

**Environment page additions** (it stays one page; these are new sections and
a new header):

- **Golden-signal header**: four tiles with sparklines — requests/s, error
  rate, p95, and saturation (peak CPU and memory as % of limit) over the
  selected window. Rendered from one call, `GET
  /api/v1/environments/{name}/requests/summary` + the existing metrics
  endpoint.
- **Requests section** — the centrepiece:
  - Charts: traffic, error rate, latency p50/p95/p99 over time
    (`…/requests/series`), with deploy marks from the activity feed.
  - **Route table** (`…/requests/routes`): one row per route template —
    requests, error %, p50/p95/p99, sortable, window-scoped. Clicking a route
    filters the charts and the request list to it. This is the per-path
    breakdown the goals ask for, and it works because §3.3 made routes finite.
  - **Request list** (`…/requests`, filterable by route/status class/method,
    SSE live tail exactly like logs): recent requests with time, method,
    path (raw), status, duration. A failing request expands to the
    correlated view: log lines from the same environment in a ±30s window
    (one click pre-fills the existing log query), and — when the app is
    instrumented — its trace link via `trace_id`.
- **Diagnostics strip**, at the top of the page when any environment-scoped
  signal (§7) is firing: *"2 problems: crash-looping (12 restarts in 30m),
  memory at 96% of limit"* — each linking to its evidence. Backed by
  `GET /api/v1/environments/{name}/signals`.
- **Crash report** (`GET /api/v1/environments/{name}/diagnostics`): when a
  container has terminated abnormally, one assembled view — exit code and
  reason (OOMKilled vs. crash), restart trajectory, **the last log lines
  before the termination instant**, the memory series leading up to it, the
  related Warning events, and edge requests in the failure window. This is
  "troubleshooting is reading rather than hunting" made concrete: everything
  the platform knows about the crash, on one screen, assembled by the API
  rather than by the person.
- Non-HTTP environments: §3.4's honest degrade.

**Project page**: per-environment mini-cards (sparkline, error %, p95,
health), so a project owner sees all previews and production at a glance.
`/metrics/overview`'s per-project traffic numbers switch source to
`http_requests_1m` (correcting the gate/interceptor misattribution).

The existing `/observability` log analytics screen stays the log surface —
search bar, histogram, facets, patterns, saved queries — and gains nothing
but a route from request rows into it (the correlated-logs click above).

### 6.2 For the operator (platform-scoped, operator mode)

A new **Platform** section of the sidebar, visible in operator mode:

- **Overview** — the platform's front page: a health strip (nodes N/N,
  components M/M, ingest, store, edge, certificates, builds — each green or
  naming its problem) and the **problems list**: every currently-firing
  signal from §7, ordered by severity, each with its evidence link. This
  screen *is* the future inbox, minus persistence: today it evaluates on
  view (`GET /api/v1/platform/signals`), later it reads
  `signal_transitions`. Same screen, same data shape.
- **Nodes** — per node: conditions (Ready, pressure), CPU/load/memory/disk
  series, filesystem fill with a "full in ~N days" projection, pod count,
  and **telemetry freshness** — when the store last received anything from
  this node's collector. A node whose collector is dead or was never
  admitted looks *clean* everywhere else; this column is where it looks
  broken.
- **Workloads** — every pod on the platform (applications and platform
  components alike): restarts, OOM kills, phase, pending-with-reason,
  admission refusals surfaced from Warning events. The component survey's
  trick — "no pods at all is the worst symptom" — applied cluster-wide.
- **Edge** — cross-project traffic: platform requests/s and error rate, top
  routes and hosts by traffic and by error rate, latency leaders, the
  unrouted-host bucket, Gateway/listener status, cloudflared health, and the
  certificate table (wildcard + per-domain: issuer state, renewal state,
  days to expiry, last ACME error verbatim).
- **Storage** — PVCs with usage (from the new volume stats) and their
  workloads; the store's own health: disk used vs. PVC size, parts, ingest
  rows/s, per-table bytes (already computed today), TTL horizon; flow-stream
  loss counters (§3.2).
- **Events** — the `k8s_events` explorer: window, facets on
  reason/kind/namespace/node, full-text over messages, deep links from every
  other screen ("show me this pod's events").
- **Builds** — the existing builds list plus queue state: waiting builds and
  wait times against the concurrency limit.

`GET /api/v1/status` grows into `GET /api/v1/platform/*` endpoints backing
these (nodes, workloads, edge, storage, events, signals, ingest), all
reading informer caches and the store — no new watch on the request path.

---

## 7. The operator's signal catalogue

The model first, because it is what makes "screens now, alerting later" a
promise instead of a hope:

- A **signal** is a named, versioned rule in Go: `Evaluate(snapshot) →
  []Finding`, where the snapshot is the operator's informer caches plus
  store queries. Pure — no side effects, no state.
- A **finding** is `{signal id, severity, scope (platform | project/env |
  node | domain | …), fingerprint, title, detail, since, evidence link}`.
  The **fingerprint** is stable for the same underlying condition across
  evaluations (e.g. `workload.crashloop/shop/pr-41/web`), which is what
  makes findings diffable.
- Today: evaluated on request when a screen asks; findings are ephemeral.
- Later, with **zero change to the above**: a background loop evaluates the
  same catalogue on an interval, diffs fingerprints against the previous
  round, writes open/resolve transitions to `signal_transitions`, and a
  delivery layer routes transitions to Slack/email. Detection is this
  design; only the loop, the table write and the routing are new work.

The catalogue, v1 — each row names what it is computed from. Audience "dev"
means it also surfaces on the environment's diagnostics strip.

**Workloads** (dev + operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `workload.crashloop` | container in CrashLoopBackOff, or restart delta over threshold in window | pod status (API) + `metrics_5m.restarted` |
| `workload.oomkilled` | OOM kills in window | `metrics_5m.oomKills` |
| `workload.near-memory-limit` | working set ≥ 90% of limit sustained | `metrics_5m` vs. limit gauge — *the OOM kill, before it happens* |
| `workload.at-cpu-limit` | usage pinned at limit sustained (throttling proxy) | `metrics_5m` vs. limit gauge |
| `workload.imagepull` | ImagePullBackOff / ErrImagePull | pod status (API) |
| `workload.unschedulable` | pod Pending with PodScheduled=False, with the scheduler's reason | pod conditions (API) |
| `workload.admission-refused` | workload wants pods, has none, FailedCreate Warning names why | workloads + `k8s_events` — the survey's PodSecurity lesson, applied to app namespaces |
| `workload.notready` | available < desired beyond grace period | workload status (API) |
| `env.error-rate` | 5xx ratio over threshold vs. trailing baseline | `http_requests_1m` |
| `env.latency-regressed` | p95 sustained above trailing baseline | `http_requests_1m` |
| `env.traffic-vanished` | requests/s ≈ 0 where the trailing window had traffic | `http_requests_1m` |
| `env.no-backend` | edge serving 502/503 for an environment with zero ready pods | `http_requests_1m` + workload status |

**Nodes and capacity** (operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `node.notready` | NodeReady false | node conditions (API) |
| `node.pressure` | Memory/Disk/PID pressure condition | node conditions (API) |
| `node.saturated` | CPU or memory ≥ 90% sustained | `host_metrics` |
| `node.disk-filling` | filesystem projected full within N days (linear fit) | `host_metrics` filesystem series |
| `node.silent` | node exists in the API but no telemetry rows for 10m | store freshness vs. node list — catches dead collectors *and* the namespace-PodSecurity failure, which is otherwise invisible |
| `cluster.overcommitted` | scheduled requests exceed allocatable minus one node | node allocatable + pod requests (API) — "the next node loss cannot be rescheduled" |

**Storage** (operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `pvc.pending` | PVC unbound — message names the default-StorageClass suspect, the classic first-install hang | PVC status (API) |
| `pvc.filling` | volume ≥ 85% used | `kubelet_stats` volume group (new) |
| `volume.attach-failed` | FailedAttachVolume / FailedMount warnings | `k8s_events` — the CSI-misbehaving detector |
| `store.disk` | ClickHouse data volume past threshold | `system.parts` + volume stats |
| `store.ingest-stalled` | newest row in `otel_logs` older than N minutes while pods run | store |
| `ingest.flows-lost` | Relay reported lost events / follower reconnects with gaps | follower's own accounting (§3.2) |

**Edge and certificates** (operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `gateway.unprogrammed` | Gateway Programmed=False (AddressNotAssigned et al.) | Gateway status (API; today a condition, now with evidence) |
| `route.rejected` | any HTTPRoute unaccepted / refs unresolved | route status (API) |
| `dns.mismatch` | a sampled `*.baseDomain` name does not resolve to the Gateway address | active resolution by the operator — cheap, and catches the "everything green, nothing reachable" install |
| `cert.expiring` | certificate inside 21 days with renewal not progressing; ACME order error attached verbatim | cert-manager objects (unstructured, as the operator already addresses them) |
| `tunnel.down` | cloudflared unavailable or flapping | Deployment status + restarts |
| `edge.unrouted-hosts` | sustained requests for hosts no HTTPRoute publishes | `http_requests` unrouted bucket, minus every hostname the routes publish — the platform's own surfaces are unattributed, not unpublished |

**Builds** (dev + operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `build.queue-backed-up` | builds queued longer than N × median build time | Build CRs + activity feed |
| `build.pod-pending` | build job pod unschedulable | job/pod status (API) |
| `build.failing-repeatedly` | N consecutive failures in one project | Build CRs |

**Cross-project — the platform-cause detectors** (operator)

| Signal | Fires when | Computed from |
|---|---|---|
| `platform.latency-correlated` | p95 rising in ≥ 3 projects simultaneously | `http_requests_1m` across projects — several projects degrading together is a platform problem wearing project costumes; evidence links to node saturation and edge status |
| `platform.error-correlated` | 5xx rising in ≥ 3 projects simultaneously | same |
| `platform.component-unhealthy` | the existing component survey, folded into the same feed | Kitchen status |

Thresholds are constants with taste, not user configuration, in v1 —
configurable thresholds are an alerting-era feature and the catalogue is
versioned code either way.

---

## 8. Staged delivery

Each stage ships something usable alone; later stages never rework earlier
ones. (This staging exists to decompose implementation into independent
work packages, not as a multi-quarter roadmap.)

- **Stage 0 — prove the vantage point.** A CI job that installs Cilium (the
  pinned `CILIUM_VERSION`, `gatewayAPI.enabled=true`, Hubble + Relay) in
  kind, routes a request through a Gateway, and asserts an L7 `RESPONSE`
  flow with method, URL, status and non-zero `latency_ns` arrives at Relay
  with no CiliumNetworkPolicy present. This closes the one unverified fact
  (verification record) — and gives Kitchen what it currently lacks anyway:
  CI that exercises the flow pipeline at all. Also: measure and document
  `hubble.eventBufferCapacity` guidance for the prerequisites.
- **Stage 1 — the request pipeline.** `http_requests` (+ rollups) DDL in the
  schema package; follower extension (filters, templating, host attribution,
  loss accounting); `/metrics/overview` switched to it. Ships: correct
  platform traffic numbers, queryable raw requests.
- **Stage 2 — the developer surface.** The requests endpoints and the
  environment page: golden header, charts, route table, request list with
  live tail, correlated-logs click-through. Ships: goal 1's core screen.
- **Stage 3 — reading instead of hunting.** `k8s_events` table + recorder;
  volume metric group; diagnostics + crash report endpoints and UI;
  environment signals strip. Ships: the crash-forensics experience.
- **Stage 4 — the operator estate.** Signals package + catalogue v1;
  platform endpoints; Overview, Nodes, Workloads, Edge, Storage, Events
  screens. Ships: goal 2's screens-and-derived-signals scope, complete.
- **Stage 5 (out of current scope, designed for) — detection and delivery.**
  Background evaluation loop, `signal_transitions`, inbox, routing.

Stages 1–4 decompose further along package seams (schema / follower / API /
UI per stage), which is how the work parallelises across implementers.

---

## 9. Open questions, and trade-offs deliberately taken

Settled by choosing a side, with the reason:

- **Raw request rows vs. aggregates only** — raw, 7 days. The failing
  request itself is the product ("hand them everything"); measured at ~10
  B/row it is the cheapest table in the store.
- **Normalise paths in Go vs. in SQL** — Go. The route budget is stateful
  per environment; the store must never see the unbounded set.
- **Extend the follower vs. a new component** — extend. It already holds the
  Relay connection, the leader election, and sits beside the host map.
- **`http_requests` vs. widening `flows`** — a new table. Flows model edges
  for the service map; requests model the product surface. Their keys,
  attributions and retentions differ; forcing one shape onto both is how
  the gate-misattribution bug happened.
- **One retention knob with derived ratios vs. per-kind knobs** — one knob,
  per the schema package's own precedent.
- **Kubernetes events via the operator vs. a second collector** — operator
  (§4). The activity feed stays separate and untouched.
- **Signals as versioned code vs. user-configurable rules** — code, until
  the alerting era forces the question with real requirements.

Flagged rather than guessed. **Stage 0's job has since run, and answered all
five** — the readings are in the verification record; each is summarised here
beside the question it settles, because the question is why the answer matters:

- **Does the Gateway's Envoy emit L7 flows with populated latency on a
  stock cluster?** The design's one load-bearing unverified fact.
  **Answered: yes**, with method, URL, status and non-zero latency, against a
  cluster carrying no CiliumNetworkPolicy at all. `CiliumGatewayClassConfig`'s
  access logs stay the named fallback (§3.1b) and are not needed.
- **gRPC status and header capture** — whether Hubble's header list can
  carry `grpc-status` without unacceptable cardinality/privacy cost.
  **Half answered**: `Grpc-Status` is obtainable, so the remaining question is
  the cost of keeping headers, which is a decision rather than an unknown.
  gRPC error honesty stays a screen footnote (§3.4) until it is taken.
- **Trace context at the edge** — whether Cilium populates `trace_context`
  from an incoming `traceparent`. **Answered: yes**, the id the request
  carried arrives on the flow, so `trace_id` is filled at the edge rather
  than only by joining spans. The reserved column needed no migration, which
  is what reserving it was for.
- **Requests Envoy answers itself** (upstream timeout → 504, no backend →
  503). **Answered**: they are recorded like any other response — a dead
  backend produced a real 503 flow — so `env.no-backend` reads off request
  rows rather than needing the upstream-facing half.
- **Hubble loss under sustained high rate.** **Measured**: the 4095-event
  default buffer held about ten minutes of history at 6.6 flows/s, and the
  window shrinks in proportion to traffic. The accounting (§3.2) makes loss
  visible when it happens; raising `hubble.eventBufferCapacity` before
  driving real load is the cheaper move, and the chart README says so.

---

## Appendix — verification record

Established empirically during this design, against ClickHouse **25.8.30.16**
in Docker:

- The full §5 DDL applies cleanly: `http_requests`, both rollup tables, both
  materialized views, TTLs included.
- The read side works as designed: `countMerge`, `countMergeIf(status ≥
  500)`, and `quantilesTDigestMerge` over the 1m rollup return per-route
  traffic, error-rate and p95 in one query.
- Cost measurements over 500 000 synthetic request rows: raw table 4.63 MiB
  compressed (≈ 9.7 bytes/row, 3.8× compression), 1m rollup 484 rows /
  714 KiB, 1h rollup 12 rows. The basis for §3.2's cost claim.

Established during **implementation**, and worth recording because both
corrected an assumption this document made. Each was found by running the
pinned collector image against a real ClickHouse and reading the tables,
rather than by reading its documentation:

- **The utilisation metrics do not exist by default.**
  `system.cpu.utilization`, `system.memory.utilization` and
  `system.filesystem.utilization` are all disabled in the pinned contrib
  build. A reader that asks for them finds nothing — and `node.saturated`
  would have reported healthy nodes forever. Utilisation is derived instead:
  CPU from the idle fraction of `system.cpu.time` across the bucket, memory
  from `used` over the sum of the states, fill from `used/(used+free)`.
- **`system.filesystem.usage` has a third state, `reserved`**, and it is
  large: on the measurement host, 16.4 GB used, 23.3 GB free and 230 GB
  reserved. Counting it as capacity renders a disk that is 41% full as 7%
  full — the one direction of error `node.disk-filling` cannot afford. It is
  excluded, so capacity means what a writer can actually use.
- Two smaller ones: there is no `k8s.volume.used` metric at all (naming it
  makes the collector refuse to start, so used is `capacity − available`),
  and a claim is told apart from the projected-token mounts every pod carries
  by its *claim name* attribute — the volume-type attribute comes back empty
  for both.

Also established in implementation, against ClickHouse **26.3**: the rollup
reads answer only windows snapped to the rollup's own resolution (an
unsnapped hourly read of a window starting at 12:34 matches no bucket and
reports real traffic as zero), and `quantilesTDigestMerge` over an empty
window returns `nan`, which parses and then makes `json.Marshal` refuse the
response.

Established from documentation, source and proto definitions (not
re-verified live): the Hubble flow proto's `Layer7{type, latency_ns,
HTTP{code, method, url, protocol, headers}}` shape and `LostEvent`
reporting (cilium/cilium `api/v1/flow`, the version the operator already
vendors — the existing follower ingests these fields' subset today);
Cilium's agent↔Envoy access-log socket and L7-visibility mechanism (Cilium
Envoy/observability docs); Hubble's default event buffer (4095) and
`redact.http.urlQuery` option; OBI/Beyla's pre-1.0 status.

**Settled by stage 0's CI job**, on kind with the pinned Cilium, Hubble and
Hubble Relay, and asserted against a cluster the job checks is carrying no
CiliumNetworkPolicy, no clusterwide policy and no NetworkPolicy:

- L7 `RESPONSE` flows arrive for Gateway traffic with method, URL, status and
  non-zero latency. This is the fact the whole design rests on.
- `trace_context.parent.trace_id` is the id the request carried
  (`4bf92f35…4736` sent, the same read back), so `trace_id` is filled at the
  edge.
- Envoy's self-generated answers are ordinary responses: a route with a dead
  backend produced a `503` at 112µs.
- Headers are populated and include `Grpc-Status`, so capturing it is a cost
  decision rather than an open capability question.
- The 4095-event ring buffer held 868 flows at 6.6 flows/s — about ten
  minutes of history, shrinking in proportion to traffic. No lost-event
  notices in the window.

What follows is why that job did not exist when this was written, and stands
as the record of the gap it closed. Not verifiable in the design-time sandbox:
that Gateway API traffic emits L7 HTTP flows *without any CiliumNetworkPolicy*. Kitchen's CI
never installs Cilium (the kind job's Gateway is deliberately never
programmed), so there was no existing harness; a from-scratch kind + Cilium
cluster could not run in the design-time sandbox (its kernel exposes only
cgroup v1 to containers, which current kind node images and Cilium's
kube-proxy replacement both refuse). The experiment script exists and is the
seed of stage 0's CI job. Circumstantial support meanwhile: Cilium documents
L7 flow records for all proxied traffic and assigns Gateway traffic the
`ingress` identity through that same proxy, and the existing traffic view's
HTTP edges — status and latency from these exact flows — are the feature
SCOPE.md records as shipped and working.
