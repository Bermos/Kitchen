# Kitchen — Metrics, traffic and traces

The three views onto the same telemetry store: what an environment is doing
in aggregate, the requests that make up the aggregate, and the spans within a
request.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Metrics

`GET /metrics/overview` answers the dashboard's numbers pre-aggregated, in
one shape:

- deploys over 7 days and a per-day series, plus the median build time, from
  the activity feed
- requests, error rate and p95 latency over 24 hours with per-hour series,
  from the request pipeline
- log volume over 24 hours with a per-hour series
- the store's own size and ingest rate
- `projects`: per-project 24h traffic (requests, 5xx, p95, hourly series),
  from the same request pipeline

Every traffic number here leaves out the platform's own health checks: a
project's declared health path (`spec.runtime.health.path`) is a probe rather
than a visit, and counting it makes a quiet project look visited. The exclusion
is a (project, route) pair, so one project's health path never subtracts from
another's traffic, and it applies to the totals, their hourly buckets and the
`projects` rows alike — the environment page's numbers are the same kind of
number ([environments](environments.md#the-platforms-own-health-checks-are-not-traffic)
says what counts as one, and how to ask for them back). The platform's *edge*
view is the deliberate exception: `/platform/edge` is about everything that
crossed the edge, probes and scanners included.

Every traffic number here is the edge's request rows — the totals as well as
the rows, which was not always true. Flows are attributed by the *destination*
endpoint: a protected preview's traffic is credited to the forward-auth gate
and an idling environment's to the KEDA interceptor, both of which live in the
platform's own namespace, so both vanished from the project that served them
and swelled the platform's own numbers instead. A request row is attributed by
the `Host` header, which is the one thing every hop preserves and the only
thing the interceptor routes on. Every project gets a row, at zero if nobody
visited it: this is a list of projects with numbers on it, not a list of
numbers.

The totals are read across projects rather than added up from the rows below
them, because a p95 does not add up: the mean of twenty projects' p95s is not
the platform's p95, and neither is the largest of them. The percentile has to
be merged from the stored aggregate states, which is a read of its own — and
the per-hour series is a read per hour, for the same reason applied to each
bucket.

`?project=` narrows everything to one project, drops the `projects` join, and
answers the same numbers off that project's own rollups.

**Without `?project=`, "everything" means everything of the caller's.** An
operator is answered about the platform. Anybody else is answered about the
projects they hold a role on, which is one project-scoped read per project,
added together:

- counts add, and so do their hourly and daily buckets;
- `errorRate24h` is recomputed from the counts it is a ratio of, so it is
  total errors over total requests rather than a mean of rates;
- `p95Ms24h`, `p95MsPerHour` and `medianBuildSeconds` **do not merge**, for
  the reason above — a mean of p95s is not a p95. Over several projects they
  come back as `0`, which the dashboard renders as "—"; each project's own
  honest p95 is still in `projects`. Over exactly one project there is nothing
  to merge and both are that project's own. Answering them across a set of
  projects needs the store's queries to take one, which they do not yet;
- `storeBytes` and `storeRowsPerSecond` are the telemetry store's own figures
  rather than any project's, and are reported as the store gives them — the
  same numbers `?project=` already answers with.

A caller who holds no project at all gets every number as `0`, and the store is
not asked. Those zeroes are honest rather than withheld: they are *their*
numbers, and they have none.

There is deliberately
no raw metrics query surface: the raw material is the logs, events and request
tables, and `/logs` already exposes the store's own syntax for ad-hoc
questions.

## Traffic

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

## Traces

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
OTLP/HTTP endpoint — see [SCOPE.md](../SCOPE.md) for why nothing here is derived
from the flow data. Every environment the operator deploys is handed that
address through OTLP's own environment variables, so instrumenting an
application is adding its language's SDK and nothing else. Nothing about that
changed when the receiver moved out of the operator and into the collector:
same Service name, same port. The Service is `internalTrafficPolicy: Local`
now, so one stable name means the agent on the caller's own node — and on a
node where no agent is Ready, an export is dropped rather than sent to
another.
