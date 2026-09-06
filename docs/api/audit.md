# Kitchen — The activity feed and the audit log

Two records of the same writes, kept apart on purpose. The feed is what a
screen shows and is best-effort; the audit log is the contract, and a write it
refuses is a write the API does not make.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## The activity feed

`GET /events` answers what the platform did recently, newest first: builds
finishing, releases moving, previews coming and going.

```
GET /events?project=shop&limit=50&since=2026-08-13T00:00:00Z
```

Entries are
`{timestamp, type, project, environment, build, release, claim, process, run, message, actor, value}` —
the object fields name what the entry is about so a client can link to it,
`actor` is the authenticated caller for API-driven changes and `operator` for
things the reconcilers decided on their own, and `value` carries the one
number some events have (a finished build's duration in seconds, a scheduled
run's). Types:
`build.succeeded`, `build.failed`, `release.promoted`, `release.rolledBack`,
`release.redeployed`, `preview.created`, `preview.removed`, `preview.refused`,
`project.created`, `project.deleted`,
`claim.created`, `claim.deleted`, `claim.bound`, `claim.failed`,
`run.started`, `run.succeeded`, `run.failed`, `secret.rotated`.

`preview.refused` carries both refusals, and its `message` says which: a
project at its [preview ceiling](./projects.md#the-preview-ceiling), or a pull
request from a fork the project does not publish
([`previewsForks`](./projects.md#pull-requests-from-forks)). The second has no
`build` field, because a fork the project does not build never produces one.

`secret.rotated` is the one entry here that is not about somebody's write: it
is the platform restarting a workload because a Secret it reads changed under
it, and it names which workload and what it was reading. A pod roll at a
moment nobody deployed anything has no other account of itself. The write that
caused it is the audit log's, as a credential change.

`release.redeployed` is the deploy no commit caused: somebody corrected a
project setting and asked for the release the environment was already on to be
cut again with it (see
[redeploying](environments.md#redeploying-what-is-already-there)). It is its
own type rather than a `release.promoted` so that "what changed when nothing
was pushed" is answerable from the feed alone.

The three `run.` types are one firing of a
[scheduled job](processes.md); `process` and `run` name which, and `run` is
what the log store keys that firing's output by. Both outcomes are in the feed
rather than only the failure, because "it ran at 03:00 and took nine seconds"
is the entry that makes the *absence* of an entry mean something. Only a run
somebody started by hand announces its start — a schedule firing is not news
until it has an outcome.

The feed is written by the reconcilers and the API into the events table of
the telemetry store, under the same retention as the logs. Kubernetes Events
were deliberately not the source of truth: they expire in an hour and carry
machinery noise the feed would have to filter back out.

## The audit log

`GET /audit` answers what the platform *did* — as evidence rather than as
prose. It is not the activity feed above and does not replace it: the feed is
best-effort and reads like a story, this is an append-only hash chain and a
transition it could not record is a transition the platform refused to make.
See [COMPLIANCE.md](../COMPLIANCE.md) for the model.

```
GET /audit?kind=Project&name=shop&actor=grace@example.com&since=2026-08-13T00:00:00Z
```

Records are
`{sequence, timestamp, actor, actorKind, correlation, operation, kind, name, project, fromState, toState, reason, details, privileged, privilegeClass, prevHash, hash}`,
newest first. `actorKind` is `user` or `service`; a transition the platform
decided on its own is attributed to the reconciler that decided it
(`system:controller/build`), never to "the operator". `correlation` ties every
record from one cause together — for a deploy, the commit.

### Privileged records

Most of the log is what the platform is *running*: builds, releases, rollbacks,
environment variables. A few records are what the platform will *allow*, and
they are the ones a supervisor asks about. Those carry `privileged: true` and a
`privilegeClass`:

| Class | What moved |
|---|---|
| `break-glass` | an exception granted, relied on, resolved, or expiring unresolved |
| `requirements` | what an environment demands — its bundle, its parameters, its owners |
| `classification` | a data class or a residency |
| `access` | who may do what: a project's grants, the operator list, a recertification decision |
| `credential` | a credential the platform holds, written or replaced |
| `integrity` | a write to a Kitchen-managed object that no reconcile made ([access](access.md#out-of-band-writes)) |

```
GET /audit?privileged=true&since=2026-01-01T00:00:00Z
GET /audit?privilegeClass=break-glass
```

`privilegeClass` implies `privileged=true`, and a class that is not one of the
six answers `400` with the list rather than an empty page.

Both fields are a *reading* of the record and not a second source for it: the
marking lives inside `details`, which is what the chain hashes, so a privileged
marking added to or taken off a stored record breaks verification. It is
deliberately not a column of its own — a new column would change the hash of
every record ever written, and the whole log would stop verifying at once.

The chain fields come back with every record on purpose. An audit view that
hid them would be asking to be believed, and the point of a chain is that it
does not have to be.

### When there is nothing to read

The log is a table in the telemetry store, and the platform creates it when it
reconciles an installation that keeps one. Both reads therefore have three ways
to answer nothing, and they are told apart rather than collapsed into one
failure — a caller who is only told "failed" cannot tell whether to retry, to
fix the call, or to go and find somebody:

| Answer | What happened | What to do |
|---|---|---|
| `503` | This installation keeps no audit log: `spec.compliance.audit` is off, so nothing has been recorded and there is no table to read | Nothing. `GET /compliance` says the same thing about the whole evidence surface |
| `503` | The log's table is not in the store yet, or the store did not answer | Try again. Both clear on their own; the Kitchen object's compliance status says which it was |
| `500` | The store refused the platform's own query | Report it. The message is deliberately not the store's diagnostic — that is a fault for whoever maintains Kitchen and is in the operator's log |

The CLI publishes the first two as `unavailable` (exit 7) and the third as
`failed` (exit 1), so `kitchen api GET /audit` answers the same question in the
same words as the dashboard.

```
GET /audit/verify?from=1
```

answers `{from, to, checked, intact, findings, anchorPresent, anchor,
anchorOrigin, anchorAdoptedFrom, anchorMessage, truncated}`. Each finding is
`{sequence, break, detail}` with `break` one of:

| `break` | What it is |
|---|---|
| `mutated` | A record no longer hashes to the hash stored beside it |
| `missing` | A gap in the sequence: records were deleted |
| `unlinked` | A record whose `prevHash` is not its predecessor's hash |
| `truncated` | The log stops short of where the anchor says the chain ends: records were cut off the end |
| `unclaimed` | The log runs *past* the anchor: rows that claimed no sequence number, or an anchor wound back |
| `unanchored` | There is no anchor to check this run against at all |

A run that starts partway through is linked to the record before it, so a tail
lifted out of another chain does not verify; asking for a `from` whose
predecessor is not in the log answers `400`.

### The anchor, and why `intact` depends on it

The hash chain can only ever say the records agree with each other. A log cut
short from the end agrees with itself perfectly — recomputing every hash from
the edit onwards is exactly as cheap for whoever did it as it was for the
platform — so the only thing that shows it is the **anchor**: the head object
`kitchen-audit-head`, which sequence numbers are claimed through and which
lives outside the table the log is in.

`intact` is the whole answer and consults the anchor. It is `false` for a run
that ends below the anchor and for a run with **no** anchor to check against,
so `kitchen api GET /audit/verify`, a script, and the dashboard all read the
same verdict; the comparison used to be the dashboard's alone, done
client-side (#428).

| Field | What it is |
|---|---|
| `anchorPresent` | Whether there is an anchor at all. `false` on an installation that is recording means the object was removed |
| `anchor` | Where the chain ends according to it — `null`, never `0`, when there is none. `0` is a real answer, about a chain nothing has been appended to yet |
| `anchorOrigin` | `genesis` (the anchor predates the first record), `adopted` (it was seeded from the log's own last record) or `unknown` (a head written before the platform recorded this) |
| `anchorAdoptedFrom` | For an adopted anchor, the sequence it was taken from. Records at or below it are bounded by the hash chain alone |
| `anchorMessage` | Why there is no anchor: an object somebody deleted and a cluster that did not answer are the same gap and very different events |

The platform creates the anchor as soon as it keeps a log at all — the
compliance reconcile does it, not the first append — so "no anchor and no
records" is never a fresh installation, and *empty because nothing was
written* and *empty because somebody emptied it* are different answers.

**An anchor adopted from the table is adopted once, and says so in the log.**
An installation upgrading from before the head object existed has its anchor
only in the table, and the platform seeds one from the log's own last record.
That is a privileged act and it is recorded as one: an `AuditAnchor` record,
classified `integrity`, naming the sequence the numbering was taken from. It
is *inside* the chain, so it cannot be removed without the verifier reporting
the removal — which is what makes a second adoption, from an anchor somebody
deleted and a tail somebody cut, impossible to tidy away. `GET
/audit?kind=AuditAnchor` is the query.

Under a running platform the head never moves backwards. An append against one
that has is refused, and the write that caused it fails with it, on the same
principle as everywhere else here: an unrecorded change is not one the platform
makes.

`GET /compliance` answers whether any of this is actually happening:

```json
{
  "audit": {
    "enabled": true, "recording": true, "retentionDays": 365,
    "sequence": 1428, "anchored": true
  },
  "attestation": {
    "enabled": true,
    "signing": true,
    "keyID": "9f2c…",
    "publicKey": "-----BEGIN PUBLIC KEY-----\n…"
  },
  "policy": {"storing": true}
}
```

`anchored` is whether the chain has an anchor at all. `sequence` is a
statement about the log only when it is `true`: a missing anchor used to
answer `0`, which is also what a chain nothing has been appended to answers,
and telling the two apart is the whole of #428. `anchorMessage` says why there
is none, or where an adopted one was taken from.

The public key is handed out deliberately. It is not a credential — evidence
signed under a key nobody can obtain is evidence nobody can check — and it is
what lets an auditor run `cosign verify-attestation --key` against the
registry with Kitchen out of the loop.

`policy` is the [decision register](decisions.md)'s posture, mirroring the
audit log's: the policy engine always evaluates — a bundle and an input in
hand need nothing else — but keeping a replayable record needs the store, and
`storing: false` with its message is the platform owning up to decisions that
stand without one.

## The classification inventory

`GET /compliance/inventory` answers where every environment's and resource
claim's data stands — its class, its provenance and its location — in one
request, exportable as it is. It is filtered like every cross-project read:
a member gets their projects' rows, an operator gets the whole install.

```json
{
  "generatedAt": "2026-08-21T09:00:00Z",
  "defaultResidency": "CH",
  "items": [
    {"kind": "environment", "project": "shop", "name": "shop-production",
     "type": "production", "dataClass": "confidential", "residency": "CH"},
    {"kind": "claim", "project": "shop", "name": "shop-db", "type": "postgres",
     "dataClass": "confidential", "provenance": "production", "residency": "aws-eu-central-1"}
  ]
}
```

The absences are words, never blanks, because an export an auditor reads must
not leave an empty cell open to a generous reading: `dataClass` is
`unclassified` when nobody declared one, `provenance` (claims only) is
`undeclared` when the provider made no statement about what the provisioned
data derives from, and `residency` is `unknown` when nothing is declared or
reported. An environment without a residency of its own inherits
`defaultResidency` — the platform-wide declaration on the Kitchen object,
declared rather than observed — while a claim's residency is the provider's
*reported* placement and deliberately inherits nothing: it is the placement
of record, not a declaration.

Rows are sorted by project, kind and name, so two exports diff cleanly. There
is no dedicated CLI command — `kitchen api GET /compliance/inventory` is the
terminal's route to the same document.
