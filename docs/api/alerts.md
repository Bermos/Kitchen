# Kitchen — Alerts

**An alert's tier belongs to the pair (condition, audience).** A node going
NotReady is the operator's to act on now and not even a word a developer has; a
failed dependency install is the developer's to fix and a data point about queue
load for the operator. One flag saying who *sees* a finding cannot express
either, so every rule in the signal catalogue declares a tier per audience, and
these endpoints are where that model is read and acted on.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table. What the rules themselves find is
[`GET /platform/signals` and `GET /environments/{name}/signals`](platform.md);
this page is the same conditions asked a different question — *and what am I
meant to do about it*.

## The vocabulary

A **delivery** is one condition delivered to one audience: the pair
`(fingerprint, audience)`. It is the unit here, and it is not the finding. A
developer-audience condition is delivered twice — to the project and to the
operator — at two tiers, in two vocabularies, and the two are acknowledged and
silenced separately.

A **tier** is what that audience is meant to *do*:

| Tier | Means | In the dashboard |
|---|---|---|
| `page` | Act now: urgent, actionable by this reader, user-visible | **Act now** |
| `ticket` | Act in hours: a fix is owed and nobody needs waking | **Needs a fix** |
| `log` | A data point. Appears on the list it belongs to and notifies nobody | **For the record** |

The dashboard does not use the word *page*, deliberately: there is a delivery
mechanism but nothing in it wakes a person, and the word will be used when
something behind it actually pages. The API keeps it because the model is the
industry's.

A **mitigation** is a record about a delivery — an acknowledgement, a silence,
a claim. It is a classification's companion, **not a ticket's status field**:
nothing here transitions, nothing is closed, and a condition resolves when the
condition stops being true. That line is what keeps a later hand-off to GitLab,
ServiceNow or PagerDuty additive rather than a rewrite.

## What each reader sees

`GET /alerts` is answered for anybody with a token, and filtered:

- a **member** gets their projects' developer deliveries, plus **symptom rows**
  — a platform condition degrading one of their projects, said in their own
  words with no Kubernetes noun, nothing to press and no silence;
- an **operator** gets every delivery of both audiences, and therefore no
  symptom rows: they have the condition itself, and a derived restatement of a
  row already in the list would be the same fact twice.

```sh
curl -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/alerts
```

`?project=` narrows to one project — the project's own Alerts screen. A project
the caller may not see answers with an empty list rather than a refusal, like
every other read here.

```json
{
  "items": [
    {
      "signal": "workload.crashloop",
      "severity": "critical",
      "scope": { "kind": "environment", "project": "shop", "environment": "shop-production" },
      "audience": "developer",
      "tier": "ticket",
      "baseTier": "page",
      "fingerprint": "workload.crashloop/shop/shop-production/web",
      "title": "web is crash-looping",
      "detail": "12 restarts in 30m",
      "evidence": "/environments/shop-production",
      "since": "2026-04-01T11:40:00Z",
      "openedAt": "2026-04-01T11:41:00Z",
      "actionable": true,
      "mitigation": {
        "acknowledged": true,
        "acknowledgedBy": "ada@example.com",
        "acknowledgedAt": "2026-04-01T11:52:00Z",
        "acknowledgedVia": "explicit"
      }
    }
  ],
  "counts": { "page": 0, "ticket": 1, "log": 0, "untended": 0 },
  "source": "recorded",
  "evaluatedAt": "2026-04-01T12:00:00Z"
}
```

`tier` is what this reader reads it at **now**, after everything below; `baseTier`
is what the rule declared. They differ exactly when something has happened —
which is what lets a screen say *a page, held down to a ticket because somebody
is on it* rather than only the answer.

`source` is `recorded` when the answer comes from the background evaluation
loop's history, and `evaluated` when it does not. The second is honest and
thin: the conditions are right and nothing carries an age, because how long
something has been true is the one thing an evaluator that runs when somebody
looks cannot know. Rows in an evaluated answer are not `actionable`, and
`message` says so.

## How a tier is arrived at

Four rules, in this order, each reading the answer of the one before:

1. **The owner is the developer where there is a developer delivery**, and the
   operator otherwise.
2. **Mitigation lowers.** A condition somebody is acting on is a ticket for its
   owner — a fix is still owed — and a log for everybody else.
3. **A silence lowers to log, for that delivery alone.** A member silencing
   their project's row leaves the operator's exactly where it was. What a
   silence *does* reach across is the clock: somebody wrote down a reason and
   an expiry, which is a stronger statement than an acknowledgement, so nothing
   escalates underneath it.
4. **Escalation raises the operator, never the owner.** Nothing is more broken
   at hour four than at hour one, so an unacknowledged condition past the
   window does not become a page: it repeats, and *adds the operator as a
   ticket*, carrying the sentence that says why —
   `shop / production · unmitigated 4h 12m · nobody has acknowledged`. Past a
   multiple of the same window it becomes an untended-incident line on
   [the compliance posture](../API.md), with names on it.

The two windows are constants — one hour, and four of them — chosen once and in
one place. The base tier is code, versioned with the catalogue; the *clock* is
what an installation will configure ([#472](https://github.com/Bermos/Kitchen/issues/472)).

## Acknowledging

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/alerts/ack \
  -d '{"fingerprint": "workload.crashloop/shop/shop-production/web", "audience": "developer"}'
```

`200` with the delivery as it now reads. An acknowledgement commits to no fix —
that is the point of shipping it, since *somebody is looking* needs to be
sayable without promising anything — and it is what stops the escalation clock.

**The audience decides who may write**, and that is the whole authorization
rule: a project's member acts on their project's delivery, an operator on the
operator's. A member acknowledging the operator's row about the same condition
is a `403`, because escalation is explicitly about *nobody* having
acknowledged, and it must not be satisfied by the wrong person.

**Starting the resolving action records one too.** Rolling back, redeploying or
building records an acknowledgement of the project's own open deliveries, at
`"acknowledgedVia": "action"` — the alternative is a condition somebody is
actively fixing going on counting as untended and escalating underneath them.

## Silencing

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/alerts/silence \
  -d '{"fingerprint": "pvc.filling/shop/shop-data", "audience": "developer",
       "reason": "waiting on the upstream fix", "until": "2026-04-03T09:00:00Z"}'
```

`200`. A silence must carry a reason and an expiry, and both are refused as
`400` when missing: a decision with no reason and no end is a rule that quietly
outlives whoever made it. The bound is **thirty days** — a decision nobody
revisits within a month is one whose reason has stopped being true without
anybody noticing, and asking again is cheap.

A silence is **project-scoped**, and it is scoped by being keyed on the pair:
it lowers the delivery it names to `log` and never reaches the operator's row
about the same condition. It also stops the escalation clock, which an
acknowledgement does too — somebody wrote down a reason.

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/alerts/unsilence \
  -d '{"fingerprint": "pvc.filling/shop/shop-data", "audience": "developer"}'
```

Lifting one is a record rather than a deletion. The log is append-only, and
*who decided this should be loud again* is as much a question as who quietened
it.

## Claiming

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/alerts/claim \
  -d '{"fingerprint": "workload.crashloop/shop/shop-production/web"}'
```

`200`. **There is no on-call rota, and there is not going to be one.**
Escalation addresses one ticket to the operators as a group, and any operator
claims it — which turns *three tickets nobody owns* into *one ticket anybody
can take*. `GET /access/identities` already enumerates every operator with
their last activity and whether they have stopped showing up, which is the
failure a stale rota causes and does not detect; an installation large enough
to genuinely need scheduling is already running PagerDuty or Opsgenie, and the
correct integration there is the webhook that already exists.

A claim is about the operator's delivery and no other, so it takes no
`audience`. It is not restricted to a delivery that has already escalated:
refusing an early claim would be refusing somebody saying *I have this* before
the clock agreed with them.

## What is recorded

Every acknowledgement, silence, lift and claim is written twice:

- to the **audit log**, hash-chained, under kind `SignalMitigation`, correlated
  on `fingerprint#audience` and carrying the rule, the tier, the reason and any
  expiry. That is the durable evidence, kept under the audit retention;
- to the telemetry store's `signal_mitigations` table beside
  `signal_transitions`, which is what answers *what stands now*.

A write the audit log cannot record is a write the platform does not make:
`503`, and nothing is stored.

## When nothing is recording

All four writes resolve their subject in the recorded history and refuse
otherwise:

- `409` when background evaluation is not recording — there is no delivery for
  the record to be about, and the escalation clock it is meant to stop is
  measured from an `openedAt` only the history has;
- `404` when that delivery is not open — a condition that has resolved, or a
  row this platform never recorded. A **symptom row** is always a `404` here:
  its fingerprint is derived and was never a delivery, which is the mechanism
  behind *no button they cannot press, and no silence either*.

## Notifications

A subscription may ask for `signal.firing` and set `minTier` — see
[Notifications](notifications.md). The tier lives on the rule and the *filter*
lives on the subscription: what kind of thing a condition is, and what a reader
is meant to do about it, is catalogue knowledge versioned with the catalogue;
which of them reach a given relay is an installation's preference.

## Route reference

| Method | Path | What | Role |
|---|---|---|---|
| GET | `/alerts` | Every open delivery this caller may read, worst first. `?project=` narrows | any account — filtered |
| POST | `/alerts/ack` | Record that somebody has seen it | member of the delivery's project for `developer`, `operator` for `operator` |
| POST | `/alerts/silence` | Quieten one delivery, with a reason and an expiry | as above |
| POST | `/alerts/unsilence` | Lift a silence before it expires | as above |
| POST | `/alerts/claim` | Take the escalated ticket | `operator` |
