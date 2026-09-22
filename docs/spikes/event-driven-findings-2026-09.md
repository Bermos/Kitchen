# What the teams said — findings from the asynchronous-work questionnaire

*September 2026. Four applications answered
[the questionnaire](event-driven-questionnaire-2026-09.md): Velostand
(`publibike`), Werkverzeichnis, zäme and Enterprise. This reads the answers
against the key written into the questionnaire before they arrived, and says
what they decide. It supersedes the recommendation in
[the event-driven spike](event-driven-2026-09.md) where the two disagree,
and it disagrees on three points.*

## The three findings that change the plan

**1. Nobody needs a bus.** No application has an event with two consumers
today. Two of them emit events that go nowhere (`events/party.opened`,
`training/*`). Cross-team events are a daily webhook in one case and "not
applicable" in the rest. The one Go-only application is clock-driven and
would not use a bus for anything but a cache-invalidation broadcast it can
do with Postgres `LISTEN/NOTIFY`. Fan-out was marked *later* by three teams
and *not for us* by one. A shared NATS JetStream with an account per
environment answers a question nobody asked.

**2. Everybody who does asynchronous work uses memoised steps, and one
depends on them.** Three of four applications already run on Inngest. All
three use `step.run`; Enterprise's remediation, research and venture-sweep
functions are built on step memoisation, idempotency keys, concurrency
limits and throttles, and it named losing any of them as its deal-breaker.
zäme's invite fan-out is one memoised step per recipient so that a retry
after mail seven does not resend one to six. Werkverzeichnis's export is
three steps. Nobody uses `waitForEvent` in production and nobody needs
compensation, so a heavyweight workflow engine is more than anyone asked
for — but a plain pub/sub with retries is less than two of the three need,
and both said so: zäme's answer to "would you use the bus" was *only if it
has delayed delivery you can cancel or replace by key*, and Enterprise's was
*no, the durable workflow*.

**3. The languages are TypeScript and Go, and that is the whole list.**
Three applications are TypeScript on Nuxt; three have Go (one entirely, two
as a worker). Nothing else appears. Every team that uses an SDK uses the
vendor's, in both languages, and the recurring ask is *one schema generating
both TypeScript and Go types*, because two of them hand-mirror payload types
between the two today. "A library in as many languages as possible" is, in
practice, a library in two, and both already exist for every candidate.

## The answers, against the key

### B and C — the family

| | Velostand | Werkverzeichnis | zäme | Enterprise |
|---|---|---|---|---|
| Multi-step, must finish | – | T, shallow (3 steps) | T, mild (per-recipient) | **T, load-bearing** |
| Long waits | – | – | **T** (days to weeks, future-dated) | L (polls instead) |
| Wait for something external | – | – | – | L (built, unused) |
| Rows that must not be lost or run twice | none: failures are data | 1 (split results) | 2 (invites, cancellation) | 2 (remediation, briefing) |
| Runs on today | in-process ticker + CronJob | Inngest Cloud | Inngest | Inngest, self-hosted, own manifests |

Velostand is outside the feature. Its asynchronous work is a poller that
must never run twice and never skip silently, and its one ask of the
platform is a *singleton worker that is not routed as a web backend* —
Kitchen #250 and #256. Both have shipped since the workarounds were written
(the web pods carry `kitchen.bermos.dev/component: web` for the Service to
select on, and `singleton` exists on a process). The team should be told,
and their `-mode=all` workaround retired; that is a support conversation,
not a platform feature.

The other three are one family: **event-triggered durable functions** —
an event or a clock starts a function, the function's steps are memoised so
a retry resumes, and retries, concurrency, throttle and idempotency are
declared beside the function. That is Inngest's model, and they chose it
independently of each other and of the platform.

### D — volume

Tiny, everywhere. The largest is Enterprise at about 3,300 runs a day, of
which 2,900 are two transit polls. Payloads are IDs. Nobody keeps events
beyond a week except zäme's *undelivered* future-dated ones, which must
survive months and every redeploy in between. **No substrate decision is
forced by volume; anything works, Postgres included.**

### E — the screen

| Wanted | Teams |
|---|---|
| The error the handler returned, per event | 4 of 4 |
| Parked events with payload, retry and discard | 3 of 4 |
| Backlog and oldest unprocessed | 3 of 4 (Werkverzeichnis: "this catches *nothing registered*") |
| Throughput and duration per function | 3 of 4 |
| A map of who publishes what and who consumes it | 2 of 4, and it would have shown both orphaned events |
| Failures by deploy, before and after | 2 of 4, and it is Werkverzeichnis's actual incident |
| A trace through the queue | 2 want it, 2 would take it if free; nobody runs OpenTelemetry today |

Two asks are not on the list and matter: zäme wants a view of **scheduled,
not yet due** deliveries per event, with cancel and reschedule; Enterprise
points out that an engine that delivers the alerts **cannot alert on its own
death**, so its health has to be watched from outside it — which is what
`status.components` and the signal catalogue exist for.

Every incident reported was found by a person, never by an alert: functions
silently de-registered after a deploy; a job "completing" with empty data
through memoisation misuse; state wiped on restart; runs stuck after an
eviction; an unauthenticated delivery endpoint found by reading SDK source.

### F — previews

As predicted: consumers yes (3 of 4; Velostand runs a simulator instead),
scheduled jobs no (4 of 4 — "the metrics push would write preview data into
the real Enterprise desk"; "they push to the owner's phone"), own events only
(4 of 4). Nobody has staging. Werkverzeichnis has no preview isolation at all
today and calls background work in previews "unreliable at best"; zäme's
previews fall back to a dry-run mail transport by design.

### G — how they want to write it

| | Velostand | Werkverzeichnis | zäme | Enterprise |
|---|---|---|---|---|
| Consume | standard Go client | route for the Nuxt app; **the Go worker must dial out** (no ingress, on-prem) | route in the app; no worker to run | route in the app and the worker; neither has an ingress |
| Publish | — | standard client | one typed `send(name, data)` | typed client, TS and Go |
| Local dev | must | must | must, and CI-runnable; must not be a mode production can fall into | nice to have; a single binary |
| Schema | agreed JSON | **yes: one schema → TS and Go** | welcome, not needed | **welcome: TS and Go share one instance** |
| Declaration lives | repo config file | in code, beside the handler ("a dashboard-only declaration is how we lost our registrations") | in code, or a repo file; never the dashboard | in code, a `triggers` contract per department |

The shape three teams describe is the same one: **the handler is a route
the engine calls, and a typed client in TypeScript and Go declares functions
and sends events.** Nobody asked for a broker client, and the one team that
would take one has no use for a bus.

### H — between teams

Nothing now. Werkverzeichnis's daily webhook into Enterprise is the one
cross-application flow, and both sides said events would be nicer and nobody
has asked. Where a data owner answered, subscription is **by approval**
(private gatherings, multi-tenant IDs). Phase later, through the offerings
model, as the spike already said.

### I — priorities and deal-breakers

| | First use | Deal-breaker |
|---|---|---|
| Velostand | (d) singleton worker, not routed as web | a tick that runs twice or drops silently |
| Werkverzeichnis | (a) bus with parked list and backlog; (c) cron to a route | a worker that cannot dial out; registration drift that fails silently on deploy |
| zäme | (a) **if** delayed delivery can be cancelled or replaced by key; else stays on Inngest; (c) second | losing a future-dated delivery on a redeploy; an endpoint that is open when unconfigured |
| Enterprise | (b) durable workflows | losing memoisation, idempotency keys or throttles; losing the Go SDK; requiring a cloud service |

## What this decides

**Stop designing an eventing product.** The three applications that do
asynchronous work converged on one programming model without being asked,
they are in two languages that every candidate engine already has SDKs for,
and their volume is three orders of magnitude below anything a broker
choice would matter at. Their pain is operational and it is nearly all the
same list: functions silently de-registered on deploy; an unauthenticated
endpoint when a key is unset; a single replica on SQLite with state lost on
restart; a dashboard that has to be proxied by hand; no backlog view and no
parked-events list; no alert when the engine itself dies; no isolation in
previews; payload types mirrored by hand between TypeScript and Go.

Every item on that list is something the platform can own, and most of it is
independent of which engine sits underneath:

- **Keys always minted, never optional.** The self-hosted `inngest` claim
  already does this (`INNGEST_DEV=0`, keys minted per server). zäme's #89
  does not exist on the platform path; it exists because the app is not on
  it yet.
- **Registration is a condition, and a deploy checks it.** The claim's
  `AppConnected` condition already reports the function count. What is
  missing is the *diff*: a deploy that leaves fewer functions registered than
  the last release is the Werkverzeichnis incident, and it should be a
  failed deploy or a firing signal, not a missing daily push noticed a week
  later.
- **The engine's health is watched from outside it.** A row in
  `status.components` for each server; signals for *no worker connected*,
  *failed runs rising*, *server down*, delivered through the platform's own
  notification path, which does not run through the engine.
- **The screen** is the E table above: per function, backlog, in flight,
  failed runs with payload, retry and discard; per deploy, the registration
  diff; the publisher-and-consumer map from the registrations, which is the
  screen that shows an orphaned event. Read from the engine's own API with
  the signing key the platform holds — Enterprise built exactly this by hand
  and calls it the "Inngest page".
- **One schema, two languages.** An `events` block of JSON Schemas in
  `kitchen.json`, and generated TypeScript and Go types. Engine-independent,
  asked for by both bilingual teams, and it removes the only contract-drift
  risk either of them named.
- **Previews isolated by construction.** One environment's events never
  reach another's functions, and scheduled functions do not run in previews
  unless asked. The self-hosted claim gives each preview its own server for
  exactly this reason.

**What is left to decide is the engine, and it is a two-way choice**, not
the four-way one in the spike:

| | Keep Inngest, run properly | Hatchet |
|---|---|---|
| Programming model | the one all three chose; migration zero | the same shape (events trigger workflows, several can subscribe to one event; memoised steps; sleep; `waitForEvent`; cron; rate limits; concurrency; idempotency; scheduled runs that can be cancelled) — a port, not a rewrite; about 38 functions across three apps |
| SDKs | TypeScript and Go, vendor-maintained, mature | TypeScript, Go and Python, vendor-maintained; the Go SDK was rewritten in 2026 |
| Tenancy | none: a server per environment (built, v0.30: CNPG + Valkey for production, one embedded pod per preview) | tenants in one deployment, with per-tenant tokens, retention, limits and metrics |
| Delivery | serve (a route the server calls) *and* connect (a worker dials out) — the on-prem worker's requirement | workers dial out over gRPC only; a route-shaped handler is the SDK's, not the engine's — check what a Nitro app looks like as a Hatchet worker |
| Delayed delivery, cancellable by key | a sleeping run with `cancelOn` — zäme's need appears solvable in Inngest with `step.sleepUntil` plus `cancelOn` matched on the event id, rather than a future-dated signal; to confirm | scheduled runs with an id, cancellable |
| Dashboard and API | REST v1 by signing key; the UI and its GraphQL are unauthenticated and must stay unreachable | tenant-scoped REST with per-tenant tokens, made for exactly this |
| Footprint | production: server + CNPG + Valkey per project; preview: one pod | one engine + API + one CNPG for the installation |
| Licence | SSPL, Apache-2.0 after three years; fine in-house, not as a hosted service | MIT |
| Risk | the project's self-hosting is second to its cloud, and the teams' complaints are all self-hosting complaints | a younger project and a bet on its roadmap; a migration nobody asked for |

Temporal is the fallback if Hatchet fails the evaluation: it covers every
requirement here with the best SDKs in both languages and MIT, at the cost
of a heavier server and a model (workers poll; no topics; a handler is not a
route) that fits Enterprise well and the two Nuxt apps less well.

**Recommended next step:** a two-week spike that ports three real functions
to Hatchet in the cluster — Enterprise's alert remediation (idempotency,
throttle, memoised steps), zäme's invite-and-reminder pair (per-recipient
steps, a sleep of weeks that must survive a redeploy and be cancellable),
and Werkverzeichnis's split choreography (Go worker dialing out,
request-reply by `jobId`) — beside the same three on the self-hosted
`inngest` claim, with the preview isolation, the registration diff and the
screen built against whichever engine's API. The port either goes cleanly,
in which case tenancy, MIT and a tenant-scoped API decide it, or it does
not, in which case the platform runs Inngest properly and the per-environment
server is the price of the model the teams already have.

## Things to tell the teams now, whatever is decided

- **Velostand:** #250 and #256 have shipped. `singleton: true` and
  `-mode=ingest` should work; the `-mode=all` workaround can be retired once
  confirmed on the platform.
- **zäme:** the reminder for a moved event fires at the old time (the nudge
  has a rescheduled check, the reminder does not); `events/party.opened` has
  no consumer; and the cancellable delay likely exists in the engine they are
  on as a sleeping run with `cancelOn`. The unauthenticated `/api/inngest`
  is closed by the platform's claim, which always mints keys.
- **Werkverzeichnis:** the registration-diff check is the platform's to build
  and is on the list above. `training/*` has no consumer in Enterprise.
- **Enterprise:** five of the seven self-hosting workarounds (SQLite on a
  volume, `Recreate`, `INNGEST_PORT`, the proxied dashboard, the unpinned
  image) are things the `inngest` claim's self-hosted provider already does
  differently: CNPG and Valkey behind it, a pinned image, keys minted, the
  server in `kitchen-inngest`. Moving onto the claim is the first step
  regardless of the engine decision.
