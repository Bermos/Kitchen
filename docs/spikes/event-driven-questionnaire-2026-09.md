# Asynchronous work on the platform — a questionnaire for teams

*September 2026. Companion to [the event-driven spike](event-driven-2026-09.md).
It exists to find out what teams actually need before the platform commits to
a shape. Fill it in per team, or per application where a team runs several
that differ. It takes about 45 minutes; the best results come from doing it
as a conversation with someone from the platform side taking notes.*

## Why we are asking

The platform is going to offer a first-class way to do work *outside the
request*: things that happen after an HTTP request has been answered, on a
schedule, in reaction to something another service did, or over minutes and
days rather than milliseconds. Today that means a worker process and a cron
process you supervise yourself, or a queue you bring along.

What we build depends on what that work looks like for you. Two very
different families of tools exist, and they are not interchangeable:

- **An event bus.** You publish "an order was placed" and other code, in your
  service or somebody else's, reacts to it. The platform delivers it, retries
  it, and parks it somewhere visible if it keeps failing. Simple, fast, and
  the right tool for most background work and for services talking to each
  other.
- **A durable workflow engine.** You write a function with several steps —
  charge the card, wait three days, send the reminder, cancel if unpaid — and
  the engine makes sure it finishes even if the process crashes halfway,
  remembering which steps already ran. Heavier, and the right tool when the
  work is a *process* rather than a *reaction*.

Most teams need the first. Some need the second. A few need both. We would
rather find out than guess, and we would rather build one thing well than two
things half. **You are not being asked to choose a technology.** Describe the
work; that is the whole ask.

How to read the questions: where there are boxes, tick as many as apply. Where
there is a table, one row per thing. Where you do not know a number, an order
of magnitude is fine. Where a question does not apply, say so and move on.

---

## A. Your team and your applications

1. Team name, and the applications this answer covers.
2. Languages and frameworks in those applications, in order of how much code
   is in each. (Backend and any worker code. Front-end frameworks matter less
   here.)
3. How many separately deployed services make up the application(s)?
4. Which of these do you run **today**, on the platform or anywhere else?

   - [ ] A worker process consuming from a queue
   - [ ] Scheduled jobs (cron)
   - [ ] A message broker (which: Kafka, RabbitMQ, NATS, Redis, SQS, Pub/Sub, other)
   - [ ] A background-job or workflow product (which: Inngest, Temporal, Trigger.dev, Sidekiq, Celery, BullMQ, Hangfire, Quartz, other)
   - [ ] Database-table-as-queue (a `jobs` table something polls)
   - [ ] Webhooks you receive from third parties
   - [ ] Webhooks you send to third parties
   - [ ] None of the above — everything happens inside the request

5. For whatever you ticked: what do you like about it, and what have you had
   to work around? A sentence each is enough.

## B. The asynchronous work you have, or want

The most useful part. One row per piece of work that runs outside a request,
whether it exists today or you have been meaning to build it. Examples of
the kind of thing that belongs here: sending a welcome email, resizing an
upload, recalculating a report nightly, syncing to a CRM, updating a search
index when a record changes, reminding a customer three days after an
invoice, retrying a failed payment, cleaning up expired sessions.

| # | What it does | What starts it | Runs for | How often | If it fails | Exists today? |
|---|---|---|---|---|---|---|
| 1 | *send welcome email* | *user signed up* | *< 1 s* | *200/day* | *retry, then give up quietly* | *yes* |
| 2 | *invoice reminder* | *invoice issued, then 3 days pass* | *seconds, after a 3-day wait* | *50/day* | *must not be lost; must not send twice* | *no — done by hand* |
| 3 | | | | | | |

Column notes:

- **What starts it** — a user action, another service's event, a clock, a
  third-party webhook, a person pressing a button, a deploy.
- **Runs for** — the handler's own running time, not counting any waiting.
- **If it fails** — what *should* happen: retry until it works; retry a few
  times then alert; retry then park it for someone to look at; drop it; run
  compensating work (undo the earlier steps). Say what the cost of a lost or
  duplicated run is: nothing, a support ticket, money, a legal problem.

## C. Patterns

For each pattern: **T** you do this today, **N** you need it and cannot do it
well today, **L** would be nice later, **–** not for you. Plain-language
descriptions follow each one; tick by the description, not the name.

| Pattern | T | N | L | – |
|---|---|---|---|---|
| **Fire and forget.** Hand off a piece of work and go back to answering the request. | | | | |
| **Scheduled.** Something runs at a time or interval. | | | | |
| **Fan-out.** One thing happens, several independent pieces of code react to it — possibly in different services. | | | | |
| **Cross-team.** Another team's service publishes something yours reacts to, or the other way round. | | | | |
| **Multi-step, must finish.** A sequence of steps where a crash halfway must resume, not restart from the top, and steps already done must not run again. | | | | |
| **Long waits.** Wait hours or days between steps without a process sitting around for it. | | | | |
| **Wait for something external.** Pause until a webhook arrives, a person approves, a payment settles. | | | | |
| **In order, per thing.** Events about the same order (or user, or account) must be processed in the order they happened. | | | | |
| **Exactly once, in effect.** Running the same handler twice for the same event would be harmful, so it must be prevented or made harmless. | | | | |
| **Rate limited.** At most N per second against some third party, however many events arrive. | | | | |
| **Delayed.** Do this, but not before a given time. | | | | |
| **Replay.** Re-run past events against a new consumer, or after fixing a bug. | | | | |
| **Batch.** Process events in groups rather than one at a time. | | | | |
| **Request-reply over messaging.** Ask another service something asynchronously and wait for its answer. | | | | |
| **High volume.** Thousands per second, sustained. | | | | |

If you ticked **Multi-step, must finish**, **Long waits** or **Wait for
something external**, describe one real case in a few sentences. These three
are what decides whether a workflow engine is needed at all.

## D. Volume, latency, size

1. Events or jobs per day for the application, today and in a year. Peak
   rate if it is spiky (end of month, a campaign, a batch import).
2. Typical payload size, and the largest you can imagine (a row of fields;
   a document; a file — files should go to object storage with a reference
   in the event, so say if that is what you would do).
3. How quickly must a reaction happen after the event? Milliseconds, a
   second, a minute, "before tomorrow".
4. How long must the platform keep an event after it was delivered? Only
   until processed; a day, for debugging; a month, for replay; forever.

## E. When it fails

1. Think of the last time a background job or a consumer broke. How did you
   find out, how long did it take, and what did you have to do to fix it?
2. What do you want to *see* when something is failing? Tick what matters:

   - [ ] How far behind a consumer is (backlog, oldest unprocessed event)
   - [ ] How many events are being retried right now
   - [ ] The events that gave up, with their payload, and a button to retry or discard them
   - [ ] The error the handler returned, per event
   - [ ] A trace from the request that published the event through to the handler that processed it
   - [ ] Which events a deploy caused to fail (before/after)
   - [ ] Throughput and processing time per subscription
   - [ ] A picture of which service publishes what and who consumes it

3. Who should be told, and how, when a subscription is falling behind or
   events are being parked? The same channel as your other alerts, or
   somewhere else?
4. Do you use distributed tracing today (OpenTelemetry or similar)? If yes,
   what do you use it for; if no, is it something you have wanted?

## F. Environments and previews

Every pull request gets a preview environment. The question is what
background work should do there.

1. Should consumers run in preview environments by default? (Consider: a
   preview that reacts to production's events is a bug; a preview that
   reacts to nothing cannot be tested.)
2. Should scheduled jobs run in previews? The nightly report that emails
   customers, for instance.
3. When a preview needs events to react to, where should they come from? The
   preview's own publishes only; a synthetic seed; a copy of recent
   production events; something else.
4. Does anything about staging differ from production here?

## G. How you want to write it

1. Given the languages you named in A2: would you rather use
   - [ ] the standard client library for whatever the platform runs, in your language (as you would use a Postgres driver)
   - [ ] plain HTTP — you publish by calling a URL, and your handler is an ordinary route in your web framework that the platform calls
   - [ ] a framework-specific package that hides all of it
   - [ ] no preference, as long as it is documented and typed

   Say why, if you have a reason. If you would answer differently for
   publishing and for consuming, say that too.
2. How important is running the whole thing on a laptop with no cluster
   (`docker compose up`, or a single binary), on a scale from "must" to
   "never do that"?
3. Do you want event payloads to have a schema the platform checks and
   generates types from, or are you happy with JSON you agree on?
4. Where should the declaration of "this service subscribes to X" live? In a
   config file in the repository beside the code; in the dashboard; in the
   code itself; no opinion.
5. If you have used one of these — Inngest, Temporal, Trigger.dev, Kafka,
   RabbitMQ, SQS, Pub/Sub, BullMQ, Sidekiq, Celery, Cloudflare Queues,
   Vercel Queues — what was the single best thing about it, and the single
   worst?

## H. Between teams

1. Do you consume events from another team today, or want to? Which ones?
2. Do others consume yours, or would they want to?
3. When another team changes the shape of an event you consume, how do you
   want to find out? Before it ships (a contract, a version); when it ships
   (a changelog); when it breaks (never again, please).
4. Should another team be able to subscribe to your events without asking,
   or only with your approval?

## I. Priorities

1. If one of these existed next week, which would you use first, and for
   what?
   - (a) publish an event, subscribe to it from any service, with retries, a
     parked-events list and a backlog graph
   - (b) a multi-step workflow that survives crashes, can sleep for days and
     wait for external events
   - (c) scheduled jobs that call a route in your running application
     instead of starting a separate process
   - (d) something else — say what
2. What are you *not* building today because none of this exists?
3. What is the one thing that, if the platform got it wrong, would make you
   keep running your own?

## J. Anything else

Anything the questions above did not let you say.

---

## For the platform side: reading the answers

Not for the teams; kept here so the interpretation is written down before the
answers arrive.

- **B and C decide the family.** Count the rows in B whose "if it fails"
  says *must not be lost* or *must not run twice*, and the ticks under
  *Multi-step*, *Long waits* and *Wait for something external*. Rows that are
  short handlers with a retry-then-alert policy are bus work. Rows that are
  processes spanning days with compensating steps are engine work. If the
  second group is small or empty across every team, no engine is built now,
  whatever anyone said they would like.
- **A2 and G1 decide the client story.** If every language named has a
  maintained client for the substrate and teams prefer the standard client,
  no platform library of any kind is needed. A team preferring plain HTTP is
  the push-delivery shape, which also gives scale to zero. A team asking for
  a framework package is asking for something the platform should not own,
  and the answer is the standard client plus documentation.
- **D decides the broker tier**, and whether any team is outside what one
  shared broker serves. Thousands per second sustained, or events kept
  forever, is a conversation, not a default.
- **E is the screen.** Whatever is ticked most is what the environment page
  shows first. The trace question tells us whether push delivery's
  through-the-queue trace is worth its per-event cost to that team.
- **F sets the preview defaults.** Expect "consumers yes, cron no, own
  events only"; anything else is a per-project setting.
- **H decides whether cross-project events are phase one or later**, and
  whether an offering needs approval (the existing `offers` model) or is
  open.
- **I1 is the tiebreaker**, and I3 is the list of things to get right first.
