# Kitchen — Access, recertification and privileged operations

Who holds what on this platform, who last looked at that, and what they
decided about each of it. Every route here is the operator's — the answer is
the whole installation's access in one document, so a member who could read it
would learn every account on the platform and every project's membership.
"Who has access to `shop`" is already answerable to `shop`'s own admins,
through [`GET /projects/{name}/members`](projects.md#membership).

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to. The
model behind it — why a recertification is an object, why the reviewer may be
the reviewed, and what the out-of-band detection can and cannot see — is
[COMPLIANCE.md §11](../COMPLIANCE.md).

## Who holds what, right now

```
GET /access/identities
```

One row per **grant**, not per account: an account holding admin on three
projects is three rows, because those are three decisions for a reviewer.

```json
{
  "generatedAt": "2026-08-24T09:00:00Z",
  "inactivityDays": 90,
  "directoryConsulted": true,
  "orphans": 1,
  "identities": [
    {"subject": "user_1", "email": "grace@example.com", "grant": "platform", "role": "operator",
     "lastActive": "2026-08-24T08:12:00Z"},
    {"subject": "user_7", "grant": "shop", "role": "developer",
     "lastActive": "2025-02-03T11:00:00Z", "inactive": true, "unknown": true, "orphaned": true}
  ]
}
```

The platform's own grants lead, then projects by name, then subjects — so two
exports of an unchanged platform diff cleanly.

**`orphaned` is `inactive` *and* `unknown`, never either alone.** `inactive`
is no recorded action within `inactivityDays`; `unknown` is the identity
provider holding no account for that subject. Either on its own has an
innocent reading — a quiet quarter for a perfectly real person, or a machine
account the directory does not list — and a list that called either an orphan
is a list nobody acts on. The pair does not have an innocent reading: it is a
grant pointing at nobody.

Two limits, stated rather than papered over:

- **`lastActive` is the audit log's, and the audit log records writes.**
  Somebody who only ever reads — opens the dashboard, follows logs, looks at a
  build — is `inactive` here and is not inactive in fact. The alternative
  would be recording reads in the evidence log, which would drown it.
- **`directoryConsulted: false` means nothing is claimed about ownership.** An
  installation federated to an issuer of its own serves no account directory
  at all, so no row is `unknown` and no row is `orphaned`, and `message` says
  why. "We could not ask" and "nobody is behind it" are different sentences
  and only one of them is evidence.

There is no dedicated CLI command for the raw survey — `kitchen access
identities` prints it, and `kitchen api GET /access/identities` is the same
document unshaped.

## Recertification cycles

A cycle opens, freezes a snapshot of every grant in scope at that instant, is
reviewed grant by grant, and closes. Closing does two things: it carries out
the revocations, and it mints a signed, timestamped artefact of the whole
cycle which is kept in the telemetry store's `signed_records` table — so the
evidence outlives the object.

The platform opens one on its own every `spec.compliance.access.intervalDays`
(90 by default), counted from the last cycle's *close*. These routes are for
opening one out of cadence and for doing the review.

### Opening one

```
POST /access/reviews
```

```json
{
  "scope": "all",
  "reviewers": ["grace@example.com", "cto@example.com"],
  "dueBy": "2026-09-07T00:00:00Z",
  "reason": "the annual internal audit"
}
```

Everything is optional. `scope` is `all` (the whole installation, the
default), `platform` (the operator list alone) or `project` — which requires
`project`. `reviewers` defaults to the platform's operators, who are the only
accounts that can see every grant anyway. `dueBy` defaults to
`spec.compliance.access.dueDays` from now.

Answers `201` with the cycle, snapshot included.

**One cycle at a time over the same grants.** A second overlapping cycle
answers `409` naming the one in the way: two open cycles would be two
reviewers deciding the same question, and a close that applied one set of
revocations while the other still showed the grant.

`reviewers` is an **expectation, not an enforcement**. The API admits a
decision from any platform operator and records who actually made it, because
a cycle only a named person could close is a cycle that stalls the week they
are on holiday. Who was expected is on the cycle; who decided is on every
entry and in the audit log.

### The register

```
GET /access/reviews                    # open cycles
GET /access/reviews?historical=true    # and the closed ones
GET /access/reviews/{name}
```

Newest first. A cycle carries its snapshot, every decision, the tallies, and —
once closed — a pointer to its artefact:

```json
{
  "name": "access-review-8x2kd",
  "scope": "all",
  "reviewers": ["grace@example.com"],
  "openedBy": "system:controller/accessreview",
  "phase": "Open",
  "dueBy": "2026-09-07T00:00:00Z",
  "snapshotAt": "2026-08-24T09:00:00Z",
  "pending": 3, "confirmed": 5, "revoked": 1, "selfReviewed": 1, "orphaned": 1,
  "entries": [
    {"subject": "user_1", "grant": "platform", "role": "operator",
     "decision": "confirm", "decidedBy": "grace@example.com",
     "decidedAt": "2026-08-24T10:00:00Z", "selfReview": true},
    {"subject": "user_7", "grant": "shop", "role": "developer", "orphaned": true,
     "decision": "revoke", "decidedBy": "grace@example.com", "note": "left in June",
     "applied": true}
  ],
  "artifact": {
    "recordID": "0f6c…", "subject": "sha256:9ab3…",
    "predicateType": "https://kitchen.bermos.dev/attestation/access-review/v1",
    "signedAt": "2026-08-24T11:00:00Z"
  }
}
```

`phase` is judged against the clock server-side, so `Overdue` means overdue
*now* rather than "the reconciler has got round to it". Overdue is a report
and never a consequence: nothing is revoked, nothing is refused, and no
deployment is blocked because a review ran late.

### Deciding, and closing

```
PATCH /access/reviews/{name}
```

```json
{
  "decisions": [
    {"subject": "user_7", "grant": "shop", "decision": "revoke", "note": "left in June"},
    {"subject": "user_1", "grant": "platform", "decision": "confirm"}
  ],
  "close": true
}
```

`decision` is `confirm` or `revoke`. The `(subject, grant)` pair identifies
the entry; a pair that is not in the cycle's snapshot answers `400` saying so
— a review decides about what was true when it opened, and a grant made since
belongs to the next cycle.

Decisions and the close are one request on purpose: a reviewer works through
the list and closes it, and a close that raced the last decision would produce
an artefact missing it. Either half alone is fine too.

A closed cycle answers `409`. Its artefact is minted and its decisions stand;
what comes next is a new cycle, not a reopened one.

**The reviewer may be the reviewed, and it is recorded rather than refused.**
A decision somebody makes about their own grant is stamped `selfReview: true`,
counted in `selfReviewed`, carried into the artefact and named in the audit
record. This is the answer [§8.4](../COMPLIANCE.md) gives for self-approval,
and it is the same argument: an installation with one operator has exactly one
person who can review that operator's grant, and refusing would either make
the control unsatisfiable or push somebody into creating a second account to
satisfy it — worse evidence, not better.

### What closing actually does

Revocations are carried out by the operator, one at a time, each with its own
privileged audit record. `applied: true` on an entry means the grant was
taken off; `applyMessage` says why one was not, and there are three reasons:

- **it is the platform's last operator.** A platform with no operators refuses
  every operator-only route to everybody — including the one that names an
  operator — and there is no way back that does not involve `kubectl`. Name a
  replacement first, then revoke.
- **the grant was already gone**, or had moved to another role, by the time
  the cycle closed. That is often the outcome the reviewer wanted.
- **the project no longer exists**, so the grant went with it.

The artefact is a DSSE envelope wrapping an in-toto Statement under
`https://kitchen.bermos.dev/attestation/access-review/v1`, whose subject is
the cycle's identity digest (sha256 over its namespace, name and UID — a
review has no OCI repository). It carries the snapshot, every decision, who
made it, which were self-reviews and which revocations were actually applied.
It verifies against the public key on `GET /compliance` with Kitchen out of
the loop. A platform with no signing key or no store still closes the cycle,
still applies the revocations and still writes the audit records —
`artifact.message` says what could not be kept, rather than leaving a blank
field open to a generous reading.

## Privileged operations

Changing what an environment demands, granting a break-glass exception,
rotating a credential, moving a data class and changing who holds a role are
not ordinary writes: each moves the bar the rest of the log is judged against.
They are a distinct class in the audit log and separable with one filter —
see [the audit log's privileged records](audit.md#privileged-records) for the
six classes and the query.

## Out-of-band writes

Anybody holding cluster-admin can edit Kitchen's objects directly and bypass
every control this API enforces. Kitchen does not claim to prevent that. What
it does is notice: the operator reads `metadata.managedFields` on the six
kinds whose content *is* a control — `Kitchen`, `Project`, `Environment`,
`Exception`, `Connection`, `AccessReview` — and records a privileged
`integrity` audit record for a field manager it does not recognise.

The result is on the singleton, and on `GET /compliance`:

```json
{"access": {"reviewing": true, "openReview": "access-review-8x2kd",
            "lastClosed": "2026-05-24T11:00:00Z", "identities": 9, "orphaned": 1,
            "outOfBandWrites": 1, "lastOutOfBand": "2026-08-24T03:14:00Z"}}
```

`outOfBandWrites` is a standing count rather than a running total: a foreign
manager stays on an object's `managedFields` until the platform writes those
fields again, so it answers "what is currently marked" and the audit log
answers "what happened".

What it does not see is documented in
[COMPLIANCE.md §11.4](../COMPLIANCE.md) rather than glossed here, and the
short version is: a caller may name their own field manager anything they
like, so `kubectl --field-manager=kitchen` is invisible to it and always will
be. An installation with a legitimate writer of its own names it in
`spec.compliance.access.expectedManagers`, which is the operator putting "this
writer is expected" on the record — the alternative being an alert everybody
learns to ignore.
