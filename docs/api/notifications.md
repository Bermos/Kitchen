# Kitchen — Notifications

A build that fails at 02:00 is found by whoever opens the dashboard next.
These endpoints are how somebody is told instead: a **subscription** says
which events go to which address, and every matching event becomes a
**delivery** — one signed POST, retried a bounded number of times, and kept as
a dead letter when the receiver never took it.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## The shape of the feature

The platform ships **one payload shape, signed**, and nothing else. There is no
Slack app, no Discord webhook format and no Teams card here on purpose: each of
those is forty lines of somebody else's code in front of this — a *relay* — and
a relay somebody wrote in an afternoon does not go out of date when the vendor
changes its card schema.

Two objects sit behind it, and the split is a requirement rather than a
preference (the reasoning is on the types in
[CRDs](../CRDS.md#notificationsubscription-namespaced-kitchen-system)):

- a `NotificationSubscription` is configuration — where, what, whose;
- a `NotificationDelivery` is one event on its way to one subscription, in etcd
  before the first attempt is made, which is what makes at-least-once survive
  the operator pod moving.

**A notification that fails never touches what it reports on.** Deciding to
notify is one object creation, off the reconcile's own goroutine; every
attempt, failure and dead letter after that is written on the delivery and
nowhere else.

## Scope decides who may write it

A subscription **naming a project** carries that project's events, and writing
one is its `admin`'s: it sends the project's activity to an address of
somebody's choosing, and it carries a credential. A subscription **naming no
project** hears every project's events — that is the platform scope, and only
an operator may create one. A member who is not an operator is not shown that
platform subscriptions exist: they name no project, so there is no project of
that member's they could be about.

The scope cannot be patched. Moving a subscription between a project and the
platform would change who may write it, which would make one route two
requirements; delete it and create the one you meant.

## The events

| Event | When |
| --- | --- |
| `deploy.succeeded` | A release went live on an environment — by promotion, by auto-deploy on a push, or by rollback. All three are one event because all three are one fact: what is serving changed |
| `build.failed` | A build ended in failure |
| `environment.unhealthy` | An environment stopped being healthy without anybody deploying anything: a deploy task that failed, or a workload that was available and stopped being available while running the release it already had |
| `preview.created` | A preview environment was published for a pull request |
| `preview.destroyed` | That preview went away again |
| `alert.firing` | A [saved query](logs.md) with an alert crossed its threshold |

`events` is required and an empty list is refused. A subscription with no
events is not one that hears everything — it is one that would start hearing
whatever the platform learns next, which is how an upgrade comes to page
somebody at 03:00.

Every one of these is also in [the activity feed](audit.md), which is where it
comes from: notifications are a second reader of the feed rather than a second
set of announcements, so *what was I told about, and why* is answerable from
one place.

## Subscriptions

### `GET /notifications/subscriptions`

Every subscription the caller may see, name-ordered. `?project=` narrows to
one. A project's members see that project's; an operator sees those and the
platform's.

```json
{"items": [{"name": "shop-relay", "url": "https://relay.example.com/kitchen",
            "events": ["deploy.succeeded", "build.failed"],
            "project": "shop", "scope": "project",
            "description": "into #shop-deploys", "suspended": false,
            "maxAttempts": 5, "timeoutSeconds": 10,
            "createdBy": "grace@example.com", "createdAt": "2026-09-01T09:00:00Z",
            "ready": true,
            "delivered": 412, "failed": 3, "deadLettered": 0,
            "lastResult": "delivered", "lastDeliveryAt": "2026-09-04T01:59:12Z",
            "lastStatusCode": 204}]}
```

There is no field for the signing key, here or on any other route.

### `POST /notifications/subscriptions`

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-relay", "project": "shop",
       "url": "https://relay.example.com/kitchen",
       "events": ["deploy.succeeded", "build.failed"],
       "secret": "'"$(openssl rand -hex 32)"'"}' \
  https://kitchen.apps.example.com/api/v1/notifications/subscriptions
```

`name` is a DNS label. `url` must be absolute and `https` — a signed payload
over plain HTTP is one anybody on the path can read, and the signature proves
only that it was not changed on the way. `maxAttempts` (1–10, default 5) and
`timeoutSeconds` (1–30, default 10) bound the retry ladder and one attempt.
Omitting `project` asks for the platform scope and is refused for anybody but
an operator.

**`secret` is supplied by the caller and never comes back.** It is at least 16
characters, and the platform writes it into a Secret it owns and manages; no
response echoes it, and rotating it is another write of a value that also never
comes back. The platform does not generate one for you, because a generated key
answered once would live in a shell history, a browser's memory and whatever
logged the response — to save the caller an `openssl rand -hex 32`.

`201` with the subscription. A name already taken is `409`.

### `GET /notifications/subscriptions/{name}`

One subscription, the same view. `viewer` on its project; a platform
subscription is an operator's and answers `404` to anybody else.

### `PATCH /notifications/subscriptions/{name}`

Changes `url`, `events`, `description`, `suspended`, `maxAttempts`,
`timeoutSeconds` or `secret` — send only what is changing, and at least one of
them. Rotating the key is `{"secret": "…"}`, and the new key applies to the
next attempt of every delivery, including ones already queued.

`suspended: true` pauses without deleting anything: nothing new is queued while
it is set, and what is already queued waits rather than being dropped — the
switch is for a receiver being repaired, and the deploy it was silenced during
is still worth hearing about afterwards.

### `DELETE /notifications/subscriptions/{name}`

`204`. The signing key and the whole delivery history go with it — the Secret
is deleted on the spot rather than left to the collector, because it is a
credential.

## Deliveries, and the dead letters among them

### `GET /notifications/deliveries`

Every delivery the caller may see, newest first, capped at 200.
`?subscription=` narrows to one, `?phase=` to `Pending`, `Delivered` or
`DeadLettered`. A delivery is visible exactly when its subscription is.

```json
{"items": [{"name": "shop-relay-8f2c1", "subscription": "shop-relay",
            "event": "build.failed", "eventId": "9f1c…", "project": "shop",
            "phase": "DeadLettered", "attempts": 5,
            "queuedAt": "2026-09-04T02:00:00Z", "completedAt": "2026-09-04T02:09:41Z",
            "lastStatusCode": 502, "lastError": "receiver answered 502 Bad Gateway",
            "payload": "{\"version\":\"v1\",…}",
            "attempted": [{"number": 1, "at": "2026-09-04T02:00:00Z",
                           "statusCode": 502, "error": "receiver answered 502 Bad Gateway",
                           "durationMillis": 41}]}]}
```

The dead letter carries the **payload it would have sent**, which is what makes
it something a person can act on rather than only read. It holds nothing
secret: it is the body, and the signature is not part of it.

### `POST /notifications/deliveries/{name}/retry`

Puts a dead letter back on the queue — `202`, and the delivery. Only a dead
letter can be retried; anything else is `400`. The payload and the event id are
unchanged, so a receiver that did get it de-duplicates on the id.

`admin` on the subscription's project: re-sending puts the project's activity
offsite a second time.

## Retention

A delivered notification is kept an hour — the record of what happened is the
activity feed, and a success is interesting only while somebody is watching it
happen. A dead letter is kept seven days, because it is the thing somebody has
to find, and they find it on Monday. A subscription keeps at most 200
deliveries; past that the oldest *finished* ones are deleted early. Nothing
still pending is ever dropped.

## The payload

Every attempt POSTs `application/json`, the same bytes every time — the
delivery holds them verbatim, which is what makes the signature reproducible
across attempts.

```json
{
  "version": "v1",
  "id": "9f1c0b7e5d3a4f628a1c0d9e8b7a6f54",
  "type": "deploy.succeeded",
  "occurredAt": "2026-09-04T01:59:12Z",
  "subscription": "shop-relay",
  "project": "shop",
  "environment": "shop-production",
  "build": "shop-build-000142",
  "release": "shop-release-000141",
  "message": "release shop-release-000141 is live on shop-production",
  "actor": "grace@example.com",
  "value": 0
}
```

| Field | Always | Meaning |
| --- | --- | --- |
| `version` | yes | The payload version, `v1`. Fields may be **added** under a version; nothing is removed or given a new meaning without it changing |
| `id` | yes | The event's id, and the receiver's **idempotency key**. Every attempt of a delivery carries the same one, and so does the same event sent to a second subscription. Delivery is at-least-once: a receiver will see a repeat, and de-duplicating on this id is how it stays correct |
| `type` | yes | One of the events above |
| `occurredAt` | yes | When the platform recorded the event, RFC 3339 in UTC. Not when the request was made — that is the timestamp header |
| `subscription` | yes | Which subscription this was sent to, so a relay fronting several can tell which one is talking |
| `project` | no | Empty for a platform event, which names no project |
| `environment`, `build`, `release` | no | Present when the event is about one. A build failure names no release |
| `message` | no | The same sentence the activity feed shows |
| `actor` | no | Who caused it: an authenticated caller by name, or `operator` for what the platform decided on its own |
| `value` | no | The one number some events carry — a finished build's duration in seconds, an alert's count |

The headers describe the delivery:

| Header | |
| --- | --- |
| `X-Kitchen-Event` | The event type, so a receiver can route before it parses |
| `X-Kitchen-Event-Id` | The same id as `id` in the body |
| `X-Kitchen-Delivery` | The delivery's name, which is what the dashboard lists it under |
| `X-Kitchen-Attempt` | 1 for the first attempt, and up from there |
| `X-Kitchen-Timestamp` | Unix seconds, and **part of the signature** |
| `X-Kitchen-Signature` | `v1=<hex>` — below |

## Verifying the signature

`X-Kitchen-Signature` is `v1=` followed by the hex HMAC-SHA256, keyed with the
subscription's secret, over the string

```
v1:<X-Kitchen-Timestamp>:<the request body, byte for byte>
```

Three things in that a receiver author would otherwise have to guess at:

- **The timestamp is inside the signed string**, so a captured request cannot
  be replayed a week later against a receiver that checks how old it is. Reject
  anything older than a few minutes; the window is yours, because only you know
  how far your clock can be from the platform's.
- **The scheme is named in the value**, so a second scheme can one day be sent
  alongside the first rather than instead of it. Match on the `v1=` prefix
  rather than assuming the whole value is a hash.
- **The body is signed as bytes, not as JSON.** Nothing re-serializes the
  payload between attempts, so verify *before* you parse — which is the order
  that keeps a malformed body from being parsed for an unauthenticated caller.

Compare in constant time. A receiver that compares with `==` leaks the correct
signature a byte at a time to anybody willing to send a few million requests.

```python
import hashlib, hmac, time

def verify(secret: bytes, headers, body: bytes) -> bool:
    signature = headers.get("X-Kitchen-Signature", "")
    timestamp = headers.get("X-Kitchen-Timestamp", "")
    scheme, _, digest = signature.partition("=")
    if scheme != "v1" or not timestamp.isdigit():
        return False
    if abs(time.time() - int(timestamp)) > 300:      # your replay window
        return False
    signed = b"v1:" + timestamp.encode() + b":" + body
    expected = hmac.new(secret, signed, hashlib.sha256).hexdigest()
    return hmac.compare_digest(digest, expected)
```

The operator's own implementation, and the one the tests check, is `Sign` and
`Verify` in `internal/notify/signature.go`.

**Answer `2xx` and answer it quickly.** Anything else — any other status, a
connection that never opens, a response that takes longer than
`timeoutSeconds` — is a failed attempt. Do the work after you have answered:
a receiver that relays to a chat vendor before replying is a receiver whose
vendor's bad afternoon becomes a dead letter here.

## Retries, and what a failure costs

An attempt that fails is retried after 10 seconds, then 20, 40, 80… capped at
ten minutes, until `maxAttempts` is reached. The attempt that exhausts the
ladder dead-letters the delivery. Five attempts — the default — are four waits
of 10s, 20s, 40s and 80s: two and a half minutes, which is a receiver being
restarted or redeployed. It is deliberately not hours, because a notification
that arrives long after the deploy it reports on is worse than a dead letter;
nobody reads it as history.

Two failures are **not** the receiver's and do not consume an attempt: a
subscription that is suspended, and a signing key that is missing or empty. The
delivery waits, and the subscription's `ready` says which it is.

Redirects are not followed. A subscription's URL is where somebody agreed to
send this project's activity, and a receiver answering `302` has changed that
agreement without anybody's say-so.

## Alerts: the second trigger

An `alert.firing` event comes from a [saved query](logs.md) with an `alert` on
it: a threshold over a window, evaluated on a schedule. It feeds this same
delivery path and adds nothing to it — crossing the threshold records an
activity event exactly the way a reconciler records a deploy, and the
subscriptions do the rest.

It is **edge-triggered**: the message is sent on the transition into firing. A
threshold that stays crossed all afternoon is one message, not one every five
minutes. `status.firing`, `status.lastCount` and `status.message` on the
SavedQuery are what a person reads while it stays crossed, and
[CRDs](../CRDS.md#savedquery-namespaced-kitchen-system) carries the fields.

## From the terminal

There is no `kitchen notifications` command. Subscriptions are set up once and
looked at rarely, and `kitchen api` reaches every route here authenticated:

```sh
kitchen api GET /notifications/deliveries?phase=DeadLettered
kitchen api POST /notifications/deliveries/shop-relay-8f2c1/retry
```

See [the CLI](../CLI.md#kitchen-api).
