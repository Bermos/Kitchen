# Kitchen — The audit pack

An institution asked to show its evidence assembles it by hand from a git
platform, a ticketing system, a spreadsheet and a log store — and the
reconciliation gap between those four *is* the finding. Kitchen holds one
reconciled graph, so the export is a request rather than a project.

```
GET /api/v1/projects/{name}/audit-pack?from=&to=
```

One project, one half-open window, one document. Everything in it exists
behind another endpoint already; what this adds is that it is one file, signed,
byte-reproducible, and legible to somebody who is not an engineer.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table. The design behind it is
[COMPLIANCE.md §13](../COMPLIANCE.md).

## Taking one

```sh
curl -H "authorization: Bearer $TOKEN" \
  "https://api.kitchen.example.com/api/v1/projects/shop/audit-pack?from=2026-01-01T00:00:00Z&to=2026-04-01T00:00:00Z" \
  > pack.json
```

`200` with the pack. **Both ends of the window are required**, RFC 3339, and a
request missing either is a `400` that says why: a pack whose range ended
"now" could not be reproduced, and reproducibility is the point. The interval
is half-open — `from` is inside it, `to` is not — so consecutive quarters
tile without overlapping and nothing is counted twice.

`to` before `from` is a `400`. A project the caller cannot see is a `404`, like
every other read of a project.

**It is the operator's.** Not caution — subject matter. A route's guard has to
be the strictest thing in its body, and this body folds three operator-only
reads into a project's evidence: the recertification cycles that reviewed this
project's grants (whose signed artefacts cover every other project's too, and
are carried verbatim because a re-encoded envelope does not verify), the
platform's retention model, and the audit chain's anchor. A project admin who
could export a pack would read, in one file, what three routes refuse them
separately.

It is also who takes one: an audit pack is produced by the institution's second
line for somebody outside it, not by the team that deploys. The application
team is not left worse off — every project-scoped part of it is already a
`viewer`'s read of its own (`/compliance/inventory`, `/compliance/drift`,
`/decisions`, `/exceptions`, `/projects/{name}/promotions`, `/audit?project=`).

Taking a pack is **recorded in the audit log** under the kind
`EvidenceExport`, with the range, the format, the pack's digest, its size and
its section counts. An export the log cannot record answers `503` and is not
served: an export nobody can prove happened is the one shape of this feature
that would be worse than not having it. `GET /api/v1/audit?kind=EvidenceExport`
is "every pack anybody has ever taken".

## The three formats

One address, one document, three renderings. `?format=` chooses.

| `format` | Content type | What it is |
|---|---|---|
| `json` (default) | `application/json` | **The pack.** These bytes, exactly as served, are what the digest and the signature are about. |
| `dsse` | `application/json` | A DSSE envelope wrapping an in-toto Statement whose subject is the pack's sha256. Freshly signed on each request. |
| `html` | `text/html` | The same document rendered for a reader. Self-contained, printable, unsigned, and carrying the digest. |

Every response carries `X-Kitchen-Pack-Digest: sha256:…` — the digest of the
`json` bytes — and a `Content-Disposition` naming the file after the project
and the window.

`?format=dsse` on a platform holding no signing key answers `409`: there is
nothing to sign with, and the pack's own `verification` block says the same in
words. `?format=` anything else is a `400` naming the three.

## Byte-reproducibility, and exactly what it covers

Two exports of the same range are the same bytes unless the evidence itself
changed. That is a property the code holds deliberately, and it is worth
knowing how, because it is also the list of ways a future change could break
it:

- **No timestamp is read off a clock inside the pack.** Every other read in
  this API answers with a `generatedAt`; this one does not, and a test refuses
  one being added back.
- **Every list has a total order** — a name, or a timestamp with a name behind
  it. Nothing is left in the order a map iterated or the store answered in.
- **Every phase is judged at the range's end**, not now. An exception that
  expired inside the window reads `Expired` in a pack taken the next day and
  in one taken three years later; a cycle closed inside it reads `Closed`.
- **Canonical JSON**: struct field order (which is why the field order below
  is the format), sorted map keys, no HTML escaping, no indentation, no
  trailing newline.

`pack.reproducibility` states this inside the document, because the document is
read by somebody who has the file and not this page. It names which sections
are **fixed by the range** (`changeLog`, `promotions`, `decisions`,
`exceptions`, `drift.history`, `auditLog`, `signedRecords`, `access.cycles`)
and which describe **the estate as it is now** (`inventory`, `access.grants`,
`attestations`, `drift.current`, `platform`, `retention`).

That second list is the honest part. Kitchen reconciles the graph rather than
versioning it, so "which environments existed in March" was never recorded and
the platform will not pretend otherwise. Re-evaluating a historical state is
what `decisions` and their stored inputs are for.

**Not reproducible, and outside the signed bytes:** the DSSE envelope (an
ECDSA signature carries a nonce, so two signings of identical bytes are two
different envelopes) and the envelope's `exportedAt`. That is where the
export's own timestamp belongs.

## Checking a pack without Kitchen

§5.1 of [COMPLIANCE.md](../COMPLIANCE.md) keeps the whole evidence layer on
standards so that it can be read without this platform. The pack is the same
bargain: the signature is DSSE over an in-toto Statement, and the four commands
below use `jq`, `base64`, `openssl` and `sha256sum` and nothing else. They are
carried inside every pack, at `verification.procedure`, so they travel with the
document.

```sh
# 1. the two documents, side by side
curl -H "authorization: Bearer $TOKEN" "$PACK"              > pack.json
curl -H "authorization: Bearer $TOKEN" "$PACK&format=dsse"  > pack.dsse.json

# 2. the digest of the file must be the statement's subject
sha256sum pack.json

# 3. decode the signed statement
jq -r .payload pack.dsse.json | base64 -d > statement.json
jq -r '.subject[0].digest.sha256' statement.json     # must equal step 2

# 4. rebuild DSSE's pre-authentication encoding and check the signature
printf 'DSSEv1 28 application/vnd.in-toto+json %d ' "$(wc -c < statement.json)" > pae.bin
cat statement.json >> pae.bin
jq -r '.signatures[0].sig' pack.dsse.json | base64 -d > sig.bin
openssl dgst -sha256 -verify public.pem -signature sig.bin pae.bin
```

`28` is the length of `application/vnd.in-toto+json`; the length prefixes are
what make DSSE's encoding unambiguous, so they are not decoration.

`public.pem` is the platform's verification key. The pack publishes it at
`verification.publicKey` so the file reads as one document — but **that is not
where trust comes from**: a key taken out of the same file as the signature
proves only that the file is internally consistent. Verify against the key the
institution kept when the platform was installed, or against `GET
/api/v1/compliance` on a platform you trust. The pack says this in
`verification.warning` rather than leaving it implied.

The `signedRecords` envelopes verify the same way, one at a time, and they are
carried byte for byte for exactly that reason.

## Contents, field by field

Each row names the requirement it is there to satisfy. The GR codes are the
compliance suite's own (see the sub-issues of #144 and, once it lands, #143);
`GR-J3` (inspection readiness) and `GR-L1` (an artefact per control) are what
this endpoint exists for and are not repeated on every row.

### Header

| Field | What it is | Satisfies |
|---|---|---|
| `schema` | `https://kitchen.bermos.dev/audit-pack/v1` — the document's shape, so a pack met years later identifies itself | GR-L1 |
| `project` | The project this pack is about | GR-B1 |
| `range.from` / `range.to` | The window, RFC 3339 UTC | GR-L3 |
| `range.halfOpen` | The interval spelled out, because half of all readers assume the other one | GR-L3 |

### `platform` — who produced it, and under what conditions

| Field | What it is | Satisfies |
|---|---|---|
| `name`, `version`, `clusterName`, `baseDomain` | Which installation, which release | GR-B1 |
| `auditRecording`, `auditMessage` | Whether the log was recording at all, and why it was not. A pack from a platform that was not recording is a very different document | GR-D8 |
| `auditImmutable`, `immutabilityMessage` | Whether the store has taken the audit table's mutation privileges away from the platform's own credential. `false` is a smaller claim, not a fault | GR-D8 |
| `decisionsStored`, `decisionsMessage` | Whether policy decisions are being kept. The engine always evaluates; what needs the store is a replayable record | GR-A4 |
| `rescanning`, `rescanMessage` | Whether continuous re-evaluation is running. An empty drift section means two different things depending on this | GR-D4, GR-F2 |
| `clockSync.checked`, `method` | When node clock drift was last measured and how. No number here should be read without the caveat that belongs to it | GR-D8 |
| `clockSync.nodes`, `drifted`, `maxDriftSeconds` | How many nodes were measured, how many are beyond the threshold, and what the threshold was | GR-D8 |
| `clockSync.worstNode`, `worstDriftMillis`, `message` | The node furthest from the operator's clock and by how much — in milliseconds, because a healthy cluster's answer is a two-digit number of them. Every timestamp in this document is worth what the clock that stamped it is worth | GR-D8 |

### `reproducibility` — what the claim covers

| Field | What it is | Satisfies |
|---|---|---|
| `claim` | The promise in one sentence | GR-L1 |
| `rangeBound` | Sections wholly determined by the window | GR-L3 |
| `currentState` | Sections that describe the estate now, and do not pretend to be a snapshot of `to` | GR-L1 |
| `excluded` | Which bytes the signature does *not* cover: the envelope and the HTML rendering | GR-L1 |

### `retention` — whether the store can still answer for the window

This section is #140's argument applied to the export: a range retention has
already eaten into is **reported**, never silently answered with less.

| Field | What it is | Satisfies |
|---|---|---|
| `auditDays` | The retention in force for the audit log and the decision register — one class, because the decisions follow the audit knob | GR-G6 |
| `floorDays`, `overridden` | The documented minimum, and whether somebody signed off on going under it | GR-G6 |
| `overrideReason`, `overrideApprovedBy` | The written decision behind an audit retention under the floor. Not a credential, and read back in full on purpose | GR-G6, GR-L4 |
| `oldest`, `lastSweep` | What the last sweep measured: nothing of this class is older than this, as of then | GR-G6 |
| `truncated` | **The finding.** The window starts before the oldest evidence the store still holds | GR-G6, GR-L3 |
| `coveredFrom` | The earliest instant this pack can actually speak to | GR-L3 |
| `message` | Coverage stated either way, never by silence | GR-L3 |
| `note` | What is *not* retention-bounded, so an absence elsewhere is not read as a deletion | GR-G6 |

### `verification` — the procedure, carried with the document

| Field | What it is | Satisfies |
|---|---|---|
| `signed` | Whether an envelope exists at all. `false` is a smaller claim, not a fault | GR-D1, GR-I4 |
| `message` | Why a pack is unsigned, where it is | GR-L1 |
| `predicateType`, `payloadType` | `…/attestation/audit-pack/v1` and `application/vnd.in-toto+json` | GR-I4 |
| `keyID`, `publicKey` | The key. A public key is public by construction; evidence signed under a key nobody can obtain is evidence nobody can check | GR-I4 |
| `procedure` | The four commands, in order | GR-I4, GR-J3 |
| `warning` | That a key taken out of the same file proves only internal consistency | GR-I4 |

### `inventory` — the estate behind the project

| Field | What it is | Satisfies |
|---|---|---|
| `project.name`, `createdAt` | The project itself | GR-B1 |
| `project.dataClass` | Its classification. `unclassified` is a word, never a blank | GR-G1 |
| `project.criticality`, `rto`, `rpo` | The institution's designation and its tolerances. `undesignated` is a word | GR-C1, GR-C2 |
| `project.repository`, `branch` | Where the code comes from | GR-D1 |
| `project.requirePullRequest` | Whether the project demanded a reviewed pull request, which is what makes a build's missing review readable as "not asked for" rather than "asked for and missing" | GR-D1, GR-E2 |
| `project.sourceConnection`, `registryConnection` | The two third parties every project has | GR-C4 |
| `environments[].name`, `type`, `phase`, `url`, `createdAt` | Each environment | GR-B1, GR-D2 |
| `environments[].dataClass`, `residency` | What it is rated to hold and where its data sits. `unclassified` / `unknown` | GR-G1, GR-G2 |
| `environments[].criticality`, `rto`, `rpo`, `inherited` | The designation that actually applies, with `inherited` naming what was derived from the project rather than declared here | GR-C1, GR-C2 |
| `environments[].release`, `image` | What is running, by name and by content | GR-B1, GR-D1 |
| `environments[].owners` | Who may change what this environment demands. Empty means the platform's operators alone — segregation of duties as a schema | GR-A2, GR-E2 |
| `environments[].bundleDigest`, `parameters` | The bar itself. Absent means this environment declares none, which is a finding rather than a blank | GR-A4 |
| `environments[].domains` | The hostnames pointed at it | GR-B1 |
| `releases[]` | The releases this window is about — cut inside it, promoted inside it, or still deployed. `inRange` says which. Not every release the project has ever cut | GR-B1, GR-D1 |
| `claims[].name`, `type`, `phase`, `createdAt` | Each provisioned resource | GR-B1 |
| `claims[].connection`, `provider` | The third party behind it | GR-C4 |
| `claims[].dataClass`, `residency` | Its class and the provider's *reported* placement | GR-G1, GR-G2 |
| `claims[].provenance` | What the provisioned data derives from — `production`, `masked`, `synthetic`, or `undeclared`, which policy treats as the worst case | GR-D2, GR-G4 |
| `connections[].name`, `provider`, `capabilities`, `usedFor` | Every third-party relationship, and what it is used for | GR-C4 |
| `connections[].credential` | The words "held by the platform, never in this document". A blank would invite a reader to conclude there is none | GR-E1 |
| `domains[].hostname`, `environment`, `verified`, `tlsMode` | Every custom address, which environment answers on it, whether ownership was verified and how it is served | GR-B1 |
| `thirdParties[]` | The distinct set of providers behind all of the above — the list an operational-resilience register asks for | GR-C4 |
| `scope.releases`, `scope.depth` | Which releases are here and why, and the honest limit of the traversal: a third party an application calls from its own code is not a Connection and the platform cannot see it | GR-C4 |

### `access` — who holds what, and who last checked

| Field | What it is | Satisfies |
|---|---|---|
| `grants[].subject`, `email`, `role` | The project's own `spec.access`. The address is informational — nothing resolves against it — and the platform's operators hold admin everywhere and are not listed here | GR-E1 |
| `cycles[]` | Every recertification cycle whose scope covered this project and whose life overlapped the window | GR-E2 |
| `cycles[].scope`, `openedBy`, `reviewers`, `reason`, `dueBy`, `openedAt`, `snapshotAt`, `closedBy`, `closedAt` | The cycle | GR-E2 |
| `cycles[].phase` | Judged at the range's end, so a pack of last quarter does not change its mind in April | GR-E2 |
| `cycles[].pending`, `confirmed`, `revoked`, `selfReviewed`, `orphaned` | The tally. `selfReviewed` is recorded rather than filtered: an installation with one operator has exactly one person who can review that operator's grant | GR-E2, GR-E3 |
| `cycles[].entries[]` | The decisions about grants naming **this project** or the **platform** role — a platform operator holds admin here, so that is a grant on this project. Each carries `subject`, `email`, `grant`, `role`, `decision`, `decidedBy`, `decidedAt` and `note`. An undecided grant reads `undecided`, never omitted | GR-E2, GR-E3 |
| `cycles[].entries[].selfReview`, `inactive`, `orphaned`, `applied` | What the reviewer was looking at and what came of it: whether they were deciding their own grant, whether the account had been dormant, whether the directory knows of anybody behind it at all, and whether a revocation was actually carried out | GR-E2, GR-E3 |
| `cycles[].entriesTotal`, `entriesNote` | How many decisions the cycle held in all, and that the rest are about other projects — the omission is stated, not silent | GR-E2 |
| `cycles[].recordID`, `subject`, `predicateType`, `signedAt` | Where the cycle's own signed artefact is, in `signedRecords` | GR-E2, GR-L1 |
| `cycles[].artifactNote` | Why a closed cycle has no retained artefact, where it has none | GR-L1 |
| `note` | That the cycle's artefact covers other projects too, which is why the entries beside it are narrowed | GR-E2 |

### `changeLog` — one entry per release in the window

| Field | What it is | Satisfies |
|---|---|---|
| `release`, `build`, `createdAt`, `image`, `digest` | What was cut, and the artifact by content | GR-D1 |
| `commit`, `branch`, `message`, `author` | The change itself | GR-D1 |
| `review.provider`, `pullRequest`, `title` | The request the commit arrived through. A zero pull request is what a direct push looks like | GR-D1 |
| `review.author`, `mergedBy` | Who opened it and who merged it, which are not always the same and occasionally the only human involved | GR-D1, GR-E2 |
| `review.approvers[]` | The approvals that **still stood** when the request resolved. Reviews a later push dismissed are already excluded | GR-D1, GR-E2 |
| `review.selfApproved` | The only approvals were the author's own. Recorded, not treated as no approval: whether that is acceptable is the institution's question | GR-E2 |
| `review.independent` | Somebody other than the author approved — the question four-eyes actually asks | GR-E2 |
| `review.required` | Whether the project demanded review for this commit | GR-D1 |
| `review.machineIdentity` | The allowlisted account the commit was exempted under, where one was used | GR-E5 |
| `review.exception` | The break-glass grant that waived the review requirement, where one did | GR-L4 |
| `review.checkedAt`, `review.message` | When the provider was asked, and why it could not answer | GR-D1 |
| `reviewNote` | Why there is no review block — a build the provider was never asked about and one that went through no request are different findings | GR-D1 |
| `deployments[]` | Where the release ran and when: environment, `from`, `to`, `reason`, `by`, and `current` for one still serving | GR-D1, GR-D2 |

### `promotions` — what was asked, and what was answered

| Field | What it is | Satisfies |
|---|---|---|
| `name`, `environment`, `release`, `createdAt` | The request | GR-D1 |
| `requestedBy`, `trigger`, `reason` | Who asked, whether a person or the pipeline, and why | GR-D1, GR-E2 |
| `phase`, `verdict`, `message` | What became of it. Three verdicts and no fourth: `allowed`, `allowed-with-exception`, `blocked` | GR-A4 |
| `unmetRules[]` | The rules that fired unwaived, as the stable ids the bundle publishes — a blocked promotion says exactly what was missing | GR-A4, GR-D4 |
| `decisionID` | The stored decision, in `decisions` | GR-A4, GR-L1 |
| `appliedAt` | When the environment was actually moved | GR-D1 |

### `decisions` — the reproduction inputs

| Field | What it is | Satisfies |
|---|---|---|
| `items[].id`, `timestamp`, `kind` | Every decision stored about this project in the window: `promotion`, `rescan` or `replay` | GR-A4, GR-L1 |
| `items[].environment`, `release`, `artifact` | What it was about | GR-A4 |
| `items[].bundleDigest`, `inputDigest` | The two halves of the evaluation, by content | GR-A4 |
| `items[].dataSnapshot` | The vulnerability database the evidence was produced against — what makes a finding reproducible rather than merely repeatable | GR-D4 |
| `items[].verdict`, `decidedBy` | The answer, and who or what asked | GR-A4 |
| `items[].rulesFired` | The engine's own encoding, verbatim: `[{rule, message, waived, exception}]` | GR-A4, GR-L4 |
| `items[].input` | The full canonical input, verbatim. Re-encoding it would break `inputDigest` | GR-A4 |
| `truncated`, `limit`, `message` | Whether the window held more decisions than one read returns, and what to do about it | GR-L3 |
| `note` | That `POST /decisions/{id}/replay` re-runs one against the bundle bytes kept beside it, so a replay does not depend on a ConfigMap that may since have been edited | GR-A4 |

### `attestations` — the evidence set per artifact

An **index**, not a copy. The attestations themselves are in the registry
against the artifact's digest and are read with anything that speaks OCI
referrers; copying them into the pack would double every byte and would
quietly contradict §5.1's whole argument, which is that the evidence does not
need Kitchen. It is also the performance decision: nothing here fans out one
registry round trip per artifact per predicate.

| Field | What it is | Satisfies |
|---|---|---|
| `release`, `build`, `image` | Which artifact | GR-B1 |
| `repository`, `digest` | Where it lives and what it is, by content | GR-D1 |
| `attestedAt`, `keyID` | When the platform attached its own build record, and under which key | GR-D1 |
| `evidence[].predicateType` | What is attached: SLSA's URI for provenance, SPDX's or CycloneDX's for a bill of materials, one of Kitchen's own for a claim no standard covers | GR-B1, GR-D1 |
| `evidence[].manifest` | The manifest digest to fetch it by, without listing anything | GR-I4 |
| `evidence[].source` | `builder` for a claim the build process made and the platform countersigned, `platform` for one the reconciler made alone. The signature cannot tell them apart, and the difference matters | GR-D1 |
| `gates[].name`, `phase`, `finishedAt`, `message` | What each quality gate did — which is not the same question as what it found; what it found is in its attestation. A gate that ran and could not be attested is worth telling apart from one that never ran | GR-D4 |
| `gates[].source`, `reportedBy`, `predicateType`, `attested` | `platform` for a gate Kitchen ran, `external` for one ingested from an application's own CI, with the identity that submitted it and where its findings were signed | GR-D4, GR-E5 |
| `vex[].author`, `submittedBy`, `submittedAt`, `statements`, `digest` | The OpenVEX documents somebody attached. The document names its own author; the authenticated identity that handed it to the platform is a second and different fact | GR-D4 |
| `newestScan.decisionID`, `scannedAt`, `dataSnapshot`, `verdict`, `environment` | What the platform's own policy last concluded — the newest re-evaluation, not a list. An artifact is judged on its newest scan (`policy.NewestVulnerabilityScan`), so the pack says what the policy says | GR-D4, GR-F2 |
| `environments[]` | Where this artifact is running now | GR-B1 |
| `fetch` | The `cosign` command that pulls the evidence itself | GR-I4 |
| `message` | Why an artifact carries nothing — no digest, or a build no longer in the cluster | GR-L1 |

### `exceptions` — the break-glass register

| Field | What it is | Satisfies |
|---|---|---|
| `name`, `environment`, `release` | The grant and what it applied to | GR-L4 |
| `ruleIDs[]` | Exactly which rules it waived | GR-L4 |
| `reason`, `incidentRef` | Why, in the requester's own words, and the incident it belongs to | GR-L4 |
| `requestedBy`, `approvedBy` | Two people. The escalation ladder decides how senior the second must be, and it rises with the requested duration | GR-E2, GR-L4 |
| `createdAt`, `expiresAt`, `resolvedBy`, `resolvedAt` | Its life. Every grant has an expiry; there is no permanent waiver | GR-L4 |
| `autoRollback` | Whether expiry rolls the environment back | GR-L4 |
| `phase`, `activeAtRangeEnd` | Judged at the range's end, so a historical pack does not change its mind | GR-L4 |
| `usedBy[]` | Every promotion that actually relied on the waiver — the difference between a grant somebody asked for and a rule somebody waived | GR-L4 |

A grant is in the pack when its life overlapped the window: created before `to`,
and expired or resolved no earlier than `from`.

### `drift` — what is running that no longer meets its bar

| Field | What it is | Satisfies |
|---|---|---|
| `current[]` | The same derivation `GET /compliance/drift` makes, restricted to this project — one row per deployed pair, compliant ones included | GR-D4, GR-F2 |
| `current[].status` | `compliant`, `waived`, `newly-failing`, `waived-at-promotion`, `not-evaluated`. Five words rather than a boolean, because collapsing an expired waiver into "newly failing" would send somebody hunting a CVE that was never there | GR-D4, GR-L4 |
| `current[].rules[].since` | `rescan` for a rule that started failing after promotion, `promotion` for one that fired then too | GR-D4 |
| `current[].rules[].exception` | The grant waiving this rule in the evaluation being reported | GR-L4 |
| `current[].rules[].waivedAtPromotion` | The grant that waived it *when the release was promoted* — on a blocked row, the one that has since run out, and the reader's next stop | GR-L4 |
| `current[].verdict`, `scannedAt`, `dataSnapshot`, `findings`, `decisionID` | The last re-evaluation of this pair: what it said, when, against which vulnerability database, and the decision to go and read | GR-D4 |
| `current[].promotedVerdict`, `promotedAt` | What was decided when the release was let in — the other half of every comparison this section makes | GR-D1, GR-D4 |
| `current[].scanFailed` | Why the most recent attempt did not run, where it did not. A row answering with something older than the failure has to say so | GR-D4 |
| `history[]` | Every re-evaluation stored inside the window, oldest first: decision id, when, the pair, the verdict, the database snapshot | GR-D4, GR-F2 |
| `history[].unwaived` | How many rules fired without a grant covering them — the number that decides whether a verdict is a finding or a formality | GR-D4, GR-L4 |
| `history[].waived[]` | The grants that made the difference | GR-L4 |
| `counts` | Each status against how many pairs carry it | GR-D4 |
| `note` | That `current` is now and not the range's end, and why | GR-L3 |

### `auditLog` — the project's slice of the tamper-evident log

| Field | What it is | Satisfies |
|---|---|---|
| `items[].sequence`, `timestamp`, `actor`, `actorKind` | Each record. The sequence is dense: a gap is a deletion, not a skipped number | GR-D8 |
| `items[].correlation` | What ties every record that came out of one cause together — a push that built, released and promoted is one correlation across three objects | GR-D1 |
| `items[].operation`, `kind`, `name`, `fromState`, `toState`, `reason`, `details` | What happened | GR-D1, GR-D8 |
| `items[].privileged`, `privilegeClass` | Whether the record moved a *control* rather than a workload, and which: `break-glass`, `requirements`, `classification`, `access`, `credential`, `integrity`. Lifted out of the details for reading; the details still carry them verbatim, because that is what the chain covers | GR-D8, GR-E3 |
| `items[].prevHash`, `hash` | The chain. Shown because hiding them would be asking to be believed | GR-D8 |
| `privileged` | How many of the records moved a control — the count an examiner reads first | GR-E3 |
| `anchor` | Where the chain ends according to an object *outside* the table. A tail cut off the end rehashes perfectly, so this is the only way it is visible at all | GR-D8 |
| `truncated`, `limit`, `message` | Whether the window held more records than one read returns | GR-L3 |
| `note` | That platform-level records carry no project and are therefore not in a project's pack — they are in `GET /audit` | GR-D8 |

### `signedRecords` — the statements with no registry to live in

The one section that is evidence rather than an index: the envelopes are here
**byte for byte**, so each verifies out of the pack with no store to ask. The
payload inside an envelope is what its signature covers, so nothing here is
ever re-encoded.

| Field | What it is | Satisfies |
|---|---|---|
| `items[].id`, `timestamp` | The record | GR-L1 |
| `items[].type` | The predicate: `…/attestation/data-class/v1` for a provisioned resource's provenance declaration, `…/attestation/access-review/v1` for a closed recertification cycle | GR-G4, GR-E2 |
| `items[].subject` | The identity digest the statement is about — a claim or a cycle has no OCI repository, so the subject is sha256 over its namespace, name and UID | GR-L1 |
| `items[].envelope` | The DSSE envelope, verbatim | GR-I4, GR-L1 |
| `truncated`, `message` | Whether more matched than one read returns | GR-L3 |
| `note` | How to verify one on its own, and that this table carries no retention at all | GR-G6, GR-I4 |

## Limits, and what a large project does about them

The two store reads are capped at the store's own maxima — 1000 decisions and
1000 audit records — and a pack that hit either says so in that section's
`truncated` and `message` rather than answering short. The answer to a window
that hits a cap is two packs over two narrower windows; they tile, because the
range is half-open.

A pack carries every decision's full canonical input, which is what makes the
verdicts in it re-runnable rather than something a reader has to accept. That
is also what makes a pack large: a busy quarter is comfortably a megabyte, and
the largest a single pack can be is a few tens of them. It is a document, not
a page.

## Performance

Assembly makes a **fixed** number of reads that does not grow with the
project's history: five list calls against the operator's cache, three store
queries, and two narrow store queries per deployed environment for the drift
derivation. Nothing fans out per artifact, per attestation or per decision.

Measured in `TestATypicalProjectsPackIsWellInsideAMinute`: a 728 KB pack over
four environments, 62 releases, a full page of 1000 decisions with their inputs
and a full page of 1000 audit records assembles in **11 ms**, excluding the
store's own latency. The criterion is a minute.

## From a terminal

```sh
kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01
```

See [CLI.md](../CLI.md). The command writes the pack and, unless `--no-signature`
says otherwise, its DSSE envelope beside it; `--format html` writes the reader's
version instead, and `--json` prints one object naming the files it wrote and
the digest, so a scheduled job can check what it just produced.

## From the dashboard

**Platform → Audit → Evidence export.** Pick a project, pick a window, press
Export. The screen shows what the pack contains, the digest, the verification
procedure and any coverage warning, and offers all three files. An auditor
pressing a button is the entire point of "a request, not a project".
