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

```
GET /audit/verify?from=1
```

answers `{from, to, checked, intact, findings, anchor, truncated}`. Each
finding is `{sequence, break, detail}` with `break` one of `mutated`
(a record no longer hashes to the hash stored beside it), `missing` (a gap) or
`unlinked` (a record whose `prevHash` is not its predecessor's hash). A run
that starts partway through is linked to the record before it, so a tail
lifted out of another chain does not verify; asking for a `from` whose
predecessor is not in the log answers `400`. `anchor` is where the platform
believes the chain ends, held outside the table — a run that is `intact` but
ends below the anchor is a log cut short from the end.

`GET /compliance` answers whether any of this is actually happening:

```json
{
  "audit": {"enabled": true, "recording": true, "retentionDays": 365, "sequence": 1428},
  "attestation": {
    "enabled": true,
    "signing": true,
    "keyID": "9f2c…",
    "publicKey": "-----BEGIN PUBLIC KEY-----\n…"
  },
  "policy": {"storing": true}
}
```

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
