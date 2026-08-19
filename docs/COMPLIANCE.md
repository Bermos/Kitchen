# Kitchen — Compliance Design

> Status: **phase 1 implemented** (issues #126, #127). Phases 2–6 are designed
> for and not built; this document is written so that adding them is
> additive — each one attaches to something phase 1 put in place, and the
> places where it attaches are named.

Kitchen is not, and cannot be, "FINMA compliant". Compliance is a property of
an institution, not of software. What Kitchen can demonstrate is that a
platform which owns one reconciled graph can produce the evidence compliance
requires **as a byproduct of deployment**, rather than as a project somebody
runs afterwards by assembling four disconnected systems by hand.

Swiss banking supervision is the reference regime here because it is specific,
demanding, and mostly just good engineering hygiene written down by someone
with enforcement powers. Nothing below is Swiss-specific in its mechanism.

---

## 1. The thesis

Four ideas, in dependency order. Each is close to worthless without the one
before it.

1. **The artifact is built once and never rebuilt.** A rebuild produces a
   different artifact, and every claim about the old one is now a claim about
   something that no longer exists.
2. **Evidence accretes onto that artifact as signed attestations.** DSSE
   envelopes wrapping in-toto Statements, attached to the content digest
   through OCI referrers. Verifiable *without* Kitchen — which is also the
   exit story.
3. **Environments declare what they require; artifacts either satisfy it or
   don't move.** The environment owner sets the bar, the application team
   cannot grant itself eligibility. Segregation of duties expressed as a
   schema.
4. **The same policy runs at promotion and on a schedule.** An artifact
   compliant in March is not necessarily compliant in June.

Phase 1 builds (1) and (2), plus the record that makes any of it auditable at
all.

## 2. Design rules that are not negotiable

- **Gates record facts, policies decide verdicts.** A scanner emits findings;
  the environment decides what is disqualifying.
- **Policy inputs are fully materialized before evaluation.** No fetching from
  inside a policy, ever — a decision that reached out during evaluation cannot
  be replayed against a historical artifact.
- **Every decision stores its reproduction inputs.** Bundle digest, input
  digest, data snapshot, timestamp, rules fired.
- **Never block an emergency deployment.** A blocked hotfix means somebody
  bypasses the platform and deploys by hand, and then there is no record at
  all. Allow it, require a reason, record it loudly.
- **Developers never wrestle with any of this.** Gate configuration and policy
  live on the platform operator's side.

## 3. What is explicitly out of scope

Institutional obligations. A platform can carry inputs for them, never the
obligation itself: identification of critical functions and disruption
tolerances, outsourcing materiality determination, what counts as critical
data, the incident reporting decision, business impact analysis, backup and
restore of application state.

---

## 4. The audit log (issue #126)

Every other item in the suite produces decisions. Without an append-only,
tamper-evident record of those decisions, the platform has telemetry but not
evidence. It is deliberately first: retrofitting a log across N reconcile paths
after they exist is miserable, and designing it in now is nearly free.

### 4.1 What is recorded

One row per state transition, in `audit_log` in the telemetry store:

| | |
|---|---|
| `sequence` | position in the chain, from 1, dense |
| `timestamp` | when, at millisecond precision |
| `actor` / `actor_kind` | who: an identity from the identity provider, or `system:controller/<name>` |
| `correlation` | what tied this to the other records from the same cause — for a deploy, the commit |
| `operation` | `create`, `update`, `transition` or `delete` |
| `kind` / `namespace` / `name` / `uid` | which object |
| `project` | which project's, where it belongs to one |
| `from_state` / `to_state` | the move |
| `reason` | the one line a person reads |
| `details` | JSON, whatever the transition is incomplete without |
| `prev_hash` / `hash` | the chain |

The actor is never "the operator". A transition the platform decided on its
own is still attributable — to the named reconciler that decided it.

### 4.2 Why it is not the activity feed

Kitchen already has an activity feed in the same store (`events`). It is prose
for a person catching up: best-effort, written asynchronously, dropped silently
when the store is down. That is the right design for a feed and the wrong one
for evidence. The audit log answers to a different standard — an append that
fails fails the transition that caused it — so it is a different table.

### 4.3 The chain, and what it does and does not prove

Each record's hash covers the previous record's hash and every field of its
own content, length-prefixed so that no field's content can impersonate a
field boundary.

This catches an editor who has the store but not the chain:

- a **mutated** row no longer hashes to the hash stored beside it;
- a **deleted** row leaves a gap *and* orphans its successor's `prev_hash`, so
  one deletion shows up twice;
- an **inserted** row has nowhere to link.

It does **not** catch someone who can rewrite the whole tail: recomputing every
hash from the edit onwards is exactly as cheap for an attacker as it was for
the platform. What bounds that is an anchor kept outside the table — the head
object described below says where the chain ends, so a log cut short from the
end is visible without reading the log at all. Anchoring further out (a
transparency log, an operator-signed checkpoint) is the natural next step and
is deliberately not in this first cut.

### 4.4 One appender, across replicas

A chain needs its appends serialized: the next hash is a function of the last
one, so two appenders that both read head *N* produce two records numbered
*N+1*, each of which verifies and neither of which is the log.

An in-process mutex is not enough. The manager's reconcilers run under leader
election, but its REST API answers on **every** replica, and the API has to
append too — a deletion of an object that carries no finalizer leaves nothing
behind for a reconciler to observe, so the record made at the point of the
request is the only record there will ever be.

So the sequence number is claimed through a ConfigMap (`kitchen-audit-head`)
and the API server's own optimistic concurrency does the serializing: read the
head, compute the next record, write the head back at the resourceVersion it
was read at. Exactly one writer wins a contested round; the losers retry
against the new head. That is one Kubernetes round trip per audit record, which
is affordable precisely because audit records are human-scale — deploys, edits,
promotions, not requests.

If the store insert then fails, the number is given back, so a store that was
briefly unreachable costs a retry rather than a hole. If something has already
appended on top, the gap stands and the verifier reports it, which is the
honest outcome.

### 4.5 Recording is on both sides of a change

- **Reconcilers** record the transitions they *observe*. This is the stronger
  claim: a change made with `kubectl` behind the platform's back lands in the
  log too.
- **The REST API** records its own writes, before making them, attributed to
  the authenticated caller.

Object lifecycle writes (create, update, delete) are attributed to whoever
asked for them, through the `kitchen.bermos.dev/requested-by` annotation the
API already writes. Phase moves are not: the annotation outlives the request
that wrote it, and a build that goes `Succeeded` minutes later did so on the
platform's account, not the person's.

### 4.6 Failing closed

`audit.Recorder.Record` returns an error, and a non-nil error means the
transition was **not** recorded and the caller must not make it. A reconciler
returns the error and requeues; the API answers 503 and the change does not
happen. Over-recording — a transition recorded whose mutation then failed and
was retried — is the acceptable direction to fail in. A mutation nothing
recorded is not.

Two cases are not errors, because both are "there is no log": the feature is
turned off, or the installation has no telemetry store. Both are reported on
`Kitchen.status.compliance.audit` and on `GET /api/v1/compliance`, rather than
failing every reconcile on the platform. **The failure mode of evidence is
silence**, so the platform says so on its own object instead of waiting for an
auditor to notice.

### 4.7 Retention

`spec.compliance.audit.retentionDays`, defaulting to 365 with a floor of 90 —
deliberately not the telemetry retention. Telemetry ages out in weeks; the
evidence an incident is reconstructed from must not go with it. The floor is
not a rounded-up guess: an incident reporting duty runs from when an
institution *became aware*, which can be well after the transition that caused
it, and a log that has already aged out cannot substantiate the report.

### 4.8 Reading it

- `GET /api/v1/audit` — a page of the log, newest first, filterable by
  `kind`, `name`, `namespace`, `project`, `actor`, `since` and `until`. The
  chain fields come back with every record: an audit view that hid them would
  be asking to be believed, which is the thing the chain exists to avoid.
- `GET /api/v1/audit/verify` — re-derives the hashes over a run and reports
  every break, together with the anchor. A run that starts partway through is
  linked to the record before it; without that, a tail lifted out of another
  chain would verify.

---

## 5. Artifact identity and attestations (issue #127)

### 5.1 Standards, not a Kitchen table

Evidence is stored as **DSSE envelopes** wrapping **in-toto Statements**,
attached to the image's content digest through **OCI 1.1 referrers**. Nothing
about it is Kitchen-shaped on the wire.

This is deliberate, and the reason is the honest engineering answer and the
exit story at once: evidence must be verifiable *without* Kitchen. An
installation that stops using Kitchen keeps its evidence, because the evidence
never lived in Kitchen.

`payloadType` is inside DSSE's pre-authentication encoding, so it is signed
along with the payload. That is what stops a signature over an SBOM from being
replayed as provenance, and it is why this is DSSE rather than something
smaller written here.

### 5.2 Where it is written

One manifest, findable two ways:

- its `subject` is the artifact, which is the OCI 1.1 referrers relationship —
  a registry that implements the referrers API answers "what refers to this
  digest" directly, and one that does not is served the same answer out of the
  fallback tag the spec defines;
- it is also pushed under `sha256-<hex>.att`, cosign's attachment name, so
  `cosign download attestation` and `cosign verify-attestation --key` work
  against what Kitchen writes with Kitchen out of the loop.

Attaching is idempotent by content: an envelope whose bytes are already there
is recognised and not attached twice, so a reconcile that runs again does not
grow the evidence set. Two envelopes that assert the same thing but were signed
at different moments are different bytes and both are kept — they are two
assertions.

### 5.3 What Kitchen asserts today

`https://kitchen.bermos.dev/attestation/build-record/v1`: the reconciler's
account of how the artifact came to be — project, commit, branch, strategy,
detected framework, start and finish, and the platform version.

It is **not** SLSA provenance and does not claim to be. Provenance is a
statement by the thing that did the building, about a build it can vouch for
from the inside; this is the reconciler's statement about a build it
orchestrated, which is a weaker and different claim. Conflating the two would
be the kind of evidence that is worse than none — so this carries a Kitchen
predicate type, and issue #128's provenance will carry SLSA's.

A predicate type under `kitchen.bermos.dev/` is an admission that no standard
covers the claim. The list is short on purpose. Two more are reserved for
later phases: `deployment/v1` (a release of this artifact went live on a named
environment) and `promotion-decision/v1` (a policy decision, with everything
needed to replay it).

### 5.4 Failing to attest does not fail the build

The image exists and the deployment that follows is honest about what it is
running. What an unattested artifact cannot do is satisfy a policy that
requires evidence — which is where the consequence belongs, and where phase 3
puts it. A build that could not be attested says so on
`status.artifact.message`, and the platform's compliance status says whether
signing works at all.

### 5.5 Key custody

The signing backend is a Go interface with one method, because key custody is
the one part of this scheme an institution cannot delegate: an adopter whose
rules put the key in an HSM implements `attestation.Signer` against the HSM and
changes nothing else.

What ships is a local ECDSA P-256 key, generated once into
`kitchen-attestation-key` and kept across upgrades, or brought by the
installation through `spec.compliance.attestation.signingKeyRef`. An
installation that named its own key is never handed a generated one: a key that
appeared because a secret was missing is a key nobody's custody rules cover.

P-256 with SHA-256 and ASN.1 signatures is what the rest of the supply-chain
tooling defaults to, which matters more here than any argument about curves:
evidence nothing else can check is not evidence.

Rotation is a manual act — replacing the secret — rather than something the
operator does on a schedule. Every attestation ever signed under the old key
stays checkable only for as long as its public half is still published, so
rotating is a decision about the evidence already out there, not about the key.

The **public** half is served by `GET /api/v1/compliance`. That is not the API
reading a credential back: a public key is public by construction, and evidence
signed under a key nobody can obtain is evidence nobody can check.

### 5.6 Reading it

`GET /api/v1/builds/{name}/attestations` returns the materialized evidence set
for the build's artifact: every attestation attached to the digest, decoded,
and marked verified where a signature was accepted by the platform's key. An
evidence set gathered with no keys reports itself as a listing rather than a
verification — a reader that could not tell the two apart would eventually
treat one as the other.

The endpoint is a convenience and says so. Everything it returns is in the
registry, keyed to the digest, and readable by anything that speaks OCI
referrers.

---

## 6. Configuration

```yaml
kitchen:
  compliance:
    audit:
      enabled: true
      retentionDays: 365       # minimum 90
    attestation:
      enabled: true
      signingKeySecretName: "" # empty: the operator generates one
```

Both live on the platform singleton rather than on a Project, because both are
the operator's word rather than the application team's. A team that could turn
its own audit log off, or sign its own evidence with a key it chose, would be
attesting to nothing.

---

## 7. Phases

| | |
|---|---|
| **1 — Foundations** | audit log (#126), artifact identity (#127) — **built** |
| **2 — Evidence production** | provenance + SBOM (#128), PR verification (#129), quality gates (#130) |
| **3 — Policy** | environment ownership (#131), OPA engine (#132), staged promotion (#133) |
| **4 — Continuous compliance** | rescan (#134), OpenVEX (#135), exceptions (#136) |
| **5 — Institutional surface** | data class (#137), resource contract (#138), access (#139), retention (#140), criticality (#141), export (#142) |
| **6 — The mapping doc** | #143, kept current |

Phase 2 attaches to §5: a provenance or SBOM attestation is another envelope
against the same digest, and the store already accumulates them. Phase 3
attaches to §5.4: an environment that requires evidence reads the evidence set
and refuses an artifact that does not carry it. Phase 4 attaches to §4: a
re-evaluation is a decision, and a decision is an audit record.

---

## 8. Things that are true and easy to get wrong

- **A gap in the sequence is not always a deletion.** It is also an append that
  claimed its number and then died before the row landed. The head object and
  the operator's log are the tie-breaker; the verifier reports the gap either
  way, because reporting it is the safe direction.
- **The hash is over the record as it will be read back**, not as it was handed
  in. The timestamp is truncated to the millisecond the column stores before it
  is hashed — hashing precision the column cannot hold would make every record
  fail verification after a round trip.
- **An attestation is attached to a digest, never to a tag.** A tag is a moving
  target, and evidence about a moving target is evidence about nothing. The
  store refuses a tag reference outright.
- **The evidence set is read from the registry, not from Kitchen.** Nothing
  mirrors the attestation list onto the Build object: a copy there would be a
  second source of truth, and it is exactly the copy an installation leaving
  Kitchen would lose.
- **`kitchen-audit-head` is load-bearing.** Deleting it does not lose the log —
  it is re-seeded from the table's own last record — but it does lose the
  anchor that would have shown a truncated tail.
