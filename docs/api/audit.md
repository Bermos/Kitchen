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
`{timestamp, type, project, environment, build, release, claim, message, actor, value}` —
the object fields name what the entry is about so a client can link to it,
`actor` is the authenticated caller for API-driven changes and `operator` for
things the reconcilers decided on their own, and `value` carries the one
number some events have (a finished build's duration in seconds). Types:
`build.succeeded`, `build.failed`, `release.promoted`, `release.rolledBack`,
`preview.created`, `preview.removed`, `project.created`, `project.deleted`,
`claim.created`, `claim.deleted`, `claim.bound`, `claim.failed`.

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
`{sequence, timestamp, actor, actorKind, correlation, operation, kind, name, project, fromState, toState, reason, details, prevHash, hash}`,
newest first. `actorKind` is `user` or `service`; a transition the platform
decided on its own is attributed to the reconciler that decided it
(`system:controller/build`), never to "the operator". `correlation` ties every
record from one cause together — for a deploy, the commit.

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
  }
}
```

The public key is handed out deliberately. It is not a credential — evidence
signed under a key nobody can obtain is evidence nobody can check — and it is
what lets an auditor run `cosign verify-attestation --key` against the
registry with Kitchen out of the loop.
