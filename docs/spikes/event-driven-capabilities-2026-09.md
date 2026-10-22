# Asynchronous work for every audience — a capability map

*September 2026. The general-audience shape of the feature the
[event-driven spike](event-driven-2026-09.md) surveyed and the
[questionnaire](event-driven-questionnaire-2026-09.md) probed. The
[findings](event-driven-findings-2026-09.md) cover one persona and say so;
this is written for the product, which is for many.*

## The premise

Kitchen is a self-hosted Vercel alternative for teams that bring their own
cluster. The teams that arrive are not one shape. A Rails shop arrives with
Sidekiq. A Django shop arrives with Celery. A Node shop arrives with BullMQ.
A Laravel shop with Horizon, a Phoenix shop with Oban, a .NET shop with
Hangfire or MassTransit, a Java shop with Kafka or RabbitMQ and Spring, a Go
shop with River or asynq or a NATS client. A team that has outgrown its job
library arrives wanting Temporal. **Every one of them already has a
programming model and a library in their language, maintained by somebody
who is not Kitchen.** What they do not have, and what a platform is for, is
the infrastructure behind it per environment, isolation between
environments, a screen that shows the queue, an alert when it falls behind,
and workers that scale with it.

That reframes the question from "which eventing product should Kitchen be"
to "which infrastructure should Kitchen provision, and what should it show".
It is the same reframing the platform already made for databases (a
`postgres` claim with Neon or CloudNativePG behind it, and the team's own
driver in front), for caches (`redis` with Valkey or a hosted Redis), and
for durable jobs (`inngest` with Cloud or self-hosted). **The answer for
asynchronous work is the same pattern, three times, at three levels of
ambition**, and none of the three ships a Kitchen client library.

## Three capabilities

| Level | Capability | The audience it serves | What the team writes against | What Kitchen provisions per environment | Tenancy unit |
|---|---|---|---|---|---|
| **1** | **A job queue behind the library you already use** | the majority: Sidekiq, Celery, BullMQ, Horizon, Oban, Hangfire, River, asynq, Solid Queue, pg-boss | their job library, unchanged | a Valkey (`redis` claim, `usage: queue`) or their Postgres — **both exist** | an instance or a database |
| **2** | **A message broker with a tenant per environment** | teams with several services, or an existing broker: RabbitMQ, Kafka, NATS, hosted equivalents | the broker's standard client in their language | a vhost, an account, or a topic prefix with an ACL'd user, on a broker the operator connected or Kitchen bundles | the provider's own: vhost, account, prefix |
| **3** | **A durable-execution engine with a namespace per environment** | teams that need memoised steps, sleeps, signals: Temporal, Inngest, Hatchet | the engine's own SDK | a namespace or tenant, keys, and the address; or a server per environment where the engine has no tenancy | the engine's own: namespace, tenant, server |

Each level is a **capability with providers**, which is the `Connection`
model the platform already runs on. An operator connects what the
organisation has — the RabbitMQ it already runs, a Temporal Cloud account, a
Confluent cluster — or takes what Kitchen bundles. A developer claims the
capability and gets a binding. The provider declares what a preview gets
and what deletion destroys, as every provider does today. Nothing about the
developer's code knows which provider answered.

### Level 1 — the queue behind the library they already use

Most teams never leave this level, and today Kitchen serves it half-way: a
`redis` claim with `usage: queue` gives a Valkey that refuses writes rather
than evicting them, previews get a fresh one, and a `worker` process runs
the consumer. What is missing is everything a platform should add on top,
and all of it is possible without a library because **every one of these
job systems keeps its state in a known layout** in the store the platform
provisioned:

- **A queue screen.** Depth, in flight, failed, scheduled and oldest job per
  queue, read straight from the Valkey or the Postgres. Sidekiq, BullMQ,
  Celery, Oban, Solid Queue and River each have a documented key or table
  layout, and each has an existing exporter that reads it (sidekiq
  exporter, bull exporter, celery exporter). Which layout applies is
  detected from the repository the way the build strategy already detects
  the framework (`framework.Detect`), and declared in `kitchen.json` where
  detection cannot decide. An unknown library gets the store's own numbers
  (memory, keys, connections) and no queue rows, honestly.
- **Workers that scale on the queue.** KEDA's Redis lists and streams
  scalers and its PostgreSQL scaler already exist; the platform writes the
  `ScaledObject` for a `worker` process that names the queue it drains, and
  scale to zero follows for workers the way it does for web processes. That
  is the per-workload idling #271 asked for, for the audience that has
  workers today.
- **Signals.** *Queue backlog growing*, *failed jobs rising*, *no worker
  consuming*: into the catalogue and the diagnostics strip, from the same
  reads.
- **Previews.** Already fresh per preview for Valkey; a worker in a preview
  stays opt-in, a scheduled job in a preview stays off.

This is the cheapest level, it needs no new dependency and no new claim
type, and it serves the largest audience. It is also the one no competitor
does well: Vercel and Cloudflare cannot run a Sidekiq at all, and Railway,
Render and Fly give a Redis and nothing above it.

### Level 2 — a broker with a tenant per environment

For teams whose services talk to each other, or that arrive with a broker.
A `messaging` capability (the name is provisional) with providers:

| Provider | Tenancy per environment | How Kitchen creates it | Notes |
|---|---|---|---|
| **NATS JetStream**, bundled | an account | JWT pushed to the resolver; per-account limits by tier | the spike's substrate; cheapest to run, structurally isolated, one Go binary; the default when the operator connects nothing |
| **RabbitMQ**, connected or bundled | a vhost | management API or the Topology Operator's `Vhost`/`User`/`Permission` | the strongest existing audience among brokers; native dead-lettering |
| **Kafka**, connected (Strimzi or hosted) | a topic prefix and an ACL'd user | `KafkaTopic` / `KafkaUser` | for the audience that needs the Kafka protocol; storage quotas are the honest gap |
| hosted: CloudAMQP, Upstash, Confluent, Synadia | as above, via the vendor's API | | |

The binding is a URL and a credential. The developer uses the standard
client for that broker in their language — every broker maintains clients
in every mainstream language, which is the fact that removes the library
question. Observability is the broker's server-side data, normalised to one
vocabulary on the screen (backlog, in flight, redelivered, failed, oldest
message) — the spike's Tier 0, now per provider. The HTTP-push dispatcher
the spike designed stays available as an optional delivery mode for a
subscription that wants scale to zero and a trace through the queue for
free, but it is an addition to this level, not its basis.

Cross-project events land here, as an offering (#489) whose implementation
is the provider's cross-tenant mechanism: an account export in NATS, a
shovel or federation in RabbitMQ, an ACL grant in Kafka.

### Level 3 — a durable-execution engine with a namespace per environment

The `backgroundJobs` capability already exists with `inngest` and
`inngestSelfHosted` behind it. The general-audience move is to add the
engine the wider industry standardises on:

| Provider | Tenancy per environment | SDKs | Notes |
|---|---|---|---|
| **Temporal**, self-hosted as an Addon, or Temporal Cloud connected | a namespace, with retention and rate limits per namespace | Go, Java, TypeScript, Python, .NET, PHP, Ruby, by Temporal | MIT; single-process server on a CloudNativePG for small clusters; KEDA scaler exists; the industry's default answer |
| **Inngest**, existing | Cloud: a branch environment; self-hosted: a server per environment | TypeScript, Go, Python | the persona's choice; keep |
| **Hatchet** | a tenant | TypeScript, Go, Python | MIT, Postgres-only, tenant-scoped API; younger |

The developer uses the engine's SDK. Kitchen mints the namespace, the keys
and the address; declares what a preview gets; watches the engine's health
from outside it; and draws runs and failures from the engine's API on the
project's screen, so the vendor UI is never the thing a member has to be
given access to.

## What is the platform's at every level

The list that came out of the findings is not persona-specific; it is what
a platform owes any of the three levels, and building it once is most of
the feature:

- **One screen vocabulary.** Backlog, in flight, failed, oldest, throughput,
  duration — per queue, per subscription, or per function — on the
  environment page, from whichever provider answered. A parked-work list
  with retry and discard where the provider supports it, and the honest
  absence where it does not.
- **Signals and alerts** from the same reads, through the platform's own
  notification path, so a dead engine is reported by something that is not
  the engine.
- **A registration or consumer diff on deploy.** Fewer functions registered,
  or fewer consumers on a queue, after a release than before is a failed
  deploy or a firing signal. It is the most common silent failure the
  answers reported and it is provider-independent.
- **Preview defaults.** Consumers run in previews against the preview's own
  tenant; scheduled work does not run in previews unless asked; nothing in a
  preview can reach production's tenant, by the provider's construction.
- **Scale to zero for workers**, on the provider's backlog, through KEDA.
- **Declarations in `kitchen.json`**: the queues a worker drains, the
  subscriptions a process holds, the functions an app registers — frozen
  into the Release so a rollback runs what that release declared, and the
  input the platform provisions and diffs against.
- **Schemas to types**, optional: a JSON Schema per event in the declaration,
  generated types in the project's languages, an AsyncAPI document. Asked
  for by every bilingual team and useful to every polyglot one.
- **Scheduled calls to a route**, beside the CronJob: for the audience whose
  handler is a function in a running app.

## Order

1. **Level 1 first.** It serves the largest audience, needs no new
   dependency, and its pieces (queue detection, the screen, KEDA on the
   backlog, the signals) are the pieces every later level reuses.
2. **Level 3's Temporal provider second**, because the durable-execution
   audience is the one that arrives with the sharpest requirement and the
   industry has one default answer; the engine question for the persona in
   the findings (Inngest properly, or Hatchet) is settled inside this level
   by its spike.
3. **Level 2 third**, NATS bundled as the default provider and RabbitMQ
   connected, with the HTTP-push dispatcher as its optional delivery mode
   and cross-project events through offerings.

And the questionnaire goes in front of teams the platform's author does not
run — the Rails, Django, Java and .NET shapes above — before level 2's
provider order or level 3's second engine are fixed. The answers from one
persona were enough to show the instrument works and to show that persona
what it needs; they are not enough to choose for anyone else.
