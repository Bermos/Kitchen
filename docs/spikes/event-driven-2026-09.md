# Spike — event-driven applications: one substrate for every environment, and the library in front of it

*September 2026. A design spike: no platform code changed, and nothing here is
built. It exists to decide the shape of one feature — a developer writes an
event-driven application against a library, and the platform provides the
infrastructure behind it for every project and every environment that asks —
and to record what the survey of the field turned up, so the decision is made
on evidence rather than on which product's landing page was read last.*

The brief, as given: first-class, developer-experience-focused event-driven
architecture and infrastructure. A library the teams build against; the
platform provisions the infrastructure per application and per environment on
request; **not** the shape the self-hosted Inngest claim ended up with, where
every environment needs a deployment of its own with a Postgres and a Redis
behind it, but something with tenancy or namespacing the platform can abstract
away; and the same monitoring and eventing every other workload on the
platform gets.

The conclusion, before the working:

- **The ask is three things wearing one coat**, and they want different
  machinery: a *transport* (topics, queues, fan-out, retries, a dead-letter
  path, lag), *durable execution* (steps that are memoised, sleeps, waiting
  on an event), and *triggers* (a schedule, a queue, a person — the three
  [SCOPE.md](../SCOPE.md) already names against one execution primitive).
  Every product surveyed bundles some subset and calls it "events". Deciding
  which of the three is being built first is most of the decision.
- **The substrate should be one shared NATS JetStream, with one NATS account
  per environment.** An account is a separate subject space and a separate
  JetStream store — structural isolation rather than a permission check — so a
  preview's `orders.placed` and production's never meet, with no prefixing
  discipline for anybody to forget. Accounts are metadata; the cost of a
  preview is a JWT and a stream, not a pod. One small StatefulSet serves
  hundreds of environments. It is a single Go binary, Apache-2.0, CNCF, and
  the reference client is Go.
- **The tenancy the broker leaves to its operator is exactly the tenancy
  Kitchen already does for everything else.** The NATS operator (NACK)
  reconciles streams and consumers but mints no accounts and no credentials;
  the Kitchen reconciler does, the way it already mints registry credentials
  and Inngest keys, and writes them into a `managed-by: kitchen` Secret that
  the API never reads back.
- **Durable execution should not pick the substrate.** Only three engines give
  real in-product tenancy in one deployment — Temporal (namespaces), Hatchet
  (tenants), Cadence (domains) — and each is a control plane of its own to
  operate. The cheapest credible durable-step story in Go is embedded and
  Postgres-backed (the DBOS and River pattern), against the database a
  `postgres` claim already provisions per environment. So: transport first,
  steps as a library concern on top, and the engine question kept open until
  the transport is in use.
- **The library is thin on purpose, and it owns the observability layer.**
  Package-level declarations of topics, subscriptions and schedules; a
  CloudEvents envelope; OpenTelemetry producer and consumer spans joined by
  span links, with the `messaging.*` attributes — which the Go NATS ecosystem
  does not otherwise provide. The platform's part is the binding — a URL and a
  credential per environment, and nothing else — so the application never
  learns which broker it is on, and the broker can change without the
  application changing.
- **Previews are where every competitor is weakest**, and where accounts make
  Kitchen strong by construction: Cloudflare documents that a preview publishes
  into the production queue, Encore skips cron in previews, Vercel partitions
  topics by deployment id and lives with the retry tail that leaves. An account
  per environment gives Vercel's isolation without Vercel's tail.
- **The monitoring bar is lag and retry depth, not throughput.** Vercel's
  *Max Message Age* and *Retry Depth* per consumer, with a preset alert, is
  the screen to match. Everything on it is one read of JetStream's consumer
  info per account, which is a fact about the environment and belongs on the
  environment's page — and a failed event is a row somebody can replay.

## What Kitchen has, and what the Inngest claim taught

The pieces this feature stands on already exist, and one of them is the
cautionary tale.

- **Project → Environment → Release**, and a project's workloads beyond the web
  process — `worker`, `service`, `cron`, `task`
  ([processes](../api/processes.md)). A worker is a Deployment with no Service
  and no route; a preview runs one only when it opts in. The list is frozen
  into every Release so a rollback runs what that release declared.
- **Claims** ([claims](../api/claims.md)): `postgres`, `redis`, `objectStore`,
  `volume`, `oidcClient`, `service`, `inngest`. The contract has not moved
  since the first — a Connection matched on a capability, a provisioner built
  from it, typed requirements off the claim's own slice of `spec.config`, a
  binding Secret in the application namespace, and a declared answer to what
  a preview gets (`fresh`, `branch`, `shared`, or nothing with a reason).
- **`kitchen.json`** ([CONFIG.md](../CONFIG.md)) travels with the commit and
  is read at the commit the platform builds: build, runtime, env, processes,
  files, volumes, offers. A pull request that adds a worker adds the worker's
  declaration in the same diff.
- **Every pod is instrumented for free**: `OTEL_EXPORTER_OTLP_ENDPOINT` and
  `OTEL_RESOURCE_ATTRIBUTES` carrying `kitchen.project` and
  `kitchen.environment`
  ([environment_controller.go:1055](../../internal/controller/environment_controller.go)),
  spans in ClickHouse's `otel_traces`, the signal catalogue and the activity
  feed on top ([OBSERVABILITY.md](../OBSERVABILITY.md)).
- **KEDA is already installed by the operator** for scale to zero, and KEDA
  ships a JetStream scaler that reads a consumer's pending count per account.
- **The `redis` claim already has `usage: queue`** — a Valkey per claim that
  refuses writes rather than evicting them.

And the tale. The `inngest` claim shipped against Inngest Cloud first (#268),
whose *branch environments* are a real preview story. Self-hosting was the
follow-up (#394), and it ran into the fact #268 had named: **a self-hosted
Inngest server is one environment.** It takes one signing key, `INNGEST_ENV`
selects nothing, the dashboard and its GraphQL API are unauthenticated by
design, and two previews registering functions on the same event name into one
server trigger each other's functions — "app naming namespaces the apps; it
does not namespace the event stream". The only honest isolation was **a
server per environment**: for production a Deployment plus a CloudNativePG
Cluster plus a Valkey, for every preview a pod with an embedded SQLite and
in-memory Redis on a PersistentVolumeClaim of its own
([claims.md](../api/claims.md), *Storage is two shapes*). That is the shape
the brief rejects, and rightly: the isolation was bought with pods because the
product had no cheaper unit to buy it with. **The lesson is not "Inngest bad";
it is that the tenancy unit of the substrate decides the cost of an
environment.** Pick a substrate whose tenancy unit is metadata.

Two more things the platform already says that this feature has to honour:

- SCOPE.md's runbook decision states the execution model as *one primitive,
  three triggers: a schedule, a queue, and a person*. The schedule and the
  person exist. **The queue trigger has no primitive** — a worker is a loop the
  application supervises itself.
- #489 (open) is the spike on offerings between projects: "the gap is an edge
  between projects, not a box around them". A topic one project publishes and
  another subscribes to is that edge for asynchronous calls, and it should
  arrive through the same `offers` / `service` claim shape rather than a
  second one.

## What the ask decomposes into

Every product in this space bundles some of the following and calls the bundle
"events" or "background jobs". They are separable, and separating them is what
makes the options below comparable.

| Concern | What it means concretely | Who provides it |
|---|---|---|
| **Transport** | A named topic; publish; a named subscription with at-least-once delivery, a consumer group, an ack deadline, a retry policy and a maximum delivery count; what happens to a message that exhausts them; how far behind a consumer is | the broker, and the platform's configuration of it |
| **Durable execution** | A function whose steps are memoised so a crash resumes rather than restarts; `sleep` for a day without holding a pod; wait for a named event; fan-out and gather | an engine (Temporal, Hatchet, Inngest, Restate) *or* a library over a database (DBOS, River, go-workflows) |
| **Triggers** | A schedule; a message on a queue; a person pressing a button; a deploy | the platform — three of four already exist as process types |
| **Tenancy** | A preview's messages never reach production's consumers; a project's never reach another's unless offered; quotas so a preview cannot starve production | the substrate's tenancy unit, and what it costs |
| **Observability** | Lag and retry depth per subscription; a list of failed messages with replay; a trace that crosses the queue; a map of who publishes what to whom | the library (spans), the operator (broker facts), the dashboard |

The brief asks for all five. The order they are built in is the decision, and
the recommendation below is transport, tenancy and observability first, with
triggers extended (a queue joins the schedule) and durable execution as a
second phase that does not have to change anything the first shipped.

## The substrate — what can multiplex hundreds of environments

The survey, condensed. Versions and licences are as of September 2026;
footprints marked *est.* are estimates rather than published figures.

| | Tenancy unit | How strong | Who creates a tenant | Small HA footprint | Go client | Redelivery, dead letters | Licence | KEDA scaler |
|---|---|---|---|---|---|---|---|---|
| **NATS JetStream 2.14** | *account*: own subject space, own JetStream store, per-account limits (connections, memory, file bytes, streams, consumers) | structural — a subject never crosses accounts without an explicit export/import | the platform: JWTs pushed to the `full` resolver at runtime, or auth callout; NACK's `Account` CRD does **not** create one | 3 × 0.5–2 CPU, 1–8 GiB; one Go binary; 3 × 0.5 CPU / 1 GiB is comfortable at low traffic *(est.)* | `nats.go` — the reference client; the server is Go | `MaxDeliver` + `BackOff`; **no built-in DLQ** — a `MAX_DELIVERIES` advisory the platform turns into one | Apache-2.0, CNCF (the 2025 BSL dispute settled in May 2025, trademarks to the Linux Foundation) | yes (`/jsz`, per account) |
| **Kafka 4.3 / Strimzi 1.2** | prefixed ACLs on topics, groups, transactional ids | a naming convention with byte and request quotas; **no storage quota** per tenant | Strimzi `KafkaTopic` / `KafkaUser` in the platform namespace | 3–6 JVM pods, 6–12 GiB *(est.)*; cost grows per partition × environment | `franz-go`, excellent, share groups included | retry/DLT topics by convention; share groups (KIP-932, GA in 4.2) add delivery counts | Apache-2.0 | yes |
| **Redpanda 26.x** | as Kafka | as Kafka; RBAC and OIDC need an enterprise key | Redpanda operator `Topic` / `User` | ≥ 2 cores and ≥ 4.5 GiB per broker | `franz-go` | as Kafka | **BSL — forbids offering it as a service to others** | yes |
| **RabbitMQ 4.3** | *vhost*: a real namespace with per-vhost connection and queue limits | strong | Messaging Topology Operator: `Vhost`, `User`, `Permission`, `Queue` | 3 × 1–2 GiB *(est.)*; every quorum queue is a Raft group | `amqp091-go`, adequate | the best of the three: native dead-letter exchanges, `delivery-limit` on quorum queues | MPL-2.0; Broadcom patches only the latest minor | yes |
| **Pulsar 4.2** | *tenant → namespace → topic*, the only native model with storage, backlog and rate quotas per namespace | strongest | resources operator CRDs | 10+ JVM pods, 12–20 GiB *(est.)* | good, trails Java | native retry and DLQ topics | Apache-2.0 | yes |
| **Postgres per environment** (River, pgmq) | a database | physical | CloudNativePG — the `postgres` claim already does it | 100–250 MiB **× every environment** *(est.)* | River: excellent | River retry/discard; pgmq visibility timeout | MPL / PostgreSQL | yes (`postgresql` scaler) |

Three things from the survey change the shape of what is built, and are worth
holding onto whichever way the decision goes:

- **NATS accounts are the cheapest tenancy unit anything offers.** A preview
  environment is an account JWT and a stream or two. Deleting the account
  deletes its store. There is no prefixing discipline because there is nothing
  to prefix against: `orders` can exist in a hundred accounts. Per-account
  limits are what stop a preview starving production, and they are set by the
  thing that mints the account.
- **Durability has to be chosen, not assumed.** Jepsen's December 2025 analysis
  of NATS 2.12.1 found that JetStream acknowledges before it fsyncs (every two
  minutes by default), so a correlated power loss loses acknowledged writes;
  Synadia fixed the membership bugs found alongside and documented the
  default rather than changing it. A platform sets `sync_interval: always`
  for production-tier streams, uses R1 for previews and R3 for production
  where the cluster has three nodes — and on the single-node clusters this
  platform is built for, says plainly that R1 is what there is. Every
  replicated stream and consumer is a Raft group, so the count is budgeted
  per tier rather than allowed to grow with the pull request count.
- **The monitoring endpoint KEDA reads is unauthenticated and account-wide.**
  `/jsz` answers for every account. It stays behind the default-deny
  NetworkPolicy the platform namespace already has and is opened to KEDA and
  the operator alone — the same shape as every other rule in
  `templates/networkpolicy.yaml`.

And the ones ruled out, with the reason: Redpanda's licence forbids exactly
what a platform does with it; Pulsar's model is the best and its footprint is
ten JVM pods on a cluster that may have one node; Kafka is the right answer
when tenants need the Kafka protocol or its ecosystem, and neither is in the
brief; Postgres-per-environment is a fine job queue and a poor bus, because
there is no fan-out across services and the idle cost multiplies by the
environment count. RabbitMQ is the honest runner-up — vhosts are real, its
dead-letter story is native, its CRDs create tenants — and loses on the Go
client and on Broadcom's support policy.

Two layers that sit *above* a broker were looked at and are not recommended
as mandatory: **Knative Eventing** (CNCF graduated October 2025; adds an
ingress and a dispatcher hop, and its JetStream channel is still alpha) and
**Dapr** (healthy, 1.18 in June 2026; a sidecar on every pod and a Scheduler
and Placement control plane, for an abstraction the platform's own library
provides more cheaply). What both get right is worth copying: the broker is
platform configuration, the application only sees names.

## Durable execution — what has tenancy, and what it costs

| Engine | Tenancy in one deployment | Per-tenant quota / retention | Configured by env vars | Go SDK | Small HA footprint | An API a dashboard can wrap | Licence |
|---|---|---|---|---|---|---|---|
| **Inngest** self-hosted | none — one server is one environment | no / no; Postgres cleanup by hand | `INNGEST_BASE_URL` + keys; `INNGEST_ENV` ignored | `inngestgo` v0.x, active | server × N + Postgres + Redis, **per environment** | REST v1 by signing key; the GraphQL the UI uses is unauthenticated | SSPL, Apache-2.0 after three years |
| **Temporal 1.31** | *namespace*: retention, RPS, priority and fairness per namespace (fairness GA May 2026) | yes / yes | `TEMPORAL_ADDRESS`, `TEMPORAL_NAMESPACE`, `TEMPORAL_API_KEY` via `contrib/envconfig` | the best in the field; OTel interceptor in `contrib` | four services × 2 + HA Postgres; a fixed shard count sized up front; a third party puts a small self-host at $2.5–4.5k a month plus a quarter to one engineer | strong: gRPC and HTTP per namespace | MIT |
| **Restate 1.5** | none inside a cluster; Cloud "environments" are separate clusters | no / no | ingress URL | v0.x | 1–3 nodes + S3; **no auth on admin or ingress** — a cluster per tenant behind NetworkPolicy | admin REST and a SQL introspection endpoint | BSL (no public Restate service), SDKs MIT |
| **Hatchet** | *tenant*: per-tenant tokens, retention, limits, Prometheus endpoint, REST | yes / yes | `HATCHET_CLIENT_TOKEN`, `HATCHET_CLIENT_HOST_PORT` | new `sdks/go`, OTel package | engine + API + Postgres (RabbitMQ optional) | tenant-scoped OpenAPI: runs, events, workers, tokens | MIT |
| **DBOS Transact** | a library: the system database or schema per environment | queue concurrency and rate limits; retention is yours | `DBOS_SYSTEM_DATABASE_URL` | MIT, shipped 2025 | Postgres only | its system tables; the Conductor UI is proprietary | MIT (Conductor paid) |
| **River** | a library: schema or database per environment | yours | a DSN | native, excellent; River UI is an embeddable `http.Handler` | Postgres only | its tables | MPL-2.0; workflows in River Pro |
| **go-workflows** | a library, Temporal-shaped; Postgres, Redis, SQLite backends | yours | a DSN | native | Postgres | mountable `diag` UI | MIT |
| Trigger.dev v4 | org / project / env incl. preview branches | limits | API key per env | **none — TypeScript only**; builds and deploys images itself | Postgres, Redis, ClickHouse, Electric, MinIO, s2-lite | REST runs API | Apache-2.0 |
| Cadence | *domain* + task-list isolation | yes | client config | first-class | Temporal-like, Cassandra | gRPC | MIT, CNCF sandbox |

Read against the brief:

- **Only Temporal, Hatchet and Cadence give tenancy inside one deployment.**
  Hatchet alone has per-tenant retention, limits, metrics and tokens on
  nothing but Postgres, under MIT, and a tenant-scoped REST API the dashboard
  could draw "your runs, your failures" from. It is the closest thing to "one
  shared, tenant-isolated engine" that exists, and if the decision is
  engine-first, it is the engine.
- **Temporal is the best SDK and the heaviest thing to run**, and it is the
  wrong weight for a platform whose reference cluster is one node. A
  namespace per environment maps onto the claim model cleanly; four services
  and a fixed shard count do not map onto anything.
- **The library engines make isolation free**, because the `postgres` claim
  already gives every environment a database of its own and previews a fresh
  one. DBOS and River need no server, no tenancy design and no licence
  worry; they need the platform to draw the run screen itself from tables.
  That is the same job the platform already does for CronJob runs.
- **Inngest and Restate are "a cluster per tenant" engines.** Both are single
  binaries, so it is affordable; both are source-available licences that
  permit exactly this use and would not permit selling it; neither has an
  answer to the brief's tenancy requirement. The `inngest` claim stays as a
  bring-your-own-engine path and is not the platform's answer.

## The developer-experience bar

What the products developers compare a "Vercel alternative" against actually
do, and what of it a Go-first platform should borrow.

**Declared in code, discovered before deploy.** Encore's resources are
package-level variables — `pubsub.NewTopic[*SignupEvent]("signups", …)`,
`pubsub.NewSubscription(Signups, "send-welcome-email", …)`,
`cron.NewJob(…)` — and its parser refuses a declaration anywhere else, which
is what makes the set knowable without running a handler. Nitric collects
declarations into a spec by running the application in a describe mode. Both
derive least-privilege credentials from the model: a service that only
publishes gets publish rights.

**Logical names in code, a per-environment document binding them to physical
resources.** Encore's self-host infra config is a JSON per environment mapping
each declared topic to a real one; Dapr's `Component` is a namespaced object
the code never names. The application never learns a broker URL.

**Previews are isolated by default — and this is where the field is weak.**
Cloudflare's own docs: "a Queue can have only one consumer Worker, and the
Queues service does not yet register a Preview as that consumer", so a preview
publishes into production's queue. Encore does not run cron in previews.
Vercel Queues partitions every topic by deployment id, so previews and old
versions never read each other's messages — and documents the cost, a
rolled-back deployment retrying its own messages until they expire. Inngest's
Vercel integration creates a branch environment per preview and archives it
three days after the last deploy.

**Consumers are private, and retries are declared next to them.** Vercel
consumers have no public URL; `maxDeliveries`, `max_retries`,
`dead_letter_queue`, `AckDeadline`, `MaxConcurrency` sit on the subscription.
The poison path is explicit — Watermill's `Poison` middleware and Postgres
`Requeuer`, Vercel's `acknowledge: true` from the retry callback.

**The dashboard shows lag and retry depth.** Vercel's queue screen: per queue,
messages per second, queued, received, deleted, redeliveries; per consumer
group, *Max Message Age* and *Retry Depth*, with a preset alert on retry depth
over five within five minutes. Supabase shows each message's status, retry
count and payload. Encore draws a flow map from the declarations.

**Durable steps are table stakes now.** Vercel Workflows went GA in April 2026
(`'use workflow'`, `'use step'`, on pluggable "Worlds" — local, Postgres and
Vercel, the Postgres one on graphile-worker); Cloudflare Workflows a year
earlier (`step.do`, `step.sleep`, `step.waitForEvent`, waiting instances
costing nothing). Every one of them is *a function in the application*, with
the platform holding the journal.

**Cron is declared in code and runs on the platform.** Encore, Convex and
Modal all call a handler inside the running application on schedule rather
than starting a process per tick — which matters against Kubernetes CronJob's
"approximately once", its stop-after-100-missed rule, and the seconds of pod
start-up a per-tick Job pays.

**The standards to build on**, so the format is not invented:
[CloudEvents](https://cloudevents.io) as the envelope (`sdk-go` has a
JetStream binding and an OTel observability service); OpenTelemetry's
messaging semantic conventions (still *Development* status: `send` is a
PRODUCER span, `process` a CONSUMER span, batches joined by span links,
`messaging.system`, `messaging.destination.name`,
`messaging.consumer.group.name` on every span); AsyncAPI 3 as the generated
contract, the way Encore generates API docs from its model. And one finding
that decides who writes the tracing: **NATS has no official OpenTelemetry
instrumentation in Go**, only community wrappers, and Watermill's is
third-party. The library has to own that layer, and it is a differentiator
when it does.

**Two cautionary data points on portability.** Nitric's authors pivoted to a
hosted product and the framework's last push was February 2026; Deno's new
Deploy dropped `kv.enqueue` and `listenQueue` when Deploy Classic shut down in
July 2026. Platform-specific queue APIs get abandoned. The in-application API
should stay thin and backend-neutral — a Publisher and a Subscriber over a
CloudEvents envelope — so the platform can change brokers under it.

## The options for Kitchen

Four coherent shapes, judged on the brief's own axes: tenancy without a
deployment per environment, a library, first-class monitoring, and fit with
the premises the platform already holds (it owns its cluster; nothing needs
`kubectl`; claims are the contract; previews are isolated by construction;
one-node clusters are normal; KEDA is there).

### A — a shared NATS JetStream, an account per environment, a Kitchen library

**Substrate.** One NATS StatefulSet in `kitchen-system`, rendered by the chart
like ClickHouse and MinIO are — NATS needs no CRD, so none of the Helm
constraints that push KEDA and CloudNativePG into the Addon catalogue apply;
the operator drives it through the JetStream API and the account resolver
directly, with no NACK in between. JetStream file storage on the default
StorageClass; the internal CA issues its certificate like every other bundled
store, so the last plaintext listener is not this one.

**Tenancy.** The operator mints one NATS account per Environment
(`<project>-<environment>`, previews included), pushes its JWT to the
resolver, sets its limits by tier — a preview gets a small memory and file
budget, R1, a stream cap; production gets the project's declared budget, R3
where the cluster can, `sync_interval: always` — and mints a user credential
scoped to the account. The credential lands in a binding Secret in the
application namespace carrying `managed-by: kitchen`, mounted as a creds file
at `/var/run/kitchen/events/<claim>/user.creds` beside the URL the way a
Postgres claim mounts its CA. Deleting the environment deletes the account,
and with it every stream and every unconsumed message it held. **Nothing has
to be prefixed** and nothing can be misaddressed: `INNGEST_ENV`'s failure mode
— an application overwriting one variable and reaching another project's
branch — does not exist, because the credential *is* the tenancy.

**Provisioning.** Streams and consumers are derived from what the project
declares: a stream per topic (or one per project with subject filters — a
sizing decision for the build), a durable pull consumer per subscription with
explicit acks, `MaxDeliver`, a `BackOff` list and an ack wait taken from the
declaration. The platform subscribes to the account's `MAX_DELIVERIES`
advisories and republishes the exhausted message into a dead-letter stream of
the environment's — **the DLQ NATS does not ship is the one thing the platform
builds**, and it is what makes "failed events, with replay" a screen rather
than a runbook.

**Autoscaling, and the scale-to-zero problem #268 left.** A subscription's
worker is a `worker` process scaled by a KEDA `ScaledObject` on the JetStream
scaler: pending messages above a threshold wake it, none idle it to zero.
That is *per workload* rather than per project, which is the answer #271 asked
for and connect-mode Inngest could not give, because the decision is KEDA's
from the broker's numbers rather than the HTTP interceptor's from a socket.

**The library.** Go first, TypeScript second; the wire protocol is NATS plus
CloudEvents, so any language with a NATS client works in the meantime, and
the library is convenience and instrumentation rather than a protocol.
Package-level declarations in Encore's shape, a handler per subscription with
retry, ack deadline and concurrency beside it, a `Publisher` that stamps the
CloudEvents attributes and the `traceparent`, PRODUCER and CONSUMER spans with
the `messaging.*` attributes and span links, and the two environment variables
the binding provides. A `kitchen events describe` mode emits the declaration
set as a manifest — the one input the operator provisions from.

**Where the declaration lives** is a decision of its own (below). The
platform's precedent is `kitchen.json`, frozen into the Release so a rollback
runs the subscriptions that release declared; the field's precedent is
code-derived. The two compose: the library can validate at start-up that every
topic it uses is declared, and fail with a message naming the missing line.

**Monitoring.** The operator reads each account's stream and consumer info —
pending, ack pending, redelivered, the oldest unacknowledged message's age —
and answers it on the environment's routes: a topics table, a subscriptions
table with *max message age* and *retry depth*, the dead-letter list with
replay and drop, each event's trace link where the application published one
through the library. Two signals join the catalogue, consumer-lagging and
retry-depth, and land in the diagnostics strip like every other. A flow map
— who publishes what, who consumes it — falls out of the declarations and is
the one screen Encore has that nothing else here does.

**Durable steps** are a second phase, and this shape does not decide them:
a `step.Run(ctx, "charge", fn)` memoised in a per-environment JetStream
key-value bucket (which accounts also isolate), or in the environment's own
Postgres in the DBOS pattern; `sleep` and `waitForEvent` on NATS 2.12's
delayed scheduling. Or Hatchet beside it, if the steps turn out to be the
thing people mostly want.

**Cross-project events** are NATS account exports and imports — the
providing project's environment exports a subject, the consuming one imports
it — which is #489's `offers` and `service` claim with `speaks: events`, and
not a second mechanism.

**Cost.** One StatefulSet (three pods where the cluster has three nodes, one
where it has one), a reconciler, a library, three routes and a screen. Per
environment: a JWT, a Secret, a few streams. The DLQ path and the observability
layer are the two pieces nothing hands over.

### B — Hatchet as a shared engine, a tenant per environment

Engine-first. Hatchet runs once in `kitchen-system` on its own CloudNativePG
Cluster; the operator creates a Hatchet tenant per environment with its own
token, retention and limits, and binds `HATCHET_CLIENT_TOKEN` and
`HATCHET_CLIENT_HOST_PORT`. The developer gets queues, durable workflows with
steps and sleeps, cron, rate limits and concurrency keys from one SDK, and the
dashboard draws runs and failures from the tenant-scoped REST API. Everything
the brief asks for, from one product, MIT.

What it costs: it is a control plane with its own opinions — its own
workflow model, its own Go SDK that has just been rewritten (`sdks/go`
replacing a deprecated v1), its own UI that the platform would either embed or
re-draw. Events between *projects* are not its model; a tenant is a wall.
It is a bet on one vendor's roadmap for the whole feature, which the Nitric
and Deno data points argue against. And the library the brief asks for would
be Hatchet's SDK with a Kitchen binding, not Kitchen's.

**Keep it as the engine candidate for phase two** if durable execution turns
out to be the centre of gravity, rather than the substrate for phase one.

### C — Postgres per environment as the substrate, an embedded library

No new infrastructure. An `events` capability is a `postgres` claim with a
queue schema in it; the library is River- or DBOS-shaped and talks to the
environment's own database; a preview gets a fresh empty database as it does
today; isolation is physical; transactional enqueue — the order row and the
`orders.placed` message in one commit — is the one thing no broker gives.

What it costs: it is a job queue, not a bus. No fan-out to another project,
no subscription that is not a poll, vacuum pressure under real throughput, and
an idle Postgres per environment — 100 to 250 MiB, a volume and a backup
policy each — which is the Inngest shape again with a smaller pod. **Right
for "the application's own jobs live in its own database", wrong as the
platform's eventing story.** Note it stays available regardless: a project
that wants River today claims a `postgres` and uses it.

### D — Temporal namespaces

The best Go SDK there is, `envconfig` and OTel in `contrib`, a namespace per
environment created and deleted by the operator, retention and fairness per
namespace. And four services, a fixed shard count, a Postgres of its own, a
custom Authorizer to scope the UI, and a footprint a one-node cluster does
not have. It is the right answer for an organisation that has already chosen
Temporal and the wrong first move for this platform. Recorded so the question
does not have to be re-opened from scratch.

### The comparison

| | A · NATS + library | B · Hatchet | C · Postgres per env | D · Temporal |
|---|---|---|---|---|
| Deployment per environment | **no** — an account | no — a tenant | **yes** — a database | no — a namespace |
| Isolation | structural, by credential | by token, in-product | physical | by namespace, via Authorizer |
| Preview cost | JWT + streams | a tenant row | a Postgres | a namespace + shards |
| Cross-project events | account export/import — fits #489 | no | no | no |
| Durable steps | phase two, library or engine | **built in** | library (DBOS/River) | built in |
| Library | Kitchen's, thin, owns OTel | Hatchet's SDK | River/DBOS-shaped | Temporal's SDK |
| Monitoring source | JetStream API per account | tenant REST + Prometheus | tables | gRPC per namespace |
| Footprint | one small StatefulSet | engine + API + Postgres | 0.1–0.25 GiB × envs | 8+ pods + Postgres |
| Licence | Apache-2.0 | MIT | MPL / PostgreSQL | MIT |
| Fits one-node cluster | yes | yes | yes, until it does not | no |
| Vendor-roadmap exposure | low (CNCF, protocol-level) | high | low | medium |

**Recommendation: A**, with B held as the answer to "and then durable steps"
if the library-over-Postgres route proves too thin, and C's transactional
enqueue noted as a feature the library can offer against a `postgres` claim
later (an outbox the library drains into the bus).

## Decisions the discussion has to make

These are the forks the recommendation leaves open, in the order they block
work.

1. **Transport first, or engine first?** A builds the bus and the screens and
   leaves durable steps to phase two; B buys everything at once from Hatchet
   and inherits its model. The recommendation is A. The counter-argument worth
   hearing is that the application #394 was written about wanted Inngest's
   *steps*, not a queue.
2. **Where the declaration lives.** In `kitchen.json` (`events.topics`,
   `events.subscriptions`, `events.schedules` — the precedent, reviewable in
   the pull request, frozen into the Release, no parser to write), or derived
   from code (`kitchen events describe` running the built image in a describe
   mode after the build, Nitric's approach; Encore's static parser is a
   project of its own). The recommendation is `kitchen.json` first with the
   library checking its own declarations against it at start-up, and a
   `describe` that *generates* the block as the second step.
3. **Bundled in the chart or an Addon.** NATS needs no CRD, so the chart can
   render it beside ClickHouse; the question is whether it is on by default.
   Recommendation: rendered by the chart, `events.enabled` defaulting to true,
   because a platform premise is that dependencies are bundled rather than
   listed.
4. **What a schedule becomes.** Today a `cron` process is a Kubernetes CronJob
   and a run is a pod. The field calls a handler inside the running
   application instead, and NATS 2.14 can publish on a schedule. Either the
   `cron` process stays and the library adds a *scheduled subscription* as a
   second shape, or the CronJob is reframed as one implementation of a
   schedule trigger. Recommendation: add, do not replace — a CronJob is the
   right shape for a nightly report that wants its own pod and timeout.
5. **Languages.** Go first, because it is what the operator, the CLI and the
   reference client are written in; TypeScript second, because the dashboards
   people deploy are Vue and their backends are Node. The wire format keeps
   every other language working with a NATS client alone.
6. **Durability tier defaults.** R1 everywhere on a one-node cluster and said
   so on the environment; R3 and `sync_interval: always` for production where
   three nodes exist; what a preview's storage cap is. These are numbers to
   choose, and the Jepsen finding is why they are chosen rather than defaulted.
7. **Cross-project events now or after #489.** Account exports are cheap to
   build and expensive to design without the offerings model. Recommendation:
   after, as a `speaks: events` offering.

## Phasing, if A is chosen

- **Phase 1 — the substrate and the claim.** NATS in the chart, TLS from the
  internal CA, the NetworkPolicy rule; an `events` claim type (or a project
  setting — it takes no Connection, like `volume` and `oidcClient`) whose
  reconciler mints the account per environment, provisions streams and
  consumers from the declaration, builds the dead-letter stream from
  advisories and writes the binding; the KEDA `ScaledObject` for a worker
  bound to a subscription; the route chain — `internal/api/policy.go`, the
  route table, `docs/api/events.md`, the regenerated dashboard policy — and
  the environment screen with topics, subscriptions, lag, retry depth and the
  dead-letter list; the two signals; `kitchen events` in the CLI or the
  decision that `kitchen api` carries it.
- **Phase 2 — the library.** `kitchen-go` (`sdk/go` in this repository, so
  one tag versions it with everything else): declarations, publisher,
  subscriber, retry and poison middleware, CloudEvents, the OpenTelemetry
  layer, `describe`. Then the TypeScript port.
- **Phase 3 — durable steps.** Memoised steps and sleeps in the library over
  a per-environment key-value bucket or the project's Postgres; or Hatchet as
  an Addon, tenant per environment, if the library route proves too thin.
- **Phase 4 — the edge.** Topics as offerings, exports and imports between
  accounts, the flow map across projects.

## Sources

The field survey behind the tables, with the pages the claims were checked
against.

- NATS: [server 2.14 release](https://nats.io/blog/nats-server-2.14-release/),
  [accounts](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/accounts),
  [JWT resolver](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_intro/jwt/resolver),
  [auth callout](https://docs.nats.io/running-a-nats-service/configuration/securing_nats/auth_callout),
  [NACK](https://github.com/nats-io/nack/blob/main/README.md),
  [reliable delivery and DLQ](https://www.synadia.com/blog/jetstream-reliable-delivery-dlq-replay),
  [Jepsen: NATS 2.12.1](https://jepsen.io/analyses/nats-2.12.1) and
  [Synadia's response](https://www.synadia.com/blog/jepsen-nats-2-12-1),
  [CNCF and Synadia agreement](https://www.cncf.io/blog/2025/05/01/protecting-nats-and-the-integrity-of-open-source-cncfs-commitment-to-the-community/),
  [KEDA JetStream scaler](https://keda.sh/docs/2.20/scalers/nats-jetstream/).
- Kafka and Strimzi: [Kafka 4.2](https://kafka.apache.org/blog/2026/02/17/apache-kafka-4.2.0-release-announcement/),
  [Kafka 4.3](https://kafka.apache.org/blog/2026/05/22/apache-kafka-4.3.0-release-announcement/),
  [Strimzi releases](https://github.com/strimzi/strimzi-kafka-operator/releases),
  [franz-go](https://pkg.go.dev/github.com/twmb/franz-go/pkg/kgo).
- Redpanda: [licensing](https://docs.redpanda.com/current/get-started/licensing/overview/),
  [requirements](https://docs.redpanda.com/current/deploy/redpanda/kubernetes/k-requirements/).
- RabbitMQ: [4.3 release](https://www.rabbitmq.com/blog/2026/04/23/rabbitmq-4.3-release),
  [operators](https://www.rabbitmq.com/kubernetes/operator/operator-overview),
  [community support policy](https://www.rabbitmq.com/blog/2024/05/31/new-community-support-policy).
- Pulsar: [4.2.0](https://pulsar.apache.org/release-notes/versioned/pulsar-4.2.0/),
  [resources operator](https://github.com/streamnative/pulsar-resources-operator).
- Postgres queues: [River](https://github.com/riverqueue/river/releases),
  [pgmq](https://github.com/pgmq/pgmq/releases),
  [CloudNativePG 1.29](https://www.postgresql.org/about/news/cloudnativepg-1290-released-3266).
- Layers: [Knative graduation](https://www.cncf.io/announcements/2025/10/08/cloud-native-computing-foundation-announces-knatives-graduation/),
  [eventing-natss](https://github.com/knative-extensions/eventing-natss),
  [Dapr 1.17](https://blog.dapr.io/posts/2026/02/27/dapr-v1.17-is-now-available/),
  [Dapr 1.18](https://blog.dapr.io/posts/2026/06/10/dapr-v1.18-is-now-available/),
  [Dapr pub/sub scopes](https://docs.dapr.io/developing-applications/building-blocks/pubsub/pubsub-scopes/).
- Engines: [Inngest self-hosting](https://www.inngest.com/docs/self-hosting),
  [Inngest environments](https://www.inngest.com/docs/platform/environments),
  [Inngest dashboard auth discussion](https://github.com/orgs/inngest/discussions/1932),
  [Temporal multi-tenancy](https://docs.temporal.io/evaluate/development-production-features/multi-tenancy),
  [Temporal environment configuration](https://docs.temporal.io/develop/environment-configuration),
  [Temporal priority and fairness GA](https://temporal.io/changelog/priority-fairness-generally-available),
  [temporal-operator](https://github.com/alexandrevilain/temporal-operator),
  [Restate on Kubernetes](https://docs.restate.dev/server/deploy/kubernetes),
  [Restate security](https://docs.restate.dev/server/security),
  [Restate licence](https://github.com/restatedev/restate/blob/main/LICENSE),
  [Hatchet self-hosting](https://docs.hatchet.run/self-hosting),
  [Hatchet data retention](https://docs.hatchet.run/self-hosting/data-retention),
  [Hatchet Prometheus metrics](https://docs.hatchet.run/self-hosting/prometheus-metrics),
  [DBOS Transact Go](https://github.com/dbos-inc/dbos-transact-golang),
  [Trigger.dev self-hosting](https://trigger.dev/docs/self-hosting/kubernetes),
  [go-workflows](https://github.com/cschleiden/go-workflows),
  [Watermill](https://github.com/ThreeDotsLabs/watermill).
- Developer experience: [Encore pub/sub](https://encore.dev/docs/go/primitives/pubsub),
  [Encore infra config](https://encore.dev/docs/go/self-host/configure-infra),
  [Encore Flow](https://encore.dev/docs/platform/observability/encore-flow),
  [Nitric](https://nitric.io/docs/messaging) and [its GitHub](https://github.com/nitrictech),
  [Cloudflare Workflows GA](https://blog.cloudflare.com/workflows-ga-production-ready-durable-execution/),
  [Cloudflare preview resources](https://developers.cloudflare.com/workers/previews/resources/),
  [Vercel Queues concepts](https://vercel.com/docs/queues/concepts) and
  [observability](https://vercel.com/docs/queues/observability),
  [Vercel Workflows](https://vercel.com/docs/workflows) and
  [Worlds](https://workflow-sdk.dev/worlds/building-a-world),
  [Inngest branch environments](https://www.inngest.com/blog/branch-environments),
  [Supabase Queues](https://supabase.com/docs/guides/queues),
  [Convex scheduling](https://docs.convex.dev/scheduling/scheduled-functions),
  [Deno Deploy migration](https://docs.deno.com/deploy/migration_guide/).
- Standards: [CloudEvents sdk-go](https://github.com/cloudevents/sdk-go),
  [OpenTelemetry messaging spans](https://opentelemetry.io/docs/specs/semconv/messaging/messaging-spans/),
  [asyncapi-codegen](https://github.com/lerenn/asyncapi-codegen),
  [Kubernetes CronJob](https://kubernetes.io/docs/concepts/workloads/controllers/cron-jobs/).
