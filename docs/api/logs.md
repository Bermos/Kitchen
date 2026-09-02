# Kitchen — Logs and queries

Logs are read from the telemetry store rather than from the pods, so they
outlive the pod that wrote them and can be asked questions. The one exception
is a build whose pod is still running and whose lines have not reached the
store — see [below](#a-build-while-its-pod-is-still-there).

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Logs

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
| `process` | One of the project's workers or scheduled jobs. Runtime logs only — a build's lines carry no process |
| `run` | One firing of a scheduled job, by the Job's name |

Lines come back oldest first — a log reads forwards — as
`{timestamp, source, project, environment, build, process, run, pod, container, stream, level, message, fields}`.
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

### A build, while its pod is still there

`GET /builds/{name}/logs` is the one read with a second source behind it. The
telemetry store answers first and is the source of record — it is what makes a
build's output outlive its pod — but when it has *no* lines for the build and
the build's pod is still in the cluster, the pod is read directly.

This is not a preference for the pod; it is the case where the store's answer
is wrong. The collector is a DaemonSet, and a DaemonSet whose pods are refused
at admission has no pods at all: it looks healthy and files nothing. A build
that fails in its first seconds can also finish before its first line has been
shipped. Either way the build that most needs a log is the one that has none,
and the pod has it.

The lines are the same shape and the same order, so nothing distinguishes them
on the wire, and `--follow`/`text/event-stream` tails both the same way. The
container read is the builder, or the clone while the builder has not started —
a build pod's two containers are two steps, and interleaving them by timestamp
would produce a log that is neither. Once the job's TTL collects the pod there
is only the store, which is the normal state of every finished build.

`cluster` lines are collected deliberately: a node whose storage or networking
is failing is exactly when Kitchen looks broken, and the answer is in someone
else's pod. The dashboard scopes them out by default and offers a switch.

All four are containers. The node's own system logs are deliberately not
collected — see [SCOPE.md](../SCOPE.md) for why the collector cannot read a
journal it has no `journalctl` for, and why Talos has none to read.

An installation without a telemetry store answers `503`: there are no logs to
read, which is a missing capability rather than a bad request.

## Following logs live

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

## Querying logs

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

### The query language

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

Columns are `source`, `project`, `environment`, `build`, `process`, `run`,
`namespace`, `pod`, `container`, `node`, `stream`, `level`, `message`,
`traceId` and `spanId`, plus
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

`process` and `run` are a project's workloads besides its web one (see
[Workloads](processes.md)). `run` is the Job a run produced — one firing of a
schedule, or the one run a deploy task makes for a deploy — which is what makes
"show me what last night's report job printed", or "show me why the migration
that stopped this deploy failed", one query rather than a hunt through an
environment's whole output, and it stays answerable long after the platform has
collected the Job itself.
The web process writes neither, so an environment's logs mean what they always
meant.

Anything that is not a column is a **structured field** of the line, so
`http.status:500` reads `LogAttributes['http.status']`. `labels.tier:web`
reaches the pod's Kubernetes labels instead. This is the one place a typo goes
quiet: `levl:error` asks for a field nothing writes and matches nothing rather
than being refused.

Every value travels to ClickHouse as a bound parameter, never as query text.

### The ClickHouse escape hatch

`where` is a real ClickHouse expression over the table's columns, evaluated as
written — the query language is a front door, not a cage:

```
GET /logs?where=match(Body, 'GET /works\?page=\d+') AND environment = 'shop-production'
```

Its vocabulary is `otel_logs`'s own, which is the price of a store any
OTel-shaped tool can read: `Body`, `SeverityText`, `LogAttributes['…']` where
the query language says `message`, `level` and a field name. Kitchen's own
columns — `project`, `environment`, `build`, `process`, `run`, `source`,
`namespace`, `pod`, `container`, `node` — are real columns here and mean what they mean everywhere
else, because they are what the table is ordered by.

It reaches ClickHouse as query text, which is the point — and why it runs pinned
read-only (`readonly=2`: no writes, no DDL) under an execution cap, as the
operator's own database user. What that user can read is the whole telemetry
database, so a cross-project read is narrowed by the API before it runs: a
caller who is not an operator has `project IN (…their own…)` composed onto
their selection with `AND`, which is why a `where` cannot reach another
project's lines — and why the narrowing goes into the query rather than over
the answer, so a page is a page of the caller's own lines.

A query either side refuses — a bracket that never closes, an unknown column —
answers `400` carrying the diagnostic that says how to fix it: Kitchen's parser
for `q`, ClickHouse's own for `where`.

### The histogram

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

### Facets

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

### Patterns

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

## Saved queries

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

Saved queries are shared by everyone on the platform, but **a query that names
a project the reader cannot see is not listed to them, and deleting it answers
the same `404` as a name nobody ever saved** — a refusal that differed would
confirm the existence of the project the query names. The check errs towards
hiding: a query mentioning such a name anywhere in its selection, title or
description is withheld, and its results would have been narrowed to nothing
for that reader anyway.

**They are shared *and unowned*, which is a decision and not an oversight.**
Any account may save one, and any account may delete one it can be shown — a
query naming no project, "Platform 5xx" with a bare `where` clause, is
therefore deletable by anybody with a token. `savedBy` is a byline, not an
owner: it is the caller as the API knew them at the time, an address that
changes when the account's does, so enforcing against it would take a role
away from the person it was recorded for. Making a saved query owned means
recording the issuer's `sub` on the object and letting its author or an
operator delete it — a field on the CRD and a migration for the queries
already saved, which is a bigger change than the risk (a shared shortcut
nobody else can read anything through) is worth. The list is capped at 100 and
every deletion is the platform's to see.
