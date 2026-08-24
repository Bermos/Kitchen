# Kitchen — Compliance Design

> Status: **complete** (issues #126 through #143). Every phase attaches to
> something an earlier one put in place, and the places where it attaches are
> named — §13, the evidence export, is where all of it is read back out in one
> request, and **§17 is the requirement mapping**: what produces the evidence
> for each supervisory requirement, where it is read, and what it does not
> cover. If you are here to find out what this platform can be shown to
> demonstrate, read §17 first and §17.1 before that.

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

Institutional obligations. A platform can carry inputs for one, never the
obligation itself — and the six below are not a hedge, they are the boundary
the rest of this document is only credible because it draws.

| The obligation | Whose it is, and why it cannot be this platform's | What Kitchen carries for it |
|---|---|---|
| **Identifying critical functions** | A judgement about what the institution does for the outside world, made by people accountable for it. A platform that defaulted, inferred or refused on a designation would become the source of a determination nobody made | `criticality` on Project and Environment, carried and answered as *undesignated* where nobody has decided (#141) |
| **Setting disruption tolerances** | The same judgement, in units of time, and it is the board's | `rto` / `rpo` as declared values — and the RTO is wired to alerting rather than filed, so the number somebody chose is the number that pages them (#141) |
| **Determining that an outsourcing is material** | A determination under the outsourcing circular, made against the institution's own risk appetite | The inventory of what is outsourced and what depends on it: Connections, ResourceClaims, providers, and the reverse traversal from any one of them (`GET /compliance/dependents`) |
| **Deciding what counts as critical data** | A classification of the institution's data, not of a database | `dataClass` and `residency` across the graph, inherited and narrowable, with `unclassified` answered as a word rather than left blank (#137) |
| **Judging that an incident is of substantial importance** | The reporting decision under Art. 29(2) FINMAG and the NCSC duty under Art. 74b ISG. A platform cannot make it, and one that appeared to would be worse than one that does not | The evidence the report is written from, inside the clock that starts at awareness: the audit log, its 90-day floor, and a pack that says when a window has been truncated (§4.7, §12.5, §13.5) |
| **Business impact analysis** | An analysis of the business, done by the business | The dependency graph a BIA would otherwise be assembled from by hand, and the tolerances once they are declared |
| **Backing up and restoring application state** | An application's data is its own; the platform never holds it | The platform's own state, and only that — [BACKUP.md](BACKUP.md) says exactly what one archive holds and what it does not |

§17.5 says the same thing from the mapping table's side: these are the rows
that are absent on purpose, and saying so is part of what makes the rows that
are there worth reading.

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
predicate type, and the provenance in §6 carries SLSA's.

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

## 6. What the builder attests (issue #128)

### 6.1 Two claims, two attestations

Kitchen's build record (§5.3) is the reconciler's account of a build it
*orchestrated*. SLSA provenance is the account of the process that actually ran
it. They are different claims and they stay different attestations.

The distinction is not pedantry, it is the whole value of provenance. Only the
builder knows which base image it really resolved and to what digest, which
source tree it really fetched, and what it was really invoked with. The
reconciler knows what it *asked for*, which is a claim about an intention.

So provenance is not written by Kitchen. It is produced by BuildKit, harvested,
and countersigned.

### 6.2 What is asked of the builder

```
--opt attest:provenance=mode=max,version=v1,builder-id=https://kitchen.bermos.dev/builder/buildkit
--opt attest:sbom=generator=docker/buildkit-syft-scanner:1.12.0
--output type=image,…,oci-mediatypes=true
```

Every part of that line is load-bearing:

- **`mode=max`** records the resolved base images and the build parameters.
  `min` records that a build happened, which is not evidence of anything.
- **`version=v1`** is SLSA 1.0. BuildKit can emit it and **does not default to
  it** — left alone it writes `https://slsa.dev/provenance/v0.2`. Both
  spellings are recognised on the way back in, because an installation that has
  not rebuilt since an upgrade has artifacts carrying the older one.
- **`builder-id`** is the one field BuildKit cannot fill in for itself. Left
  unset it writes an empty string, and provenance that does not say who
  produced it answers the first question a verifier asks with nothing. It
  carries no version: the platform's version is in Kitchen's build record
  against the same artifact, and an identifier that moved every release would
  break every policy that pinned it.
- **`oci-mediatypes=true`** because attestations are pushed as extra manifests
  under an index, which the OCI media types describe and Docker's older ones do
  not. It is set only when something is being attested, so a build with the
  feature off pushes exactly what it pushed before.

### 6.3 The artifact is the image, not the index

Asked for attestations, BuildKit stops pushing a bare manifest. It pushes an
index holding the image manifest and, beside it, an attestation manifest
annotated `vnd.docker.reference.type=attestation-manifest`, whose layers are
bare in-toto Statements. What it reports through `--metadata-file` — and
therefore what the reconciler first sees — is the **index** digest.

**The subject of every statement inside is the image manifest digest.** That
was established by pushing one and reading it back, not by reading the docs.

So the reconciler resolves the index to the image manifest and calls *that* the
artifact: it is what the statements are about, it is what the Release deploys,
and it is the same digest Kitchen used before any of this existed, so turning
the feature on does not renumber the artifacts an installation already has.
Attesting therefore happens *before* the Release is created, because it is what
decides which digest the Release names.

Getting this backwards would not have failed loudly. Evidence attached to the
index while the statements inside describe the image would verify perfectly and
describe something the platform never deployed.

### 6.4 Countersigned, not merely carried

The statements BuildKit leaves are **unsigned**. The index is the only thing
tying them to the artifact, and an index is not a signature — anything with
push access to the repository could write a different one.

Each is therefore restated about the digest Kitchen calls the artifact and
signed under the platform's key. Restating rewrites three things and preserves
one absolutely:

- the subject becomes the repository and digest, because BuildKit names its
  subject with a package URL carrying the tag it pushed to, and a tag is a
  moving target;
- the statement version becomes in-toto v1, because BuildKit still writes v0.1;
- a signature is added;
- **the predicate is copied byte for byte** and never re-marshalled from a
  decoded map. Re-encoding is not guaranteed to reproduce field ordering, and a
  predicate that does not reproduce exactly is a different claim from the one
  the builder made — which would make the platform's signature an assertion
  about something nobody said.

A statement whose subject is some other digest is dropped and counted, never
restated. Signing one would put the platform's name on a claim about an image
it did not build.

`status.artifact.evidence[].source` records which of the two a piece of
evidence is: `builder` or `platform`. The platform's signature is on both, so
the signature cannot tell them apart, and the difference is exactly what a
reader is trying to establish.

### 6.5 The SBOM costs something, and the generator is pinned

Provenance is nearly free — BuildKit already holds everything it records. An
SBOM is not: BuildKit runs a scanner image over the finished filesystem, and
because the build pod is ephemeral **that image is pulled on every build**.

It is pinned to `docker/buildkit-syft-scanner:1.12.0` rather than left on
BuildKit's default `stable-1`, which is a floating tag on an image this project
does not own. Evidence about an artifact should not change because somebody
else's tag moved overnight, and a scanner that changed under an installation
would produce a differently-shaped bill of materials for the same image — which
reads as the image having changed.

**The format follows the generator**, and the platform records what came out
rather than converting it. The default emits SPDX 2.3, which Grype, Trivy and
OSV-Scanner all read unmodified; a generator that emits CycloneDX produces a
CycloneDX attestation whose predicate type says so. Kitchen does not transcode
between them — a bill of materials rewritten by something that did not scan the
image is a claim by the transcoder.

An installation that cannot reach the generator, or will not spend the seconds,
turns the SBOM off in the chart. That is a decision it has made and can show,
rather than one the platform made for it.

### 6.6 The documented limitations

- **A build-time SBOM describes the image, not the running process.** It cannot
  see a dependency loaded at runtime, fetched by the application on start, or
  side-loaded into the container. It is the right input to vulnerability
  management and it is not an inventory of what is executing.
- **Only the Dockerfile strategy produces provenance.** The Cloud Native
  Buildpacks lifecycle is not BuildKit and emits no SLSA provenance, so a
  buildpacks build carries Kitchen's build record and no provenance. Minting a
  SLSA predicate for it from the reconciler's own knowledge is exactly the
  conflation §6.1 exists to prevent, so it is not done.
- **Losing the builder's evidence does not fail the build**, for the same
  reason §5.4 gives. It is recorded on `status.artifact.message`, and an
  artifact with provenance and no SBOM is worse than one with both and far
  better than one with neither.

---

## 7. Quality gates (issue #130)

### 7.1 Facts, never verdicts

A gate emits findings. Whether a finding is disqualifying is a property of the
environment an artifact is being promoted to, not of the artifact — so
`kitchen.bermos.dev/attestation/quality-gate/v1` records the gate, its version,
when it ran and its raw output, and **has no pass field**.

That is what lets the same scan be acceptable in staging and blocking in
production without running the scanner twice. It is also what keeps application
teams out of threshold negotiation: there is no threshold at the point where
they could argue about one. Phase 3's policy engine reads these and decides.

Two tests hold the line rather than a comment: both the gate runner's predicate
and a submitted result's are asserted to carry no `pass`, `passed`, `verdict`,
`ok` or `allowed` field.

### 7.2 Ran and found problems is not failed

A gate that scans an image and reports ninety critical vulnerabilities has
**completed**: it did exactly its job. `Failed` is for a gate that did not run —
the image would not pull, the scanner crashed, the deadline expired — where
nothing is known either way.

Collapsing those two is how a compliance system comes to report green while its
scanners are quietly broken, so the runner keeps them apart by construction: the
gate is an **init container**, and its exit status is only ever a statement
about whether it ran. Nothing passes a scanner the flag that makes it exit
non-zero on findings, and a gate configured with one shows up as every artifact
failing to be scanned rather than as a policy nobody agreed to.

### 7.3 A gate is a pod, and it is given nothing

The gate is an image somebody else wrote, running in an application's
namespace. It gets the artifact reference, a credential to pull it with, and a
directory to write to. It gets no service account token, no cluster access, and
it runs as an unprivileged user with every capability dropped.

Kubernetes' own `$(VAR)` expansion is what points it at the artifact, so a gate
is configured with environment variables rather than with templating of
Kitchen's:

```yaml
gates:
  - name: trivy
    image: aquasec/trivy:0.58.0
    version: "0.58.0"
    format: trivy-json
    args: [image, --format=json, --output=$(KITCHEN_FINDINGS), $(KITCHEN_ARTIFACT)]
```

Gates live on the Kitchen object, not on a Project. A team that chose which
scanners ran over its own code would be marking its own homework — and adding a
gate is a change to that list and to nothing else, which is the acceptance
criterion "adding a gate requires no application-side change" restated.

### 7.4 Getting a megabyte out of a pod

Findings are large. A container scan of an ordinary Node application runs to
several megabytes, and every obvious channel is wrong at that size:

- a pod's **termination message** caps at 4 KiB — which is why the build digest
  fits there and a scan report does not;
- a **ConfigMap or Secret** caps at about a megabyte, and truncating findings
  turns evidence into an opinion about which findings mattered;
- the pod's **log** is shipped by the collector, which is fast but not
  synchronous, so reading it back races the Job finishing — and a race that
  silently shortens evidence is the worst of the three.

So the findings go where large content-addressed blobs already go: the registry
the artifact is in, under the artifact's own repository, with the credential the
pod already holds because it had to pull the image to scan it. A second
container — `cmd/qualitygate`, a third binary in the operator's image — reads
the file, stores it, and reports the digest, which is 71 bytes and fits
anywhere.

**The blob is not evidence while it sits there.** It is unsigned, and anything
with push access could have written it. It becomes evidence in the operator,
where the signing key is and where the gate's image never reaches.

### 7.5 Results produced somewhere else

Many organisations already run their scanners in the application's own CI,
minutes before Kitchen sees the commit. `POST /builds/{name}/gates` ingests such
a result instead of re-deriving it — `kitchen gates submit` is the same call
from a pipeline.

What changes is not the predicate but the **attribution**. A result Kitchen
produced is a claim about an artifact; a result somebody submitted is a claim
about an artifact *and* a claim about who said so. So the statement records
`reportedBy` — the authenticated identity that sent it — and `external: true`,
and the Build records the source as `external`. A policy that trusts only what
the platform ran itself can say so.

The platform's signature still means what it always means: that these bytes were
submitted by that identity at that moment and have not changed since. It is not
a claim that the findings are true. Nothing can sign that.

A submission does not overwrite a gate of the same name that the platform ran
itself. Both attestations are attached and both are readable; what the Build
shows is the one the platform can vouch for having observed.

### 7.6 The awkward parts, said out loud

- **Gates run after the Release exists.** They attach evidence to an artifact
  that is already deployable, and on a project with previews it may already be
  deployed. That is deliberate: blocking a preview on a scanner would make the
  platform slower than the thing it replaces, and the consequence of missing
  evidence belongs at promotion, which is phase 3. It does mean "gated" and
  "deployed somewhere" are not the same thing, and a policy that assumes
  otherwise is wrong.
- **A gate is not retried.** `backoffLimit: 0`, because a retry that eventually
  succeeded would leave a build whose evidence says the scanner works and whose
  history says it did not.
- **The findings blob is left in the registry.** It is content-addressed and
  orphaned once the attestation carries the same bytes, so it is the registry's
  garbage collector's problem rather than a reference anything follows.

---

## 8. How the change was reviewed (issue #129)

### 8.1 Asked while the answer is still true

Four eyes on a production change is the control a supervisor asks about first,
and the usual way of answering it — opening the git provider's UI months later
— is not evidence. It is a screen reflecting the repository as it is *now*, on a
request whose approvals may since have been dismissed, whose reviewers may have
left, and whose branch protection may have been reconfigured twice.

So the platform asks before the build, records what it was told on
`status.source`, and mints
`https://kitchen.bermos.dev/attestation/pull-request-approval/v1` afterwards out
of what was recorded. It does not ask twice: an approval can be dismissed
between a build starting and finishing, and the evidence has to describe the
moment the decision to build was made in.

### 8.2 The provider is asked, not the commit

Because the commit does not know. A **squash merge** produces a new commit on
the default branch whose author is whoever pressed the button and in which the
approver appears nowhere; a **rebase merge** loses the merge commit entirely.
Reading commit metadata — or parsing `(#123)` out of the message — answers
confidently and wrongly on the two merge strategies most organisations actually
use.

`GET /repos/{owner}/{repo}/commits/{sha}/pulls` is the association GitHub
maintains across both, and it is what is used.

### 8.3 Only approvals that still stand

A provider records every review ever left. An approval a later push dismissed is
still in the list with its state changed; a reviewer who approved and then asked
for changes appears twice. So the reduction is: newest review per reviewer, and
only where that newest one is an approval — with `COMMENTED` skipped, because on
GitHub a comment leaves the previous verdict standing.

Getting this wrong produces evidence that a change was approved when the
approval had been withdrawn before it merged, which is worse than no evidence,
because somebody would rely on it.

### 8.4 Self-approval is recorded, not filtered

A change its own author approved *has* been approved. Whether that is
acceptable is a policy question — an installation whose rules permit it on a
two-person team and one that forbids it outright both need to see that it
happened.

So `selfApproved` and `independent` are separate fields, and both are recorded.
The project-level requirement demands `independent`; a policy at promotion can
demand something different.

### 8.5 The exemption is a list, and the list is the point

Renovate merges its own dependency bumps. release-please merges its own release
commits. **This repository's release automation would fail this check on its
first run.** None of them will ever have an independent reviewer, and the
realistic alternative to naming them is somebody switching the requirement off
altogether.

`spec.compliance.machineIdentities` names them, on the platform object rather
than on a Project — a team that could add its own service account to its own
exemption list has no requirement at all. Matching is exact and
case-insensitive, with no patterns: a glob would eventually exempt more than
whoever wrote it meant.

**Every use of the exemption is an audit record**, naming the identity, the
commit and the branch. A configured exemption says who *may* bypass review; the
record says who did, and the second is the one an auditor asks for.

### 8.6 An outage is not a violation

A provider that cannot be reached, a Connection with no such capability, a
credential that expired: none of those are findings about the commit. They are
recorded as a check that could not be made, on `status.source.message`, and they
do **not** refuse the build.

Failing closed there would mean a GitHub outage stopping every deployment on the
platform, including the one fixing it — and the people affected would deploy by
hand, which is the outcome this whole suite exists to prevent. The one case that
does refuse without reaching the provider is a project that requires review over
a connection which cannot report it at all, because that is a configuration
error rather than a transient one.

### 8.7 Refused, and still recorded

The check runs before the Job is created: fast feedback, no wasted compute, and
what "refuse before the build is scheduled" means. The **Build object still
exists** and carries `SourceUnreviewed` and the reason — refusing without a
record would be the platform quietly dropping changes, which is a worse failure
than the one being prevented.

Pull request builds are exempt necessarily: a request's own builds are what
produce the preview the review happens on, so requiring the review first would
deadlock with itself. The requirement applies to the production branch alone.

### 8.8 What is not built

**GitLab.** The acceptance criteria ask for GitHub and GitLab. GitLab and Gitea
are now real providers — a credential probe, webhook registration, verified push
and merge-request deliveries, and the commit status and preview comment posted
back. What neither implements is `gitprovider.ChangeReader`, so nothing about a
GitLab merge request's reviews reaches a decision here.
`ChangeReader` is a capability interface for exactly this reason, in the same
shape as `SourceReader` and `StatusReporter`: a provider lands as a source
first and gains the rest, and a Connection that cannot answer is told apart
from one that answers "no pull request". GitLab's `CommitProvenance` is a
method on a type that exists now and does not implement it.

**Break-glass now exists** (#136). A direct push during an incident used to be
a hard refusal where the project requires review; it is now *allowed and loudly
recorded* when an active Exception covers it, which is what the suite's design
rules demand — a blocked hotfix gets deployed around the platform and leaves no
record at all. The Exception is a bounded, two-person, per-rule grant
([docs/api/exceptions.md](api/exceptions.md)): the rule id for this requirement
is **`require-pull-request`**, and an active exception naming it, scoped to the
project's production environment with no release narrowing, converts the
refusal into a build that proceeds with a privileged audit record, the
exception's name on `status.source`, and `exception`/`exempt` fields on the
signed pull-request-approval attestation — the same shape as the
machine-identity exemption. Everything else about the requirement is unchanged:
no exception, no review, no build.

---

## 9. Continuous re-evaluation (issue #134)

### 9.1 The difference between a gate and a control

Everything up to here happens once. A gate scans an artifact on the day it is
built; a promotion judges that scan on the day it is promoted. Both are
statements about a moment, and both were true when they were made.

An artifact compliant in March is not necessarily compliant in June, and
nothing about the artifact has changed — the world has. **"What is running
right now that no longer meets its environment's bar?"** is the question
almost no institution can answer about its own estate, and it is the question
this section exists to make routine.

So the platform walks every currently-deployed release on an interval, matches
the bill of materials the build already attested against a **current**
vulnerability database, signs what comes out onto the artifact's digest, and
re-runs the environment's own bar over the enlarged evidence set. The artifact
is not rebuilt, not redeployed, not even pulled. What changes is the evidence
attached to it and the decision recorded about it.

### 9.2 The same code path as promotion, and why that is load-bearing

The rescan does not have a policy engine of its own, a materializer of its
own, an evidence read of its own or an exception listing of its own. It calls
`PolicyEvaluator` — the one implementation, which the promotion reconciler was
refactored onto in the same change — with a different `kind`, a different
clock and a data snapshot.

That is not tidiness. Two implementations would eventually disagree about
something small: which claims are in scope, whether an absent field is a
failure, how an unreadable evidence set is judged. The disagreement would
surface as **drift** — a release that "went non-compliant" the moment nobody
touched it — and a compliance control whose findings can be artefacts of its
own second code path is worse than no control, because somebody will act on
one.

The only field the rescan sets that a promotion does not is `dataSnapshot`,
and the eligibility preview deliberately does not set it either: a preview
whose input digest differed from the promotion's by a field the preview
invented would be a second opinion rather than the same evaluation.

### 9.3 A sweep, not a reconciler

The idiomatic shape in this repository is a reconciler with a `RequeueAfter`,
and it is the wrong one here for two reasons, both of which are about the word
*sweep*:

- A rescan must happen **once per interval across the platform**. A Runnable
  that declares `NeedLeaderElection` says so about itself, rather than
  inheriting the claim from the manager.
- The pass has a **platform-wide budget**. The first sweep after an upgrade
  has every environment due at the same instant, and two hundred scanner pods
  pulling two hundred images is a denial of service the platform performed on
  itself. `concurrency` bounds what is in flight, and a per-object requeue has
  no vantage point from which to count.

The per-pair *state* still lives on the object, at `status.rescan`, and the
sweep is stateless between passes. The interval is counted from each pair's
own last **finished** scan, which is what makes it self-spreading: an estate
scanned four at a time stays four at a time instead of re-converging on one
minute of the day. A scan that never finishes delays the next one rather than
doubling it.

### 9.4 The scanner is a pod, and it is given a file

Everything §7.3 says about a gate applies here: the scanner is an image
somebody else wrote, running in an application's namespace, with no service
account token, no cluster access, every capability dropped, `backoffLimit: 0`,
and its arguments taken from the platform operator's own configuration —
nothing from an API request reaches its argv.

What it is *not* given is the artifact. It is given the **bill of materials**,
fetched onto an `emptyDir` by an init container (`cmd/rescan fetch`) with the
credential the pod already holds, and the scanner is pointed at it —
`trivy sbom`, `grype sbom:`, `osv-scanner --sbom`. That is what makes "no
rebuild and no redeploy" literally true rather than nearly true: the image is
never pulled, so a scan costs a scanner pod and an SBOM, not a layer download
per environment per day.

The findings come back the way a gate's do and for the same three reasons
(§7.4): a registry blob under the artifact's own repository, reported as a
digest through the pod's termination message by a second container
(`cmd/rescan publish`). They are unsigned while they sit there. They become
evidence in the operator, where the key is.

`backoffLimit: 0` here has a second edge over §7.6's: the retry is the next
interval, and it is honest about being a different scan against a different
database.

### 9.5 The snapshot is what makes a finding reproducible

`kitchen.bermos.dev/attestation/vulnerability-scan/v1` records the scanner,
its version, when it ran, the findings, and the **vulnerability database
snapshot** they were produced against. Without the last one a scan can be
repeated but never reproduced: matching the same SBOM tomorrow is a different
question, and a decision that cannot say what the world knew when it was made
is a decision nobody can check.

Three sources, in descending order of what the platform can stand behind:

1. what the scanner wrote to `$(KITCHEN_DATA_SNAPSHOT)` — its own word about
   its own database;
2. what its report carries. Grype's `descriptor.db` is the only one of the
   three understood formats that says;
3. the scanner and the day, prefixed **`unpinned:`**. That is not a database
   identifier and does not pretend to be, and the prefix is there so a reader
   can tell a snapshot that reproduces a scan from one that merely dates it.

The snapshot travels on the attestation, on `status.rescan`, on the stored
decision (`data_snapshot`), and in the decision's audit record.

### 9.6 Findings are normalized, and the raw report is signed beside them

This is the one place the platform's "never transcode evidence" rule (§6.5) is
bent, deliberately and in one direction.

The reason is that phase 3 already fixed the contract: the default bundle's
`max-severity` rule reads `predicate.findings[]` and wants `.severity` and
`.vulnerability`, and no rule can be written once against three scanners whose
reports agree about nothing. So the signed statement carries **both** — the
scanner's own bytes verbatim under `report`, and the platform's reading of
them under `findings` — and the platform's signature covers exactly that
division. A report shape nobody recognises yields no normalized findings at
all, and a policy that requires a scan then fires on an artifact whose
evidence the platform could not read, which is the honest outcome rather than
the quiet one.

Like a gate's, the predicate carries **no verdict**. Whether a finding is
disqualifying is still the environment's question.

### 9.7 Exception expiry, and no expiry engine

This pass is the only thing that judges exception expiry, and it needs no
machinery for it. `ActiveExceptionsFor` excludes an expired grant, the rules
it waived fire unwaived, and the verdict is Blocked. The exception controller
is the clock and this is the consequence.

`spec.autoRollback` is acted on here and nowhere else — the exception
controller carries the flag and deliberately leaves the acting to the pass
that has a re-evaluation in hand. It fires on all four of: the verdict is
Blocked, an exception covering this pair has expired without being resolved,
it asked for the rollback, and it waived a rule that is now firing unwaived.
There must also be a previous release to go back to. Anything looser would
turn a grant somebody took out to ship a hotfix into a mechanism that yanks a
running workload for an unrelated reason, which is the failure §2's design
rules call worse than the disease. The audit record comes first and is
fail-closed; the default is off.

### 9.8 Drift is a join, not a table

`GET /api/v1/compliance/drift` derives its answer on the request, from the
cluster's current state and the decision register. Nothing about it is stored,
because everything it needs already is.

It exists to draw one distinction, between two things that look identical on a
blocked verdict:

- **newly failing** — a rule that did not fire when this release was promoted
  and fires now. Nothing about the artifact changed; a vulnerability database
  did.
- **failing at promotion under exception** — a rule that fired at promotion
  too, waived by a break-glass grant that has since run out. Nothing new was
  discovered; a decision somebody made deliberately, with an expiry, reached
  its expiry.

Collapsing them would make an expired waiver read as a new vulnerability and
send somebody hunting for a CVE that was never there. So `status` is one of
five words — `compliant`, `waived`, `newly-failing`, `waived-at-promotion`,
`not-evaluated` — and every rule in the answer repeats the distinction in its
`since` field.

`not-evaluated` is the one that matters most and is easiest to leave out. A
pair nothing has re-checked is a finding about the *platform*, not about the
release, and it is never counted as compliant. So is a pair whose **last scan
attempt did not run**, which is the same failure wearing a disguise: the
verdict on a row comes from the newest stored *decision*, which is the last
scan that succeeded, so a pair scanned clean on Monday whose Tuesday scanner
could not pull its image would otherwise read `compliant` with a `scannedAt`
from before the failure. Such a row carries `scanFailed` and is
`not-evaluated`; a row that was already blocked stays blocked, because a failed
scan does not soften a finding, it only refuses to invent a clean one. For the
same reason the answer leads with `rescanning`: an empty drift view under a
pass that is off means *nobody is looking*, which is not the same answer as
nothing being wrong.

Historical scans are retained without anything special being done about it:
every rescan is a stored decision, timestamped, under the audit retention
(§4.7) rather than the telemetry one, and every scan's findings stay attached
to the artifact's digest in the registry. `GET /api/v1/decisions?kind=rescan`
is the history; the drift view is only its newest row.

### 9.9 The awkward parts, said out loud

- **Base-image drift after build is not detected by SBOM rescan alone.** The
  bill of materials describes the image as it was built. If the base image's
  publisher ships a fixed package under the same tag, the running artifact
  still contains the old one and the SBOM still says so — which is correct.
  But the reverse is the gap: a vulnerability *introduced* into the base image
  after the build is invisible here, because nothing rescans the base image's
  own supply chain, and so is anything the SBOM never described (§6.6's first
  limitation, now on a schedule). Detecting that needs a scan of the image
  filesystem, not of its bill of materials, and that is a different and much
  more expensive pass — a gate configured to run over the image, or a
  registry-side scanner. It is not this.
- **A rescan re-attests nothing about the decision.** The findings are
  attested; the verdict is a stored decision and an audit record and stops
  there. A promotion-decision attestation on every daily rescan would put a
  year of near-identical statements on every artifact, and the register is
  already the record.
- **The newest scan is the current claim, and the older ones are history.**
  `status.artifact.evidence` is an index (§16), so a rescan replaces the
  vulnerability-scan entry rather than appending one — and the *evaluation*
  reads the same way: `policy.EvidenceFrom` collapses the vulnerability-scan
  predicate to its newest entry before the rules see any of them, ordered by
  `scannedAt` and tied on the envelope digest so the input digest is
  reproducible. Nothing else is collapsed, because nothing else is a
  restatement: a gate result, a provenance, an SBOM and a VEX document are each
  a distinct claim made once, and dropping the older of two of those would be
  dropping a fact. Feeding every scan to the rules would leave a CVE that day
  one reported and day forty does not — withdrawn, re-rated, fixed in the
  database — still firing `max-severity`, escapable only by a VEX statement
  about a finding that no longer exists. The registry holds every scan
  regardless, and `GET /api/v1/decisions?kind=rescan` is the history.
- **Nothing here refuses a deployment.** The pass records, surfaces and — for
  a grant that asked for it — rolls back. A blocked rescan does not stop the
  running workload, and that is deliberate: the consequence of missing
  evidence belongs at promotion, and yanking production because a database
  updated overnight is how a compliance control gets switched off.

---

## 10. Exploitability (issue #135)

### 10.1 What makes §9 survive a real dependency tree

The rescan pass asks a current vulnerability database about a bill of
materials nobody has touched. On a real application it comes back with
somewhere between eighty and four hundred findings, of which a handful are
reachable from anything the process actually runs. That is not a defect of the
scanner; it is what a transitive dependency graph looks like when it is matched
against every advisory ever published about it.

A control that reports four hundred things every morning is read for a week
and then rubber-stamped, and a rubber-stamped control is worse than no control,
because somebody is relying on it. **VEX is what makes the other three hundred
and ninety stop asking.** It is the vendor's or the security team's assertion
that a vulnerability which is genuinely present in the image is not exploitable
in it: the component is not present in the running application, the vulnerable
function is not in the execute path, an inline mitigation already covers it.

So this is not a nice-to-have bolted on after the rescan controller. It is the
half of the rescan controller that makes daily re-evaluation something an
institution can actually leave switched on.

### 10.2 The document is the predicate, and the URI is OpenVEX's

An ingested document is attached to the artifact's digest as a signed
attestation like everything else in §5 — and the predicate type is
`https://openvex.dev/ns/v0.2.0`, which is OpenVEX's own context URI and not
one of Kitchen's.

That follows §5.3's rule rather than bending it: a Kitchen predicate type is an
admission that no standard covers the claim, and OpenVEX covers this one with a
specification, a vocabulary and tooling that already reads it. Minting
`kitchen.bermos.dev/attestation/vex/v1` for the same assertion would produce
evidence only Kitchen could interpret, which is the exact opposite of why this
layer is standards all the way down.

The predicate is the submitted document **byte for byte** — decoded for
validation, never re-encoded — for the reason §6.4 gives about a harvested
provenance predicate. A statement rebuilt from a decoded map reorders keys and
renumbers numbers, and the platform's signature would then be over a claim
nobody made. Older context spellings are recognised by prefix, because a
document is written by whatever tool its author runs and the first drafts of
the specification are still in circulation.

### 10.3 `not_affected` has to say why, and from a list

OpenVEX permits `not_affected` to be explained by an `impact_statement` in free
text. **Kitchen refuses that**, at ingest and again in the default bundle.

The reason is the same one the exception register exists for. "The vulnerable
code is not in the execute path" is a claim a reviewer can check against the
artifact and an auditor can count across a hundred suppressions; "we looked at
it and it's fine" is a sentence. A suppression whose reason cannot be counted
cannot be reviewed in aggregate, and a register of a thousand of them is a
register of nothing.

So a `not_affected` statement carries one of the five:
`component_not_present`, `vulnerable_code_not_present`,
`vulnerable_code_not_in_execute_path`,
`vulnerable_code_cannot_be_controlled_by_adversary`,
`inline_mitigations_already_exist`. Free text is carried too — `status_notes`
and `impact_statement` are shown wherever the statement is — beside the
justification rather than instead of it.

The check is in **two** places on purpose, which is not duplication. The API
refuses an unjustified document at ingest, with a message naming the
enumeration and saying where prose belongs. The bundle refuses to act on one
whatever attached it, because `Store.Evidence` merges the registry's referrers
listing with cosign's attachment tag: a VEX document pushed with
`cosign attest` by something that never spoke to Kitchen is visible to the
policy engine, and a rule that trusted the ingest path to have already checked
would be trusting a path that document never took.

### 10.4 Whose word it is, recorded twice

§7.5's model — *what changes is not the predicate but the attribution* —
applies here and matters more, because a submitted scan result is a claim about
what a tool found and a submitted VEX statement is a claim that something does
not count.

Two facts are kept apart and neither substitutes for the other:

- **The author** is the document's own claim about itself. OpenVEX requires
  one, and a statement may name its own `supplier` where a document collects
  somebody else's assertions — an aggregator's name on a vendor's statement
  would attribute it to the wrong party, so authorship is resolved per
  statement.
- **The submitter** is the authenticated identity that handed the document to
  the platform, which is the only half the platform witnessed. It is recorded
  on `Build.status.vex[]` and in the audit record, and it is never written into
  the document.

The audit record comes **before** the attach and is fail-closed, like every
other write the API makes: it names the caller, the author, the document id,
and every vulnerability and justification asserted, so that "what has been
waived on this platform and by whom" is answerable from the log alone without a
registry to hand. The Build's index carries the same attribution for an
installation whose audit log is off, which §4.6 permits.

The platform's signature means what it always means: that these bytes were
submitted by that identity at that moment and have not changed since. It is not
a claim that the assertion is true. Nothing can sign that.

### 10.5 Whose word is believed, at two levels

Trust is a different question from attribution, and it is answered twice
because it has two legitimate owners.

- **The platform admits.** `compliance.vex.trustedAuthors` on the singleton is
  the closed list of document authors anything may be attached for, and
  `compliance.vex.enabled` turns the door off altogether. It is the operator's
  word, on the operator's object, for the reason §7.3 gives about gates: an
  application team that decided unaided whose word suppressed its own
  vulnerabilities would be marking its own homework at the one point where the
  marking matters.
- **The environment believes.** `parameters["vexRequireVerified"]` (**on by
  default**) refuses a statement whose envelope no key the platform holds
  accepted, `parameters["vexTrustedAuthors"]` narrows to named authors —
  matched exactly and case-insensitively, the same rule the platform's own list
  uses, so an address copied from the singleton into an environment's
  parameters means the same thing in both places — and
  `parameters["vexMaxAgeDays"]` bounds how long a statement stays current
  without being restated. Production can be stricter than the platform;
  nothing can be looser than it.

`vexRequireVerified` defaulting to on is the answer to "policy can be
configured to reject VEX from untrusted signers", inverted: rejection is the
default and acceptance is the configuration. A document somebody pushed to the
registry under their own key is listed, shown, and believed by nothing.

Submitting is an **admin's** write on the project rather than a developer's,
which is the one place this endpoint parts company with the gate submission it
otherwise resembles. A gate result is a fact about an artifact; a
`not_affected` statement is an assertion whose effect is to stop a finding
counting, which is nearer to approving a break-glass exception than to
reporting a scan.

### 10.6 Expiry, and still no expiry engine

A statement can carry `expires`, per statement or per document. **That is
Kitchen's term and not OpenVEX's**, which has no expiry field at all, and it
lives inside the signed bytes rather than beside them: an expiry supplied out
of band would be an unattributable edit to somebody else's assertion.

It is judged exactly as an exception's is, and by the same mechanism as §9.7 —
which is to say by no mechanism. `policy.VEXFrom` does not list an expired
statement, the finding it was covering fires unsuppressed, and the pass that
notices is the one that was going to re-evaluate anyway. Judging it against
`input.at` rather than against the reader's clock is what makes a replayed
decision suppress exactly what the original suppressed.

`vexMaxAgeDays` is the other half of the same idea and belongs to the
environment rather than to the author: an assertion nobody has restated in a
year is an assertion about a dependency tree that has moved. Under a bound, a
statement carrying no timestamp is not current, and a bound that is not a whole
number of days makes nothing current — a bound nobody can read bounds
everything out, which is the fail-safe direction.

### 10.7 Materialized in one place, and why it is not the seam #134 left

Issue #134 left a named seam in `policyeval.go`, beside where the exception
listing is attached, on the reasoning that applied to exceptions: which grants
are in scope is a *listing* rather than a materialization, so each caller that
evaluates for real attaches its own.

That reasoning does not carry over, and following it would have been a
mistake. An artifact's VEX statements are already in the evidence set — they
are attestations on the digest — so materializing them needs the `evidence`
argument `MaterializeInput` already holds and the clock it already carries.
Nothing is fetched. Putting the call at the seam instead would have created a
second materialization site, and the promotion, the rescan, the replay and the
API's eligibility preview would then have differed by whichever one somebody
remembered to update — which is the bug the whole epic exists to prevent, and
which the eligibility preview has already had once.

So `MaterializeInput` calls `policy.VEXFrom`, every evaluation path gets the
same statements from the same bytes, and the preview needed no change at all.

The materializer judges nothing. An unjustified `not_affected`, a statement
from an author nobody trusts and one whose envelope did not verify all reach
`input.vex` with their facts intact, and the bundle is what refuses them. A
statement dropped in the materializer would be a suppression decision taken
where no rule could be read and no reader could see it.

### 10.8 Visible, never silently applied

`GET /builds/{name}/vex` answers the artifact's statements **joined to the
findings they modify**: every finding from the newest vulnerability scan,
whether or not anything suppresses it, each carrying the statement about it,
its justification, its author, who submitted it, and whether it is justified,
current and verified. The dashboard draws it on the build view and
`kitchen vex list` prints it.

The join reports facts and not a verdict, which is the same line §7.1 holds for
gates: it never says "suppressed", because whether a statement suppresses
anything is the target environment's bundle's question and the same statement
can be honoured in staging and refused in production. What it does say is why a
statement is one no policy would act on — expired, unverified, or
`not_affected` without a justification — in a sentence rather than a badge.

An expired statement is shown rather than dropped, which is the one place this
view deliberately differs from the policy input. The evaluation must not act on
it; a person asking why a finding has come back needs to see the assertion that
ran out, and the date it ran out on.

### 10.9 The awkward parts, said out loud

- **Product matching is not implemented.** A statement's `products` are
  carried into the input and shown on every surface, and the default bundle
  matches on the vulnerability identifier alone. Kitchen names artifacts by
  digest; an OpenVEX document names products by whatever package URL its
  author chose, frequently for the source package rather than the image. A
  rule that demanded they line up would refuse honest documents far more often
  than it caught careless ones. What actually binds a statement to an artifact
  is the attestation's subject — a document is attached to a digest and read
  back from that digest — and a bundle that wants more has `products` in the
  input to match on.
- **`expires` is Kitchen's, so every other reader ignores it.** A document
  carrying one is still a valid OpenVEX document, and a tool that has never
  heard of the term treats the statement as unbounded. That is the direction
  JSON-LD fails in and it is not the safe one; it is accepted because the
  alternative was an expiry outside the signature, which is worse.
- **The platform verifies with its own key and nobody else's.** A vendor's
  OpenVEX document signed by the vendor is listed and never verified, so the
  default parameters believe none of it. In practice a vendor document reaches
  an artifact by being re-submitted through the API, which means the platform's
  signature attests that *this identity submitted these bytes* and the vendor's
  own signature is not checked at all — the chain of custody is the submitter,
  not the author. Naming external verification keys on the singleton is the
  obvious next step and is deliberately not in this cut.
- **An expired statement reads as *newly failing* in the drift view.** §9.8's
  distinction is drawn between a rule that fired at promotion and one that did
  not, and a rule a VEX statement was suppressing never fired at all — so when
  the statement lapses, drift reports a rule that did not fire then and fires
  now, which is exactly what `newly-failing` means and is not the whole story.
  The cause is on the build's VEX view, one click away, and naming a third
  status would mean the drift join reading every deployed artifact's VEX
  documents out of the registry on a view whose entire design is that it joins
  things already stored.
- **A suppression is not a waiver, and the register does not merge them.** An
  Exception waives a *rule* that fired; a VEX statement removes a *finding*
  before any rule sees it. So a release whose every finding is suppressed
  reads as plainly compliant — no exception, no waived rule, nothing in the
  register — which is correct and is also the thing to watch. The question
  "what is this environment ignoring, and on whose say-so" is answered by the
  build's VEX view and by the audit log, not by the exception register.
- **A CI key cannot file one.** A project's CI key is a developer on exactly
  one project, and submitting VEX is admin's. An organisation that generates
  VEX in its own pipeline has to grant that pipeline an admin credential
  deliberately — which is friction, and is the point: an artifact's own build
  pipeline asserting that its own findings do not apply is precisely the
  self-marked homework the suite is arranged around.
- **Nothing here checks that the assertion is true.** The platform signs that
  these bytes were submitted by that identity at that moment. Whether the
  vulnerable code really is unreachable is a claim about an application, made
  by a person, and the only thing that makes it accountable is that their name
  is on it and the date is beside it.

---

## 11. Access, and the platform's own operators (issue #139)

This is the hardest control in the suite and the one a supervisor cares about
most, for a reason that is worth stating before anything else is built on top
of it: **anyone with cluster-admin bypasses every control described above.**
They can edit an environment's requirements, delete an Exception, rewrite a
Project's access list, or apply a Deployment that no Release ever produced. A
platform that gates application changes but not its own operators has a
decorative control, and pretending otherwise would be the worst kind of
evidence — the kind that reads as an assurance.

So this section is arranged around what can honestly be claimed. Kitchen can
ask, on a cadence, who holds what and make somebody answer for each of it. It
can separate the acts that move a control from the four hundred deploys
between them. It can notice a write it did not make. It cannot stop somebody
who already has the cluster, and §11.5 says so in as many words.

### 11.1 Recertification is a cycle object, not a cron

The evidence is the point. A cron that emailed a list would produce a list;
what an examiner asks for is *who reviewed this, when, and what did they
decide about each entry* — which is a document, with a beginning and an end.

So a cycle **opens**, freezes a snapshot of every grant at that instant, is
reviewed grant by grant, and **closes**. It is an `AccessReview` custom
resource, for three reasons that are all this repository's existing shapes
rather than new arguments:

- A cycle has a reconciler. It opens, it comes due, it closes, and something
  has to notice each of those on a clock — which is a RequeueAfter, and is
  exactly what the Exception reconciler already is (§9.7's "no expiry
  engine": the clock is the object's own).
- A decision is a write, and a write surface waits for its reconciler. A
  revocation has to actually take the grant off a Project, which is a
  reconcile over cluster objects and not a row in a table.
- It is in the platform backup with everything else. A table would need its
  own export, and the one artefact an auditor asks for would be the one thing
  not in the archive.

One cycle at a time over the same grants. Two open cycles would be two
reviewers deciding the same question, and a close that applied one set of
revocations while the other still showed the grant.

The snapshot is frozen and never rewritten. A review is of what was true at an
instant; a grant made after the cycle opened is simply in the next cycle, and
a decision naming one is refused with a sentence saying so.

### 11.2 The artefact outlives the object

A closed cycle carries the whole review on its status, and that would be
enough if the object were forever. It is not: objects get deleted, clusters
get rebuilt, and an institution asked to show the access review it did last
March cannot answer with a custom resource that has since been
garbage-collected.

So closing mints a **signed statement of the whole cycle** — the snapshot,
every decision, who made it, which were self-reviews, which revocations were
actually carried out — as a DSSE envelope under
`kitchen.bermos.dev/attestation/access-review/v1`, kept in the store's
`signed_records` table under no TTL. It is the resource claim's data-class
declaration exactly (§15): a review has no OCI repository, so the subject is
an identity digest over the object's namespace, name and UID, and the envelope
is kept whole rather than attached to anything.

It verifies against the public key `GET /compliance` publishes, with Kitchen
out of the loop. That is the same exit story §5.1 buys for artifact evidence,
applied to the one document that is about people rather than about images.

An undecided grant is **in** the artefact, worded as `undecided` rather than
omitted. "Nobody looked at this one" is precisely the finding an examiner is
reading it for, and an artefact that quietly dropped the unreviewed rows would
read better than the review was.

Best-effort-loud, like every other signed record here: a platform with no
signing key or no store still closes the cycle, still applies the revocations
and still writes the audit records. What it cannot do is leave portable
evidence, and `status.artifact.message` says so rather than leaving a blank
field open to a generous reading.

### 11.3 The reviewer may be the reviewed, and it is recorded

Segregation of duties is the whole subject of this issue, so the temptation is
to refuse a decision somebody makes about their own grant. The answer here is
§8.4's, for §8.4's reason: **it is recorded, not filtered.**

An installation with one operator has exactly one person who can review that
operator's grant. Refusing would either make the control unsatisfiable — a
cycle that can never be closed is a cycle nobody opens again — or push
somebody into creating a second account to satisfy it, which is worse evidence
rather than better. So a self-review is stamped on the entry, counted on the
cycle, carried into the artefact, named in the audit record, and shown on the
dashboard in the colour the rest of the platform uses for "look at this".

The same reasoning decides the reviewers list: it is an **expectation, not an
enforcement**. Any platform operator may decide, and who actually did is on
every entry. A cycle only a named person could close is a cycle that stalls the
week they are on holiday, and a control that stalls is a control that gets
switched off.

### 11.4 Out-of-band writes: detection, and what it cannot see

The operator reads `metadata.managedFields` — the API server's own record of
which field manager last wrote each field — on the **six kinds whose content
is a control**: `Kitchen`, `Project`, `Environment`, `Exception`,
`Connection`, `AccessReview`. A manager the platform does not recognise on one
of those is a write no reconcile and no API call made, and it becomes a
privileged `integrity` audit record and a count on
`status.compliance.access`.

Six kinds and not all of them, deliberately. A Build or a Release edited
behind the platform's back is a curiosity; the operator list, a project's
grants, an environment's requirements, a break-glass grant and a credential's
connection are the five places somebody with cluster access would actually go
to make the platform permit something it otherwise would not. Watching those
and being read is worth more than watching everything and being ignored.

What the mechanism does not see, stated rather than glossed:

- **A caller may name their own field manager anything.** `kubectl edit
  --field-manager=kitchen` is invisible to this and always will be. There is
  no fix inside Kubernetes' own model: the manager name is a string the client
  chooses. This is the single largest hole and it is why the whole feature is
  called detection.
- **Status writes are skipped.** Every controller writes status, including
  ones that legitimately watch Kitchen's objects, and a status write cannot
  change what the platform allows. What is looked for is a write to the spec.
- **A foreign entry disappears when the platform writes those fields again.**
  So the count is a *standing* count of what is currently marked, not a
  running total — the audit log is what answers "what happened", and the two
  are different questions.
- **A restarted operator re-records what it has already seen.** The dedup is
  in memory on purpose: writing a marker onto the watched object would itself
  be a write to the thing being watched, and the detection would start finding
  its own footprints. Over-recording is §4.6's acceptable direction.
- **False positives are real and are the operator's to name.** A GitOps
  controller applying the singleton, a mutating webhook, a restore tool: all
  of them are legitimate and none of them is Kitchen. They go in
  `spec.compliance.access.expectedManagers`, matched exactly, which is the
  operator putting "this writer is expected" on the record — the alternative
  being an alert everybody learns to ignore within a fortnight.

An alert nobody can see is not an alert, so the result surfaces three ways:
the privileged audit record, the count and timestamp on the Kitchen
singleton's `status.compliance.access`, and the audit screen's privileged
filter — where `integrity` is one click.

### 11.5 The residual risk, said plainly

**Cluster-admin bypasses everything in this document.** Not "in principle" and
not "in a poorly configured installation" — by construction. The controls
described in §4 through §10 are enforced by an operator reconciling custom
resources, and anyone who can write those resources, or write Deployments
directly, or read the ClickHouse credentials out of a Secret, is outside every
one of them. The audit log itself lives in a database whose password is a
Secret in the same namespace.

What Kitchen does about it:

- **Detect.** §11.4, with its stated blind spots.
- **Record.** Reconcilers record the transitions they *observe*, not only the
  ones the API made (§4.5), so a change made with `kubectl` behind the
  platform's back still lands in the log — attributed to the reconciler that
  noticed it, which is honest about how it was learned.
- **Bound.** The chain's anchor lives outside the table (§4.3), so a log
  truncated from the end is visible without reading the log.
- **Surface.** The privileged classification exists so that the handful of
  records that matter are one filter away rather than buried under deploys.
- **Recertify.** The operator list is reviewed on the same cadence as
  everything else, so "who can do all of this" is a question with a dated,
  signed answer rather than a property of whoever last edited a values file.

What Kitchen cannot do, and does not claim:

- It cannot prevent a cluster-admin write. Admission control could refuse
  *some* of them, and would refuse the operator's own writes along with them
  unless the operator were exempted — at which point impersonating the
  operator's service account is the bypass, and the platform has bought
  complexity rather than a control.
- It cannot detect a write that names itself as the platform (§11.4).
- It cannot detect a *read*. Somebody with cluster access can read every
  environment variable and every credential the installation holds, and
  nothing here would show it.
- It cannot make its own log unfalsifiable to somebody holding the store's
  credentials. §4.3 is precise about what hash-chaining catches and what it
  does not; anchoring to a transparency log is the next step and is
  deliberately not in this cut.

The honest framing, which belongs in a control document rather than in a
release note: **cluster access is operator access**, exactly as
[AUTH.md](AUTH.md) has said since before any of this existed. The mitigations
that actually bound it are institutional rather than technical — how few
people hold kubeconfigs, whether those are issued just-in-time, and whether
the cluster's own API server audit log is shipped somewhere Kitchen's
operators cannot reach. Kitchen's contribution is to make the *inside* of the
platform reviewable and to make going round it leave marks. It is not, and
cannot be, a control over the people who own the cluster it runs on.

### 11.6 Orphaned identities, and why one flag is not enough

An identity is **orphaned** when it has done nothing recently *and* the
identity provider holds no account for it. Both halves, never either alone:

- Dormant but known is somebody who had a quiet quarter.
- Active but unknown is a machine account, or an entry written by `sub` for an
  account the directory answers about by address.

The pair has no innocent reading: it is a grant pointing at nobody. The
window is `spec.compliance.access.inactivityDays`, 90 by default, which
matches the review cadence on purpose — an account that did nothing between
two reviews is exactly the one a reviewer should be asked about.

Two limits travel with it. **Activity is the audit log's, and the audit log
records writes**, so an account that only ever reads is dormant by this measure
and is not dormant in fact; the alternative is recording reads in the evidence
log, which would drown it. And an installation federated to an issuer of its
own serves **no account directory at all**, so nothing there is reported as
unknown and therefore nothing is reported as orphaned —
`directoryConsulted: false` says so, because "we could not ask" and "nobody is
behind it" are different sentences and only one of them is evidence.

### 11.7 Nothing here blocks a deployment

Worth stating as its own line, because every other section in this document
describes something that can refuse a promotion. This one cannot, anywhere:

- An **overdue** cycle is a phase and a condition. The rules it has not
  reviewed still apply; nothing is revoked because a review ran late.
- An **orphaned** identity is a row on a list. The grant still works.
- An **out-of-band write** is a record and a count.

An access control that took a workload down when a review ran late is a
control that gets switched off within the month, and a control nobody leaves
on is worth nothing. The consequence of all of this is that somebody has to
look, which is what a recertification control *is*.

The one thing that does take something away is a **revocation somebody
decided**, carried out when the cycle closes — and even that has a floor: the
platform will not remove its own last operator, because a platform with none
refuses every operator-only route to everybody, the route that names an
operator included, and there is no way back that does not involve `kubectl`. A
compliance control that can lock an institution out of its own platform is one
that gets turned off.

### 11.8 "Elevated and time-boxed", and what is actually built

The issue asks for privileged operations to be treated as a distinct class:
**elevated, time-boxed, audit-logged separately.** Two of those three are
built and the third is not, so it is named here rather than left to be assumed.

*Audit-logged separately* is §4's `privileged` classification, made into a
property of the log rather than a convention six call sites happened to share:
six named classes, one filter, and the marking inside what the chain hashes so
it cannot be added or removed after the fact.

*Time-boxed* is the recertification cadence. A grant does not expire, but the
**review of it** does: every grant on the platform has a dated, signed answer
to "who last confirmed this, and when", and a cycle that goes past its deadline
says so on the singleton. That is a bound on how long authority can stand
unexamined, which is what the phrase buys in practice.

*Elevated* — a just-in-time elevation, where an operator holds no authority
until they request it for an hour — is **not built**, and the reason is
§11.5 rather than effort. Elevation is only a control if the un-elevated state
is actually weaker, and on this platform the un-elevated state of somebody
holding a kubeconfig is: full authority, unobserved. A Kitchen-level elevation
gate would add a step for the honest operator and none at all for the one it
is meant to catch, while producing evidence that reads as though it had. That
would be worse than nothing, on this document's own standard.

What would make it real is elevation at the layer that actually holds the
authority — short-lived cluster credentials, issued on request, with the
issuing system's own log outside Kitchen's reach. That is an institutional
control that Kitchen can carry inputs for and cannot implement, which is
exactly §3's line about where this platform's scope ends.

---

## 12. Retention, immutability and the clock (issue #140)

Logs are only evidence if they can be shown not to have changed, and if the
timestamps mean something. Both are cheap to build in now and expensive to
retrofit, which is why this sits in phase 5 rather than in a follow-up.

Three separate claims live here and they are worth keeping apart, because each
one is worth exactly what it can be defended as:

1. **Retention** — the platform keeps each class of what it holds for a stated
   time, and can show what it is holding right now.
2. **Immutability** — the platform's own credential can append to the audit log
   and cannot rewrite it.
3. **Time** — the clocks that stamp all of it agree with each other, to a stated
   tolerance, and the platform measures rather than assumes that.

### 12.1 One model, not five

Retention was already here and it was scattered.
`spec.observability.clickhouse.retentionDays` covered every telemetry table with
one number; `spec.compliance.audit.retentionDays` covered the audit log and the
decision register; the raw request table quietly kept a week and the hourly
rollup twelve retentions, derived rather than configured. Nothing was wrong with
any of it, and together they could not answer the question a records-retention
policy asks, which is per *class* and not per table: how long do you keep
container logs.

So `spec.retention` is now the one place that says it, in nine classes —
`containerLogs`, `buildLogs`, `flows`, `metrics`, `traces`, `requests`,
`clusterEvents`, `activity`, `audit` — and everything that enforces a retention
reads it. `internal/retention` resolves the singleton into a model; the store's
TTLs, the sweep's horizons, the API's answer and the singleton's status are four
readers of that one decision rather than four decisions.

**Every field is optional and an absent one inherits the knob it used to be.**
That is not politeness towards old configuration, it is the upgrade contract: an
installation that has never heard of `spec.retention` reads exactly as it did,
and the two old fields are this model's defaults rather than a second model
beside it. `source` on every answer says which of the two a number came from,
because an operator reading "30" wants to know whether anybody chose it.

The request ratios stay derived. Raw request rows are the densest thing in the
store and a week of them is what "show me the failing requests" needs; the hourly
rollup keeps twelve of the configured window so a year-scale view has something
to read. Installations do not need to disagree about those, and a second knob is
a second thing to explain.

### 12.2 Two classes, one table, and what that costs

Build logs and container logs are different classes and the same table. They
answer different questions — a container log is operational, a build log is part
of the account of how an artifact came to exist and is read months later beside
its provenance — and installations routinely want the second kept longer.

ClickHouse can express that: a TTL is a list, and each entry may carry a `DELETE
WHERE`. What it cannot express is that *and* `ttl_only_drop_parts`, which is the
setting that makes expiry a metadata drop of a whole day-partition rather than a
rewrite of every part holding one stale row. With only-drop-parts on, a part is
dropped when **every** row in it has expired — and a part holding both classes
never does until the longer of the two dates, so the shorter class would be a
promise the store was not keeping, silently, forever.

The rule the schema implements is therefore: **you pay for the difference only
if you ask for one.** Configure the two classes the same and the log table
carries one TTL and keeps the cheap mode. Configure them apart and it carries
two conditional TTLs and the mode comes off, so expiry becomes a row-level delete
during merge. That costs merge time, and it is the price of two retentions in one
table rather than a lie about one of them.

The two conditions are exact complements (`source = 'build'` and `source !=
'build'`) because a row matching both would have two dates and no answer, and a
row matching neither would never expire at all.

### 12.3 What the immutability claim is, exactly

§4.3 already says carefully what the hash chain does and does not prove: it
catches an editor who has the store but not the chain, and it does not catch
somebody who can rewrite the whole tail, because recomputing every hash from the
edit onwards is as cheap for them as it was for the platform.

What is added here is one specific thing, and it is worth having precisely
because it is specific. The operator revokes `ALTER UPDATE`, `ALTER DELETE`,
`TRUNCATE` and `DROP TABLE` on `audit_log` **from the user Kitchen itself
connects as**. Both the operator and the REST API hold that credential; a bug, a
compromised operator pod, or an authenticated caller who found a way to reach the
store's `Exec` could otherwise delete a range of records and rebuild the chain
over the gap, and there is no cryptography anywhere in this design that would
notice. After the revoke, the platform can append to its own log and cannot
rewrite it.

`ALTER TTL` is deliberately *not* revoked: `EnsureAuditSchema` keeps the table's
retention in step with the configured one and cannot do that without it. It is
also the one mutation whose blast radius is already bounded — a retention below
the floor is refused at admission and at the API — which is the argument for
keeping it rather than an apology for it.

What this does **not** stop, said out loud because a defence nobody can state the
limits of is a defence nobody should rely on:

- a ClickHouse administrator, who can grant the privileges straight back;
- anybody with the store's filesystem, since a MergeTree part is a directory;
- a restore of the whole database from a doctored backup;
- and it is not retroactive: it revokes a privilege, it does not seal the rows
  already written.

Bounding those is what an external anchor is for — a transparency log, an
operator-signed checkpoint — and this is deliberately not that.

It is also **best effort, and reported rather than enforced.** ClickHouse's RBAC
refuses a partial revoke against a user granted everything at a wider scope
unless `partial_revokes` is enabled for them, and an installation pointing
Kitchen at a store it does not administer may not be allowed to revoke anything
at all. A refused revoke does not fail the reconcile: it lands on
`Kitchen.status.compliance.audit.immutable` (false) with the reason beside it,
and on `GET /api/v1/compliance`. The honest consequence is "this installation's
log rests on the hash chain alone", which is a smaller claim and one the platform
should state rather than a reason to stop.

**The threat model for every other class is prose, and this is it.** Container
logs, build logs, flows, metrics, traces, requests, cluster events and the
activity feed carry no chain and no revoked grant. They are operational
telemetry: an account of how the platform behaved, useful in an incident, and
*not* evidence in the sense §4 uses the word. Anybody who can reach the telemetry
store can edit them and nothing here would detect it. Treating them as evidence
would be the kind of claim that is worse than none — so the platform does not
make it, and the things it *does* treat as evidence (the audit log, the decision
register, the signed records, the attestations in the registry) are each
protected by something specific and named. An installation that needs
tamper-evidence over application logs needs a write-once sink outside this
cluster; that is a shipping problem, and no retention setting can stand in for
it.

### 12.4 Deletion evidence, and why it is a claim about what is left

"When retention expires, record that it did" has an obstacle in it worth naming
before the design: **ClickHouse expires data on its own merge schedule and tells
nobody.** There is no callback, no return value, and unless part logging happens
to be enabled, nothing durable to read afterwards. A record written by inferring
what the store must have done would be a guess with a timestamp on it, which is
worse than no record.

So the daily sweep does not claim to observe the store's deletions. It makes a
**dated claim about what is left**: for each class, the horizon its own
configuration puts there, the oldest row that survives it, how much the class
holds, and how much of that is still on the wrong side of the line. Two
consecutive records are the evidence that data expired between them, and it is
the kind of evidence retention actually needs — not "these particular rows were
deleted", which nothing here can substantiate, but "at this time, under this
rule, this class held nothing older than this date".

Where it *can* delete exactly, it does: a partition every row of which is past
the horizon is dropped as metadata and those rows are counted, which is the one
number in the record that is exact rather than observed. It usually finds none,
because the store's own TTL merge got there first — that is the intended division
of labour, not a failure.

**The sweep never deletes rows it cannot attribute to exactly one class.** That
rule covers two cases at once. The log table is shared, so a partition drop there
would take the longer-lived class with it; and the audit log is never swept,
because a sweeper that could delete audit rows on a schedule is a sweeper that
could delete the record of its own deletions. Audit expiry is left entirely to
the table's own TTL.

The record goes into the existing audit log rather than into a ledger of its own:
one record per pass, kind `Retention`, `details.change` `retention-sweep`,
carrying every class with its horizon and its numbers. One per class would be
nine lines a day for the same fact.

There is a circularity in that, and it is better named than engineered around:
the evidence of audit expiry lives in the audit log and ages out under the audit
retention. What bounds it is the floor. A record of what was deleted four hundred
days ago is not evidence anybody is looking for; a record of what was deleted
last month is, and ninety days guarantees it.

### 12.5 The floor, and the one way under it

`spec.retention.audit` has a floor of **90 days**, and §4.7's reasoning is the
whole of why: an incident reporting duty runs from when an institution *became
aware*, which can be well after the transition that caused it, and a log that has
already aged out cannot substantiate the report. Ninety days is the shortest
window in which "we found out, then we looked" is still a sentence the log can
support. The default is 365 and installations under a records-retention
obligation will want years; the ceiling is disk.

A floor with no way past it is a floor somebody eventually removes from the code.
So there is one, and it is a named field rather than a smaller number:
`spec.retention.auditFloorOverride`, with a `reason` of at least twenty
characters and an `approvedBy`. An installation that genuinely cannot keep ninety
days — a demonstration cluster, a jurisdiction whose data-minimisation rule bites
first — says so *in the object*, with a name against it, instead of patching the
platform.

It is refused in three places, and that is not belt and braces:

- a **CEL rule on the CRD**, so a `kubectl apply` behind the platform's back is
  refused too;
- the **API**, which answers `400` naming the field, the number, the floor and
  the override — because a caller should be told what to do about it by the thing
  they were talking to, not by a webhook message about a rule id;
- the **chart**, which fails the render rather than the install.

Using it is itself an audit record: kind `Retention`, `details.change`
`audit-floor-override`, carrying the number, the floor, the reason and the
approver. "Who decided we keep sixty days, and why" is a question with a written
answer and a history. The override is also read back in full by `GET
/platform/retention`, which is not the API reading a credential back — the whole
value of the field is that somebody outside the platform can see it.

### 12.6 Clock sync is measured, and the method's limits are the point

Every correlation in an incident report is three timestamps from three machines:
a log line the collector read off a node, a request row the operator wrote, an
audit record appended from whichever replica served the request. Clocks that
disagree by more than the gaps being reasoned about do not make any of those
wrong — they make the **order** wrong, silently, and there is nothing in the data
that shows it. Retention and immutability are both worthless over a log whose
ordering cannot be trusted, which is why this ships with them.

What the operator can actually observe from inside the cluster is the kubelet's
node lease. Each kubelet renews a Lease in `kube-node-lease`, stamping
`spec.renewTime` **from the node's own clock**; the operator compares that stamp
with its own. Three properties of that measurement are carried on the status
itself, in `method`, rather than left in a code comment:

- **It is one-sided in its precision.** A renewal is up to the kubelet's renewal
  period old by the time anyone reads it — ten seconds by default — so a node
  whose clock is *behind* is indistinguishable from a node whose renewal is
  merely stale, up to that period. A node whose clock is *ahead* stamps a time in
  the future, which nothing but a wrong clock produces. The threshold is
  therefore applied asymmetrically: a future stamp counts in full, a past one is
  forgiven up to a 45-second renewal grace. That grace is compiled in rather than
  configured, because it is a property of the kubelet and not of the
  installation.
- **The reference is the operator's own clock**, which comes from whichever node
  the operator happens to be running on. So what is measured is *disagreement
  within the cluster*, not agreement with UTC. **A cluster whose every node is
  uniformly ten minutes fast reads as perfectly synchronised here**, and that is
  the honest limit of the method.
- **Nothing outside the cluster is asked.** Measuring against UTC would mean
  reaching an NTP server or an HTTP `Date` header from the operator pod, and a
  platform that owns its cluster has no business deciding on its operator's
  behalf which time source the institution trusts. Whether the cluster's clocks
  agree with the world is the job of whatever runs `chrony` on those nodes.

The threshold is `spec.observability.clockSync.maxDriftSeconds`, default **5**,
and it is chosen against the *use* rather than against NTP's accuracy: five
seconds is roughly where "these happened in this order" stops being safe to say
across machines. A properly synchronised cluster sits three orders of magnitude
inside it, so a breach means time sync is broken rather than merely imprecise.

Drift beyond it appears as an unhealthy entry in the component survey —
`status.components`, name `clock-sync`, kind `Node`, `available` of `desired`
being nodes inside the threshold — which is the list an operator already reads
and the one that exists for exactly this kind of invisible failure. The message
names the worst node, the size and direction of its offset, and what to go and
look at, because "clock drift detected" is a message that sends somebody to a
search engine rather than to a machine.

Two things are deliberately *not* unhealthy: a node with no kubelet lease (the
check has no opinion about it) and a cluster where the leases cannot be read at
all (that is somebody else's RBAC, not a broken clock). Both are reported on
`status.clockSync.message` instead. A check that cried wolf would be turned off
inside a week, and a check that is off is worth nothing at all.

### 12.7 Things that are true and easy to get wrong here

- **A retention is a floor on what is deleted, not a ceiling on what is kept.**
  With `ttl_only_drop_parts` on — which is every table with one date — a
  day-partition is dropped when the whole partition is past its date, so at any
  moment the store holds up to one partition's worth of rows older than the
  horizon. That is what `expired` on the retention answer counts, and a small
  number there is normal. A number that stays large is the store holding data
  past its date, which is a thing to go and look at.
- **`enforced` false does not mean unenforced.** It means nothing has measured it
  yet: the configured half of the status is published within a reconcile of a
  change and the measured half within a day, and conflating "we have not looked"
  with "it is not happening" is the confusion this whole document is arranged to
  avoid.
- **A measurement is a claim about a horizon, so changing the horizon discards
  it.** Reporting yesterday's oldest row beside a retention that has just moved
  would be reporting a claim nothing has checked, so the status drops the
  measurement for a class whose number moved and keeps it for the classes that
  did not.
- **`signed_records` carries no TTL and that is on purpose.** It holds the
  envelopes with no registry to live in; a signed statement is kept as long as
  anything might cite it, and there are few enough of them that retention is not
  a disk question. It is deliberately not a retention class.
- **The retention sweep records even when it measured nothing.** A store that was
  down produces nine observations that each say so, and the pass records that.
  "We hold nothing" and "we could not ask" are the two answers a retention record
  must never confuse.

---

## 13. The audit pack (issue #142)

This is the section every other one has been building towards, and it is worth
being precise about what makes it worth having, because the mechanism is
unremarkable: it is a `GET`.

An institution asked to demonstrate a control does not fail because the
evidence is absent. It fails because the evidence is in a git platform, a
ticketing system, a spreadsheet and a log store, and the four do not agree —
the pull request says one person approved, the ticket says another, the
spreadsheet's inventory has an environment nobody has decommissioned, and the
log store no longer goes back that far. The reconciliation gap **is** the
finding, and closing it by hand is a project somebody runs for three weeks
every year and gets slightly wrong each time.

Kitchen has one reconciled graph. So the export is a read: `GET
/api/v1/projects/{name}/audit-pack?from=&to=` answers with one project's whole
compliance answer for one window — the inventory, the change log with the
author and the approvers of every release, the promotions and the decisions
behind them with the inputs they can be replayed from, the evidence attached to
each artifact, the break-glass register, the recertification cycles that
reviewed the project's access, what is running that no longer meets its bar,
the project's slice of the tamper-evident log, and every signed statement that
has no registry to live in. [docs/api/audit-pack.md](api/audit-pack.md) is the
field-by-field mapping.

### 13.1 Byte-reproducibility is the criterion that shapes the code

"The pack for a range is byte-reproducible" sounds like a nicety and is the
requirement that decided most of the implementation. Two exports of the same
window must be the same bytes, and the failure mode is not a broken feature —
it is a feature that works ninety-nine times and then produces a document
somebody has to explain the difference in.

Four things hold it, and each of them is a way it could have been lost:

- **No timestamp is read off a clock inside the payload.** Every other read in
  this API answers with a `generatedAt`, and the obvious first draft of this
  one did too. A test refuses one being added back.
- **Every list carries a total order**, by a key that cannot tie: a name, or a
  timestamp with a name behind it. Nothing is left in the order a Go map
  iterated or a store happened to answer in.
- **Every phase is judged at the range's end.** An exception that expired
  inside the window reads `Expired` in a pack taken the next day and in one
  taken three years later, because `EffectivePhase` is asked about `to` and not
  about `now`. The same for a recertification cycle. This is the one that is
  easy to get wrong, because judging a phase against the clock is what every
  other screen correctly does.
- **Canonical JSON**: struct field order — so the layout of the Go struct *is*
  the document's field order and is part of the format — sorted map keys, no
  HTML escaping, no indentation, no trailing newline.

And both ends of the window are required. A range ending "now" cannot be
reproduced, so the API refuses one rather than filling it in; the clients pick
a window and say so.

### 13.2 What reproduces, and what the platform will not pretend about

The honest half. Kitchen **reconciles** the graph rather than versioning it, so
"which environments existed in March" was never recorded anywhere and no export
can produce it. The pack says so in its own bytes: `reproducibility.rangeBound`
names the sections the window alone decides, and
`reproducibility.currentState` names the sections that also read the estate as
it stands — the inventory, the evidence index, the current drift rows, and two
that are easy to put in the wrong list. The change log's entries are entirely
historical in content, but a release running since before the window is in the
document because it was running *during* it; and this project's signed
declarations are all of them rather than the window's, because they are the
evidence behind the claims the inventory makes, and a row whose declaration
predated the window would otherwise stand unsupported in the one document meant
to support it.

That is not a gap to close later. Re-evaluating a historical state is exactly
what the decision register and its stored inputs are for (§4, §9.5): the
verdicts are reproducible because their inputs were materialized and kept, not
because the platform kept a copy of the cluster. A pack that presented a
current inventory as a snapshot of a past instant would be making a claim
nothing behind it supports, which is the failure mode this whole document is
arranged against.

### 13.3 Verifiable without Kitchen, as a procedure somebody can run

§5.1 keeps the evidence layer on standards so that it survives the platform,
and the pack is the same bargain applied to a document. The signature is a DSSE
envelope wrapping an in-toto Statement whose **subject is the sha256 of the
pack's own canonical bytes** — the same shape every attestation here has, with
a file where a container usually is.

The predicate is a manifest and not a copy. Putting the pack inside the payload
would double every byte of it and still not save a reader the `sha256sum`,
because what ties a file on disk to a signature is the digest either way. So
the envelope is small, the pack stands beside it, and the two are joined by one
hash a person can compute with a command they already know.

**The procedure is four commands, it is carried inside the pack so it travels
with the document, and a test runs it.** `TestThePublishedProcedureActuallyVerifies`
takes the two documents the API served, rebuilds DSSE's pre-authentication
encoding with `printf` and `cat` exactly as the published step says to, and
hands the result to `openssl dgst -verify` with the published key — no Kitchen
code anywhere in the verification path. A described intention would have been
worth nothing; §5's exit story is only true if somebody has actually walked out
of the door.

The pack publishes the public key so that it reads as one document, and says in
the same breath that a key taken out of the same file as the signature proves
only internal consistency. Trust comes from the key the institution kept when
the platform was installed. Stating that inside the document is the difference
between evidence and a thing that looks like evidence.

### 13.4 Three renderings, and why the server draws the human one

One address, three formats. `?format=json` is the document and the only bytes
the digest and the signature are about; `?format=dsse` is the envelope, freshly
signed on every request because an ECDSA signature carries a nonce and two
signings of identical bytes are two different envelopes; `?format=html` is the
same document laid out for a reader.

The rendering is the server's rather than the dashboard's, and the reason is
what a pack is for: it *leaves*. It is emailed, printed, put in a data room and
read by somebody who will never have a login on this platform. A rendering that
needed the dashboard running and the reader signed in would not be a document
that left — it would be a screen. So the page is one self-contained file with
no script, no stylesheet and nothing to fetch, and it carries the pack's digest
at the top, because a printout that cannot be tied back to bytes is decoration.

The dashboard's own job is the other half, and it is the sentence the whole
feature exists for: **an auditor presses a button.** Pick a project, pick a
window, press Export, and the screen shows the three things that change what
the document is worth before anything is saved — whether it is signed, whether
retention has already eaten into the window, whether a section reached its
limit — and offers all three files.

### 13.5 What it cannot answer for, it says

The suite's standing rule, applied where it bites hardest, because a pack is
read by somebody who cannot ask a follow-up question.

- **A window retention has already truncated is reported.** §12's whole
  argument is that deletion is provable; the corollary is that an export
  covering a range the store no longer holds must say so rather than answering
  with less. `retention.truncated`, `retention.coveredFrom` and a sentence
  naming both dates.
- **A section that hit its cap says so.** The two store reads are bounded at
  the store's own maxima — a thousand decisions, a thousand audit records —
  and a pack that filled either carries `truncated` and the advice to take two
  packs over narrower windows. The range is half-open precisely so that
  consecutive windows tile without counting anything twice.
- **An unsigned platform produces a pack that says it is unsigned**, rather
  than one that looks signed. The evidence inside is unchanged; what is missing
  is the means to check it somewhere else, and `verification.message` says
  which.
- **No credential is in it, and the absence is a word.** `connections[]`
  carries the name, the provider and the capabilities, and
  `connections[].credential` carries the sentence "held by the platform, never
  in this document". A blank there would invite a reader to conclude there is
  none.

### 13.6 The evidence index, and the one place the pack is deliberately thin

The attestation section is an **index**: predicate type, manifest digest, who
made the claim, and the `cosign` command that fetches it. It does not carry the
attestation bodies.

That is two arguments at once and they point the same way. The performance one
is the acceptance criterion: fanning out one registry round trip per artifact
per predicate is what would put a quarter's export over a minute, and nothing
else in the assembly grows with the project's history — five list calls against
the operator's cache, three store queries, and two narrow store queries per
deployed environment for the drift derivation. The honest one is §5.1: the
whole point of attaching evidence to a content digest through OCI referrers is
that the evidence does not need Kitchen, and a pack that copied it into itself
would be quietly claiming the opposite.

The section that *is* carried whole is `signedRecords` — the DSSE envelopes for
things with no registry to live in, a claim's data-class declaration and a
recertification cycle's closing artefact. Those have nowhere else to be, so
they are in the pack byte for byte, and byte for byte is not a nicety: the
payload inside an envelope is what its signature covers, so a re-encoded
envelope does not verify.

Where the pack does report a judgement rather than a fact, it reports **the
platform's own**: `attestations[].newestScan` is the newest re-evaluation of
that artifact, because `policy.NewestVulnerabilityScan` is what makes the
engine judge an artifact on its newest scan. A pack that listed every scan and
left the reader to order them would be a pack that could be read differently
from the policy that was actually applied.

### 13.7 It is the operator's, and taking one is recorded

A route's guard has to be the strictest thing in its body. This body folds
three operator-only reads into a project's evidence — the recertification
cycles that reviewed its grants, whose signed artefacts cover other projects
too and cannot be narrowed without breaking their signatures; the platform's
retention model; and the audit chain's anchor. A project admin who could export
a pack would read, in one file, what three routes refuse them separately.

It is also who takes one. An audit pack is produced by the second line for
somebody outside it, not by the team that deploys — and the application team is
not left worse off, because every project-scoped part of it is already a
viewer's read of its own.

Taking one is an **audit record**, under a kind of its own
(`EvidenceExport`), carrying the range, the digest, the size and the section
counts. It is the second read in this platform recorded that way, after a
platform backup, and for the same reason: "who took a copy of the evidence, for
which window, and what did they get" is exactly the sentence the log exists to
be able to produce. An export the log cannot record answers `503` and is not
served, which is the write path's rule applied to the one read where it earns
its keep.

It is **not** privileged. It moves no control, and the six classes in §11 are a
classification of authority rather than of importance; adding a seventh for a
read would make the filter mean less, not more.

---

## 14. Configuration

```yaml
kitchen:
  compliance:
    audit:
      enabled: true
      retentionDays: 365       # minimum 90
    attestation:
      enabled: true
      signingKeyRef: {}        # {name: …}; empty: the operator generates one
      build:
        provenance: true       # ask the builder how it built it
        sbom: true             # ask the builder what is in it
        sbomGenerator: ""      # empty: the pinned syft scanner, emitting SPDX
    machineIdentities:         # exempt from a project's pull request requirement
      - renovate[bot]
      - release-please[bot]
    exceptions:                # who may approve a break-glass waiver, by duration
      ladder:                  # empty: up to 24h developer, up to 720h admin,
        - maxDuration: 24h     # anything longer an operator
          role: developer
        - maxDuration: 720h
          role: admin
    gates:                     # what runs over every artifact; findings, never verdicts
      - name: trivy
        image: aquasec/trivy:0.58.0
        version: "0.58.0"
        format: trivy-json
        args: [image, --format=json, --output=$(KITCHEN_FINDINGS), $(KITCHEN_ARTIFACT)]
    vex:                       # who may assert that a finding does not apply here
      enabled: true
      trustedAuthors: []       # empty: any authenticated caller's document is admitted,
                               # and the attribution is the control
    rescan:                    # re-evaluate what is deployed, against today's database
      enabled: false
      interval: 24h            # per (environment, release) pair, from its last scan
      concurrency: 4           # scans in flight across the whole platform
      scanner:                 # matched against the SBOM, never against the image
        name: grype
        image: anchore/grype:v0.87.0
        version: "0.87.0"
        format: grype-json
        args: [-o, json, --file, $(KITCHEN_FINDINGS), sbom:$(KITCHEN_SBOM)]
        timeoutSeconds: 900
  retention:                 # how long each class is kept; absent = inherit
    containerLogs: 14        # empty inherits observability.clickhouse.retentionDays
    buildLogs: 180           # its own class: read beside an artifact's provenance
    flows:                   # …and so on for metrics, traces, requests,
    metrics:                 #    clusterEvents and activity
    audit: 365               # empty inherits compliance.audit.retentionDays
    auditFloorOverride:      # the only way under the 90-day floor
      reason: ""             # at least 20 characters, and read back in full
      approvedBy: ""
  observability:
    clockSync:               # do the clocks that stamp all of this agree?
      enabled: true
      maxDriftSeconds: 5     # measured against the kubelet leases; see §12.6
    access:                    # ask who holds what, and watch our own objects
      enabled: true
      intervalDays: 90         # from the last cycle's close; 0 opens none
      dueDays: 14              # before a cycle is reported overdue
      inactivityDays: 90       # dormant AND unknown ⇒ orphaned
      detectOutOfBandWrites: true
      expectedManagers: []     # writers that are not Kitchen and are expected
```

All of it lives on the platform singleton rather than on a Project, because
all of it is the operator's word rather than the application team's. A team
that could turn its own audit log off, sign its own evidence with a key it
chose, or decide how often its own running release was checked against today's
vulnerability database would be attesting to nothing.

`vex` is admission and not belief. It says whose documents may be attached to
an artifact at all; whose statements a given environment then takes the word of
is that environment's bundle parameters — `vexRequireVerified` (on by default),
`vexTrustedAuthors` and `vexMaxAgeDays`. Production can be stricter than the
platform and nothing can be looser than it, which is the same shape as a
project narrowing an inherited data class.

`rescan` has no compiled-in default scanner, deliberately. A scanner is pulled
on every scan of every environment and its database is refreshed on somebody
else's schedule; an installation that has not chosen one has not decided
something the platform should decide for it. Enabled with no scanner, the pass
reports itself as configured-and-inert on
`Kitchen.status.compliance.rescan.message` and on `GET /compliance/drift`
rather than quietly picking a vendor.

`access` is on by default, unlike `rescan`, and the difference is what each
costs. A rescan pulls a scanner image per environment per interval; a
recertification cycle costs one object and somebody's attention, and an
installation that has never been asked who holds what is the installation this
is for. Turning it off leaves the register readable and a cycle openable by
hand — what it stops is the platform opening one, or watching its own objects,
on its own initiative. `intervalDays: 0` is the middle position: no cadence,
full surface.

---

## 15. Phases

| | |
|---|---|
| **1 — Foundations** | audit log (#126), artifact identity (#127) — **built** |
| **2 — Evidence production** | provenance + SBOM (#128), PR verification (#129), quality gates (#130) — **built** |
| **3 — Policy** | environment ownership (#131), OPA engine (#132), staged promotion (#133) — **built** |
| **4 — Continuous compliance** | rescan (#134), OpenVEX (#135), exceptions (#136) — **built** |
| **5 — Institutional surface** | data class (#137), resource contract (#138), access (#139), retention (#140), criticality (#141), export (#142) — **built** |
| **6 — The mapping doc** | #143 — **built**, and §17.7 is what keeps it current |

Phase 2 attaches to §5 exactly as expected: every attestation it produces is
another envelope against the same digest, and the store accumulates them
without changing. Phase 3 attaches to §5.4: an environment that requires
evidence reads the evidence set and refuses an artifact that does not carry it.
Phase 4 attaches to §4 exactly as promised: a re-evaluation is a decision, and
a decision is an audit record — the rescan sweep records through the same
`DecisionRecorder` a promotion does, fail-closed, before it acts on anything.
It attaches to §5 too: a scan is another envelope against the same digest.
#135 closed the phase, and it closed it by attaching to §5 as well: an
OpenVEX document is another envelope against the same digest, under the
standard's own predicate type. What it changed everywhere else was one line —
`MaterializeInput` now calls `policy.VEXFrom` over the evidence it was already
handed — so the statements reach the promotion, the rescan, a replay and the
eligibility preview through the one materializer, and the seam #134 had
reserved for them turned out to be a place they should not go (§10.7).

Phase 5's data classification (#137) makes the classification a schema field
rather than documentation, because a schema field is what the platform can
enforce: `dataClass` on Project, Environment and ResourceClaim (ordered
`public < internal < confidential < strictlyConfidential`, absent =
unclassified and shown as such, never defaulted), `residency` declared on the
Environment and the Kitchen singleton and *recorded* from the provider's
actual placement on a claim's status. Classification is inherited and
narrowable, never wideable — an environment the platform creates inherits its
project's class at creation, the API refuses a claim classified above its
project, and a release flip that would land a classified project on an
environment rated below it (or not rated at all) is refused on every path:
outright on ungated environments — the build controller's fast path and the
API's direct moves and rollbacks alike, audit-recorded — and as the default
policy bundle's named `dataclass-le-environment` rule wherever a bundle is
pinned. Every classification change is a privileged audit
record carrying the previous value, and `GET /compliance/inventory` answers
the whole install's classes and locations in one exportable request.

The resource contract (#138) is the provenance axis of the same story: a
provisioner declares, on its results, what the provisioned data derives from
— `production`, `masked` or `synthetic` — and an absent declaration is
*undeclared*, treated by policy as the worst case rather than as clean. Neon
declares `production` for both a fresh database and every preview branch (a
copy-on-write branch of a production database is production-derived), which
is precisely how the default `data-provenance-preview` rule makes
"production data in a preview" a state the system refuses instead of a
finding: previews accept `masked`/`synthetic` unless the environment's policy
says otherwise. Each declaration is recorded on the claim's status, carried
in the bind's audit record, and signed as a
`kitchen.bermos.dev/attestation/data-class/v1` statement whose subject is a
claim identity digest (claims have no OCI repository), kept in the store's
`signed_records` table. The contract itself is documented in
[CRDS.md](CRDS.md) so a masking or synthetic-data provisioner can be written
outside this repository.

Criticality (#141) is the third institutional input, and the one where the
line in §3 has to be drawn hardest. **Kitchen cannot decide what is critical.**
That is a board's judgement about the institution's own functions, and a
platform that appeared to have an opinion about it — by defaulting an absent
designation, by refusing a deployment on one, by inferring criticality from
traffic — would be a platform somebody eventually cites as the source of a
determination nobody made. So `criticality` (`nonCritical < important <
critical`) and the `rto`/`rpo` tolerances are carried on the Project and the
Environment, absent means *undesignated* and is answered as that word, and
nothing anywhere refuses anything because of them.

What the platform contributes instead is the **mapping**, and that is the part
worth having: "which systems support this critical function, and which third
parties are behind them" is among the most commonly cited supervisory
findings, and every institution cited for it was maintaining the answer by
hand across four systems. Kitchen gets it nearly free, because the answer is a
traversal of a graph that is reconciled rather than maintained: `GET
/compliance/criticality` walks Project → Environment → Release and Project →
ResourceClaim/Connection/Domain and answers with everything behind each
designated function in one request, and `GET /compliance/dependents` walks the
same edges backwards from one Connection or one provider to every environment
that would break, worst designation first, with the tightest RTO among them.
Neither is cached, because there is nothing to keep in step; both carry the
honest depth of their own traversal in the answer, because a third party an
application calls from its own code is not a Connection and the platform
cannot see it.

**Criticality deliberately does not inherit the way a data class does, and
this is the most load-bearing difference between the two fields.** A data
class is a *containment* property — whatever holds classified data must be
rated to hold it — which is what makes narrowing-never-widening the right
rule and a ceiling the right enforcement. Criticality is a property of
*consequence*: what breaks in the outside world when this stops working. That
is not contained by anything. A preview environment of a payments service is
not a critical function — nobody's payment fails while a pull request's
preview is down — and copying the project's designation onto it would be the
one design choice guaranteed to get the whole feature switched off, because it
would page somebody at 03:00 for a preview. So a preview inherits nothing,
ever; a *production* environment declaring nothing reads its project's
designation, derived and marked as inherited rather than written back; and
there is no ceiling in either direction, because a `nonCritical` project may
perfectly well own the staging environment four teams integrate against. Every
change is a privileged audit record carrying the previous value, exactly as a
reclassification is.

The tolerances are not decorative, which is the criterion this feature is
easiest to fail. The declared RTO **is** the threshold `env.rto-at-risk` fires
against — half of it warns, past it is critical — so changing the number
changes when the pager goes off, and two environments with the same outage and
different objectives get different answers. Designating an environment
`critical` raises every warning about it to a critical finding, applied once
over the whole round rather than in each of thirty-odd rules. The RPO is
carried, mapped and reachable by a policy bundle, and **alerts on nothing**:
measuring a recovery point needs a recovery point to measure, no provider on
this platform declares one, and a rule that always passed would be worse than
no rule because it would read as evidence.
[OBSERVABILITY.md](OBSERVABILITY.md) §7 says what would close that.

Access (#139) is §11, and it attaches in three places rather than one. It
attaches to §4 by making the log's own convention into a property of it: the
`details.privileged` marking six sites had been setting by hand becomes a
classification with six named classes, materialized by the recorder and
filterable in one request — inside the hashed details rather than in a column,
because a new column would change the hash of every record ever written. It
attaches to §15's `signed_records` at the seam that table's comment already
reserved for it: a closed recertification cycle is an envelope with no
registry to live in, exactly as a claim's data-class declaration is. And it
attaches to nothing at all in the promotion path, on purpose — §11.7 — because
an access control that could refuse a deployment is one that gets switched off.

The export (#142) closed the phase, and it closed it by attaching to
**everything**: §13 is composition, not invention. It reads the classification
inventory of #137, the provenance declarations of #138, the recertification
artefacts of #139, the retention measurement and the clock of #140, the
designations of #141, the decision register and its stored inputs from §4 and
§9, the exception register from §4's own retained grants, the evidence index
from §5, and the pull request provenance of §8 — and answers with all of it as
one signed document for one half-open window. The only thing it adds is a
signature over its own bytes, in the shape §5 already uses, and one audit kind
so that "who took the evidence" is a query rather than an inference.

Two things about it are worth carrying forward rather than reading as detail.
The first is that **the honest gaps got louder rather than quieter** as the
suite converged: a range retention has already truncated is a field, an
inventory that is current state rather than a snapshot of the past says which
it is, and an unsigned platform produces a pack that says it is unsigned. A
document that closed those silently would have been easier to write and worth
less than the four systems it replaces. The second is that
**byte-reproducibility turned out to be a design constraint on the whole
answer**, not a property of the encoder: it is what forced a phase to be judged
at the range's end rather than against the clock, and that is a better answer
in its own right — a pack of last quarter should not change its mind in April.

Phase 6 (#143) is §17, and it is the one deliverable here that is prose rather
than platform. It attaches to everything by naming it: the `GR-` codes the
sub-issues have carried since the day they were written, and that
`docs/api/audit-pack.md` cites field by field, had no register until this
phase — §17.2 is the register they were pointing at. What that turned up is
worth recording. Writing the mapping out in one table is the first time every
component was read against the requirement it claims to answer, and the
honest column is the last one: **what it does not cover** is on every row, and
each entry in it was already documented somewhere above. A mapping table whose
last column was empty would have been the easiest version of this document to
write and the one an examiner would have stopped believing in the first ten
minutes.

---

## 16. Things that are true and easy to get wrong

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
- **The evidence set is read from the registry, not from Kitchen.**
  `status.artifact.evidence` is an *index* — predicate types and the manifest
  digests to fetch them by — and deliberately not a copy of the evidence. A
  copy would be a second source of truth, and it is exactly the copy an
  installation leaving Kitchen would lose.
- **A squash merge tells you nothing about who approved it.** The commit on the
  default branch is a new object; the approver is only in the provider's record
  of the pull request, which is why that record is what gets asked.
- **A dismissed approval is still in the provider's list.** Filtering on
  `state == "APPROVED"` without taking the newest review per reviewer counts
  approvals that were withdrawn before the change merged.
- **A gate's exit code says whether it ran, and nothing else.** Passing a
  scanner its `--exit-code` flag turns "found a vulnerability" into "the gate
  failed", which is the one confusion the whole design is arranged to prevent.
- **BuildKit's provenance version is not the one you expect.** It emits
  `slsa.dev/provenance/v0.2` unless told `version=v1`, and its `builder.id` is
  the empty string unless told `builder-id=`. Both are silent: the attestation
  is produced, verifies, and says nothing useful.
- **Base-image drift after build is not detected by SBOM rescan alone.** The
  bill of materials describes the image as it was built, so a vulnerability
  introduced into the base image afterwards is invisible to a matcher run
  against that SBOM. Catching it needs a scan of the image filesystem rather
  than of its bill of materials — a gate over the image, or a registry-side
  scanner — which is a different and far more expensive pass. §9.9 says what
  this does and does not cover.
- **A rescan's data snapshot may be `unpinned:`.** Most scanners do not report
  which vulnerability database they matched against, and a snapshot that names
  the scanner and the day dates a finding without reproducing it. The prefix
  is the whole point: a reader must be able to tell the two apart, and a
  bundle that requires reproducibility can match on it.
- **A blocked rescan does not stop anything running.** It records, surfaces
  and — only for an expired exception that asked for it — rolls back. Reading
  "blocked" as "the platform will take this down" is the wrong way round; the
  consequence of missing evidence lives at promotion.
- **A `not_affected` VEX statement without an enumerated justification
  suppresses nothing, and that is checked in two places.** The API refuses one
  at ingest; the default bundle refuses to act on one however it arrived. The
  second is not belt and braces — the evidence read merges the registry's
  referrers listing with cosign's attachment tag, so a document pushed by
  something that never spoke to Kitchen is visible to the policy engine, and a
  rule trusting the ingest path would be trusting a path that document never
  took.
- **`expires` on a VEX statement is Kitchen's term and not OpenVEX's.** It is
  read from inside the signed document, because an expiry beside the signature
  would be an unattributable edit to somebody else's assertion — and every
  other OpenVEX reader ignores it and treats the statement as unbounded.
- **A VEX suppression is not an exception and does not appear in the
  register.** An Exception waives a rule that fired; a VEX statement removes a
  finding before any rule sees it, so a release whose findings are all
  suppressed reads as plainly compliant with nothing waived. "What is this
  environment ignoring, and on whose say-so" is answered by
  `GET /builds/{name}/vex` and by the audit log, not by the exception
  register — and when a statement lapses, the drift view reports the rule as
  `newly-failing`, because it never fired at promotion. §10.9 says why that is
  the honest reading rather than a sixth status.
- **An operator upgrade moves the built-in bundle's digest, and every
  environment pinned to the old one stops promoting.** A bundle is pinned by
  digest and never by name, so a release that changes a compiled-in rule —
  `internal/policy/bundles/default/promotion.rego` — produces a new digest, and
  `policy.Resolver` refuses one matching neither the built-in bundle nor a
  labelled ConfigMap. Promotions into a pinned environment then answer "the
  environment's requirements could not be evaluated", which is a *refusal to
  judge* rather than a verdict: a bar that cannot be read has not been cleared.
  This is deliberate — it is the same rule that stops a ConfigMap edited in
  place quietly changing what everything pinned to it demands — but it surfaces
  as promotions failing across an install some time after the upgrade, which is
  a long way from where the change was made. After an upgrade that touched the
  built-in bundle, read `GET /api/v1/policy/bundles`, compare the `built-in`
  digest against what each environment pins, and repin the ones that have
  moved. `docs/api/decisions.md` describes the listing;
  `charts/kitchen/README.md` says the same thing under Upgrade.
- **`kitchen-audit-head` is load-bearing.** Deleting it does not lose the log —
  it is re-seeded from the table's own last record — but it does lose the
  anchor that would have shown a truncated tail.
- **`nonCritical` and undesignated are different answers, and so are
  `unclassified` and blank.** Somebody having looked at a function and decided
  it supports nothing critical is a determination; nobody having looked is a
  gap. Both read as "not critical" to a careless eye, which is why every
  answer words the absence — *undesignated* — rather than leaving a cell
  empty, and why `?criticality=nonCritical` matches the first and never the
  second.
- **Criticality is not capped by the project's, and that is deliberate.** The
  data-class rule is narrow-never-widen; criticality has no ceiling in either
  direction, because a `nonCritical` project can own the environment four
  teams integrate against and a critical project can own a preview that
  matters to nobody. Reading the two fields as the same shape of rule is the
  easiest mistake to make here.
- **An RTO of `0m` is a tolerance somebody set, and an absent RTO is not.**
  The first says no downtime is acceptable and is treated as declared
  everywhere; the second means nothing has been decided. The signal skips a
  zero objective rather than firing on the first blink of a rollout, and says
  so where it does.

- **The privileged marking is inside the hashed details, and that is the whole
  argument for it.** A `privileged` column would be the obvious shape and would
  change `ChainHash`, so every record ever written would stop verifying at
  once. In the details it is covered by the chain instead: a marking added to
  or taken off a stored record breaks verification, which is the property that
  makes the marking worth anything. `GET /audit?privileged=true` is a JSON
  extraction over the column, affordable because this table's row count is
  deploys and edits rather than requests.
- **A recertification cycle's evidence is not the object.** The object is the
  workflow and can be deleted; the artefact is a signed envelope in
  `signed_records`, kept under no TTL. Reading `status.artifact` as the
  evidence would be reading the index for the document — the same mistake as
  reading `status.artifact.evidence` for an artifact's attestations.
- **Out-of-band detection reads a string the writer chose.**
  `metadata.managedFields[].manager` is set by the client, so
  `kubectl --field-manager=kitchen` is invisible to it. It is detection, not
  prevention, and §11.5 says what that leaves standing rather than arguing it
  away.
- **An identity that only ever reads looks dormant.** The audit log records
  writes, so the orphan survey's `inactive` is "has made no *change* in the
  window". That is why an orphan needs the directory half as well: dormancy
  alone would flag every viewer on the platform.
- **The pack reproduces; the envelope does not, and that is not a defect.** An
  ECDSA signature carries a nonce, so signing identical bytes twice produces
  two different envelopes. What has to reproduce is the thing signed — and the
  envelope also carries the export's own timestamp, deliberately, because it is
  the record of *this* export while the pack is the document. A future change
  that tried to make the envelope stable would be solving the wrong half.
- **A field added to the pack changes the format.** The canonical encoding is
  Go's struct field order, so the layout of `auditPack` in
  `internal/api/auditpack.go` *is* the document's field order, and reordering
  the struct changes every future pack's digest without changing a byte of its
  meaning. Append rather than insert, and never for tidiness.
- **`generatedAt` is the field to keep out.** Every other read in the API
  answers with one; this one must not, and a test refuses it. The same trap
  wears three other names — `exportedAt`, `renderedAt`, `asOf` — and the test
  refuses those too.
- **A margin number in §17 is an anchor, not a citation.** The `Kap.` and `Rz`
  references are the ones the sub-issues carried, circulars get revised, and
  paragraph numbers move when they do. Every anchor names a chapter as well as
  a number so that a renumbering leaves it pointing at the right place, and
  §17.1 says the rest: verify against the published text before any of it is
  put in front of somebody who will act on it. A confidently wrong margin
  number costs more than an absent one, because it is the kind of error a
  reader generalizes from.
- **A phase judged against the clock is the subtle version of the same bug.**
  Every screen in this platform correctly asks `EffectivePhase(time.Now())`.
  Inside a pack that would make a document of last quarter change its mind in
  April, so the pack asks about the range's end, and anything new that carries
  a phase into an export has to do the same.

---

## 17. The requirement mapping (issue #143)

Everything above this section describes a mechanism. This one is the other
half, and it is the reason the rest is legible: for each supervisory
requirement the suite was built against, what in the platform produces the
evidence, where it is read, and what it does not cover.

It is also the deliverable most likely to be read by somebody who will never
open the code, so it is written to stand on its own — and the one that decays
fastest, because a mapping is worth something only while it is current. §17.7
is what holds it there.

### 17.1 Three things to read before the table

**Kitchen is not, and cannot be, "FINMA compliant."** Compliance is a property
of an institution: its governance, its risk appetite, its people and the
judgements they make. Software cannot hold that property, and a platform
claiming it would be selling the one thing it is structurally unable to
deliver. What this table claims is narrower and checkable — that the evidence
these requirements ask an institution to be able to produce is produced here as
a byproduct of deploying, rather than assembled by hand afterwards.

**A margin number is an anchor, not a citation.** The `Kap.` and `Rz`
references below are the ones the suite's own issues carried while it was being
built. They are approximate by construction: a circular gets revised, margin
numbers move when it does, and a reference written against one revision points
somewhere slightly different in the next. Every anchor therefore names a
chapter as well as a number, because the chapter survives a renumbering that
the paragraph does not. Nothing here is a quotation, and no number here has
been checked by anybody with a supervisory mandate. **Verify every reference
against the official circular before this document reaches an examiner, an
internal audit function, or anything that gets filed** — and where a paraphrase
here and the published text disagree, the published text is right and this
document is wrong.

**The table claims evidence, not adequacy.** A row says: this requirement asks
an institution to be able to show something, and *this* is the thing that shows
it. Whether the control behind it is adequate — whether the bar an environment
sets is high enough, whether the reviewers are the right reviewers, whether 90
days of audit log is long enough for this institution — is a judgement about
the institution and no table can make it. What a platform can do is remove the
excuse that the evidence is unobtainable. That is all it does here, and it is
worth more than it sounds.

### 17.2 The requirement codes

The `GR-` codes are the compliance suite's own shorthand, not a supervisory
numbering. Each sub-issue of #144 carried its codes from the day it was
written, and `docs/api/audit-pack.md` cites them field by field; this is the
register they have been pointing at.

A gap in the lettering is not a missing row. The register lists the codes
something on this platform actually answers — a requirement no component of a
deployment platform touches was never given a code here in the first place.

| Code | What it asks an institution to be able to show | What answers it here |
|---|---|---|
| GR-A2 | The first line owns its own controls, rather than a central function owning them on its behalf | Environment owners declare the bar for their own environment, and the application team cannot grant itself eligibility (#131) |
| GR-A4 | Which controls apply where, and a documented decision each time one was applied | The policy engine's stored decisions, with the bundle digest, input digest, data snapshot and rules fired — replayable (#132, §9.2) |
| GR-B1 | An inventory of assets, including software and where it is held | The reconciled graph itself: projects, environments, releases, claims, connections and domains, plus what each artifact is made of (#128, `GET /compliance/inventory`) |
| GR-C1 | Which functions are critical | The designation, carried and never inferred. Kitchen refuses to have an opinion about it (#141, §15) |
| GR-C2 | A declared tolerance for disruption, used rather than filed | `rto` / `rpo` on Project and Environment; the RTO *is* the threshold `env.rto-at-risk` fires against (#141) |
| GR-C4 | Which systems and third parties support each critical function | `GET /compliance/criticality` forwards, `GET /compliance/dependents` backwards, both traversals of the graph rather than a maintained list (#141) |
| GR-D1 | That every change to production is traceable to a person and to an artifact | The hash-chained log (§4), the artifact's identity and its evidence (§5), and the review the change came through (§8) |
| GR-D2 | Environments separated, including their data | One digest promoted through ordered stages and never rebuilt (#133), plus the provenance of what a provisioner handed a preview (#138) |
| GR-D4 | Vulnerability and defect management, risk-tiered and ongoing | Gates record findings (§7), the rescan re-runs them against today's database (§9), VEX says what does not apply (§10) |
| GR-D8 | Log integrity, and timestamps worth reading | The chain and its verifier (§4.3, §4.8), the immutability claim as it is actually made (§12.3), and the measured clock drift behind every timestamp (§12.6) |
| GR-E1 | Access control over the platform and what it holds | Project roles and platform roles as the API enforces them, and the grant list as the pack reports it (§11, `GET /access/identities`) |
| GR-E2 | Recertification of access with retained evidence, and segregation of duties | Recertification cycles closing to a signed artefact (§11.1, §11.2), and the four-eyes record on every release (§8) |
| GR-E3 | Privileged access treated as its own domain | Six named privileged classes materialized inside the hashed details, filterable in one request (§11, `GET /audit?privileged=true`) |
| GR-E5 | Machine identities named, and their exemptions visible | The allowlist a bot commit is exempted under, recorded on the release rather than implied (§8.5) |
| GR-F2 | A posture driven by today's threat picture, not by the day of the build | Continuous re-evaluation, with the vulnerability database snapshot stored beside each finding (§9, §9.5) |
| GR-G1 | Critical data identified and classified | `dataClass` on Project, Environment and ResourceClaim — inherited, narrowable, never defaulted, `unclassified` answered as a word (#137) |
| GR-G2 | Where that data sits, continuously | `residency` declared on the environment and *recorded* from the provider's actual placement on a claim (#137, `GET /compliance/inventory`) |
| GR-G4 | Protection proportionate to the classification | The class ceiling refused on every path, and the provenance of provisioned data — `production`, `masked`, `synthetic`, or `undeclared` read as the worst case (#137, #138) |
| GR-G6 | Retention and deletion that can be proved rather than asserted | One retention model per class with a documented floor and a recorded override, and the sweep's own deletion evidence (§12.1, §12.4, §12.5) |
| GR-I4 | An orderly exit: the evidence outlives the platform that produced it | DSSE envelopes on OCI referrers, verifiable with cosign and openssl by an institution that has switched Kitchen off (§5.1, §13.3) |
| GR-J3 | Readiness for an inspection, without a three-week project first | `GET /projects/{name}/audit-pack` — one window, one signed document, byte-reproducible (§13) |
| GR-L1 | An artefact per control, not a screenshot of a screen | Every decision is a record, every closed cycle an envelope, every artifact an evidence set — all of it exportable (§5, §13) |
| GR-L3 | Evidence sufficient to substantiate an incident report inside the reporting clock | The audit retention floor exists for exactly this (§4.7), and the pack reports a window retention has already truncated rather than answering with less (§13.5) |
| GR-L4 | An exception register with owner, expiry, reasoning and compensating control | The Exception object and its escalation ladder (#136), read back through `GET /exceptions`; a VEX suppression is deliberately *not* in it, and §10.9 says why |

### 17.3 The anchors

Carried from the sub-issues, and subject in full to the caveat in §17.1.
FINMA-RS 2023/1 replaced 2008/21 with effect from 1 January 2024, so a mapping
that lands on the old circular's annexes is pointing at a document that no
longer governs.

| Anchor | What it covers | Components mapped to it |
|---|---|---|
| FINMA-RS 2023/1, Kap. IV.A (~Rz 22–46) | Operational risk controls, their applicability, and how exceptions to them are handled | #131, #132, #135, #136 |
| FINMA-RS 2023/1, Kap. IV.B | ICT and change management, and the logging under it | #126, #127, #128, #129, #133, #134, #139, #140 |
| FINMA-RS 2023/1, Kap. IV.C | Cyber risk management | #126, #130, #134 |
| FINMA-RS 2023/1, Kap. IV.D (~Rz 71–82) | Critical data: identification, classification, and where it is held | #137, #138, #140 |
| FINMA-RS 2023/1, Kap. V (~Rz 102 ff.) | Operational resilience: critical functions, tolerances, and what supports them | #141 |
| FINMA-RS 2018/3, Rz 18, 18.1 | Outsourcing: portability and an orderly return of what the provider holds | #127 |
| FINMA-RS 2017/1 | Corporate governance and the internal control system — the three lines behind "segregation of duties" | #129, #131, #139 |
| FINMA-RS 2013/3 | Audit, and the evidence an audit firm asks an institution for | #142 |
| Art. 29(2) FINMAG, and Art. 74b ISG for the NCSC duty | Reporting incidents of substantial importance, against a clock that starts at awareness | #126, #140 |

The last row is the one worth dwelling on, because it is where a platform
detail turns into an institutional failure. The reporting clock runs from when
the institution *became aware*, which can be well after the transition that
caused the incident. A log that has already aged out cannot substantiate the
report that duty demands — which is why the audit retention floor is 90 days
and why going under it takes a written, read-back-in-full override (§12.5).

### 17.4 Component by component

Every component the suite shipped, in the order it was built. "Reads as" names
the surface an answer comes out of; the CLI reaches all of them, and
`kitchen api` reaches anything without a command of its own.

| Component | Answers | Anchor | What it produces | Reads as | What it does not cover |
|---|---|---|---|---|---|
| **Audit log** (#126, §4) | GR-D8, GR-L1, GR-L3 | 2023/1 Kap. IV.B/C; FINMAG 29(2) | One hash-chained row per state transition, attributed to a person or to the named reconciler that decided it, appended before the transition is allowed to stand | `GET /audit`, `GET /audit/verify`, the Platform → Audit screen, the pack's `auditLog` | Changes made outside the platform. §11.4 detects some of them and says plainly what it cannot see |
| **Artifact identity** (#127, §5) | GR-D1, GR-I4 | 2023/1 Kap. IV.B; 2018/3 Rz 18 | DSSE envelopes wrapping in-toto Statements, attached to the image digest through OCI referrers | `GET /builds/{name}/attestations`, the Build screen, `cosign verify-attestation` with no Kitchen involved | Anything about a *tag*. Evidence is bound to content, and a tag reference is refused outright |
| **Provenance and SBOM** (#128, §6) | GR-B1, GR-D1, GR-D4 | 2023/1 Kap. IV.B | SLSA provenance from the builder itself, and a bill of materials, both countersigned by the platform | `GET /builds/{name}/attestations`, the Build screen | Runtime dynamic loads, and base-image drift after the build (§9.9) |
| **Review provenance** (#129, §8) | GR-D1, GR-E2, GR-E5 | 2023/1 Kap. IV.A/B; 2017/1 | Author, merger and the approvals that still stood — asked of the provider while the answer is still true, and recorded with whose claim it is | The pack's `changeLog.review`, the Build screen | Whether self-approval is acceptable. It is recorded as such, never filtered — that judgement is the institution's (§8.4) |
| **Quality gates** (#130, §7) | GR-D4 | 2023/1 Kap. IV.B/C | Signed findings from a gate that is given nothing and decides nothing | `POST /builds/{name}/gates`, `kitchen gates`, the Build screen | Verdicts. A gate that emitted one would be deciding the environment's business (§7.1) |
| **Environment requirements** (#131) | GR-A2, GR-C2, GR-E1 | 2017/1; 2023/1 Kap. IV.A | The bar an environment sets, changeable only by its owners and recorded with the digest it replaced | `PATCH /environments/{name}/requirements`, `GET /environments/{name}/eligibility`, the environment's Requirements panel | An application team's opinion of its own eligibility. That is the point of it |
| **Policy engine** (#132, §9.2) | GR-A4, GR-L1 | 2023/1 Kap. IV.A | A stored decision per evaluation: bundle digest, input digest, data snapshot, verdict, rules fired, and the canonical input verbatim | `GET /decisions`, `GET /decisions/{id}`, `POST /decisions/{id}/replay`, `GET /policy/bundles`, `kitchen decisions` | Fetching anything during evaluation. A decision that reached out cannot be replayed, so the engine cannot (§2) |
| **Staged promotion** (#133) | GR-D1, GR-D2 | 2023/1 Kap. IV.B | One digest moving through ordered stages, each boundary a decision; three verdicts and no fourth | `POST /projects/{name}/promotions`, `GET /promotions/{name}`, `kitchen promote`, the project's pipeline | A rebuild between stages. There isn't one, which is what makes the rest true |
| **Continuous re-evaluation** (#134, §9) | GR-D4, GR-F2 | 2023/1 Kap. IV.B/C | A scan of what is deployed against today's database, re-run through the promotion code path, and the drift between then and now | `GET /compliance/drift`, `kitchen drift`, the drift panel | Taking anything down. Blocked means recorded and surfaced; the consequence of missing evidence lives at promotion (§16) |
| **Exploitability** (#135, §10) | GR-L4 | 2023/1 Kap. IV.A | OpenVEX documents as attestations, with an enumerated justification for `not_affected` and the author recorded twice | `GET /builds/{name}/vex`, `POST /builds/{name}/vex`, `kitchen vex`, the VEX panel | The exception register. A suppression removes a finding before a rule sees it, so nothing is waived (§10.9) |
| **Exceptions and break-glass** (#136) | GR-L4 | 2023/1 Kap. IV.A | A per-rule waiver with requester, approver, incident reference and expiry, on a ladder that escalates with duration | `POST /projects/{name}/exceptions`, `GET /exceptions`, `GET /exceptions/{name}`, `PATCH /exceptions/{name}` to close one early, `kitchen exceptions`, the exceptions panel | Blocking the emergency. It is allowed, recorded loudly, and expires (§2) |
| **Data classification** (#137) | GR-G1, GR-G2, GR-G4, GR-B1 | 2023/1 Kap. IV.D | `dataClass` and `residency` across the graph, inherited and narrowable, with every change a privileged record carrying the previous value | `GET /compliance/inventory`, the pack's `inventory` | Deciding what is confidential. It carries the classification the institution made |
| **Provider data contract** (#138) | GR-D2, GR-G4 | 2023/1 Kap. IV.B/D | A provisioner's declaration of what the data derives from — `production`, `masked` or `synthetic` — signed, and `undeclared` treated as the worst case | The claim's status, the bind's audit record, the pack's `claims[].provenance` | Masking anything. It defines the contract; a masking provisioner is written against it ([CRDS.md](CRDS.md)) |
| **Access recertification** (#139, §11) | GR-E2, GR-E3, GR-E5, GR-E1 | 2023/1 Kap. IV.B; 2017/1 | Cycles that close to a signed artefact, privileged actions as their own class, orphan detection, and out-of-band write detection | `GET /access/identities`, `GET /access/reviews`, `POST /access/reviews`, `PATCH /access/reviews/{name}`, `kitchen access` | Preventing a cluster-admin bypass. §11.5 states the residual risk rather than arguing it away, and §11.7 says why none of it blocks a deploy |
| **Retention and the clock** (#140, §12) | GR-D8, GR-G6, GR-L3 | 2023/1 Kap. IV.B/D | One retention model over every class, a floor under the audit log, deletion evidence, and a measured answer to whether the clocks agree | `GET /platform/retention`, `PATCH /platform/retention`, `kitchen retention`, the retention panel | Immutability the platform does not have. §12.3 states exactly what the claim is, and `auditImmutable: false` is a smaller claim rather than a fault |
| **Criticality** (#141) | GR-C1, GR-C2, GR-C4 | 2023/1 Kap. V | The designation and its tolerances, the function-to-resource mapping forwards, and the blast radius of one provider backwards | `GET /compliance/criticality`, `GET /compliance/dependents`, `kitchen criticality`, the criticality panel | Deciding what is critical, and RPO alerting — no provider declares a recovery point, and a rule that always passed would read as evidence |
| **Audit pack** (#142, §13) | GR-J3, GR-L1 | 2023/1 throughout; 2013/3 | One project's whole answer for one window, byte-reproducible, in three renderings of one address — the JSON the digest is about, a DSSE envelope over it, and a self-contained page for a reader who is not an engineer | `GET /projects/{name}/audit-pack`, `kitchen audit-pack`, the pack panel | The gaps it names out loud: a truncated window, an inventory that is current state, an unsigned platform (§13.5) |
| **This mapping** (#143, §17) | Meta | — | The document that makes the rest legible, and the register the GR codes point at | `docs/COMPLIANCE.md` §17, linked from the README | Being right about margin numbers on its own authority. §17.1 |

### 17.5 What has no row, and why

§3 is the list, and it is specific on purpose. The short version: this platform
carries inputs for institutional obligations and never the obligation. Nothing
here identifies a critical function, determines that an outsourcing is
material, decides what counts as critical data, judges that an incident is of
substantial importance, performs a business impact analysis, or backs up and
restores an application's own state. Those are the institution's, every one of
them, and a platform that appeared to have an opinion about any of them would
become the thing somebody later cites as the source of a determination nobody
made.

Saying so plainly is not a disclaimer. It is what makes the rest of the table
worth reading: a document that claimed all six would be claiming the parts a
platform structurally cannot do, and an examiner would find that out in the
first ten minutes.

### 17.6 The question, and the one request that answers it

The rows above are the mapping. This is what it looks like from the other side
of the table — the questions that cost an institution three weeks of
reconciliation, and what each one is here.

| Asked | Answered by |
|---|---|
| "What is running in production right now, and what is it made of?" | `GET /compliance/inventory`, and the evidence set on each artifact |
| "Who approved this change, and were they somebody other than its author?" | The pack's `changeLog.review`, per release — `independent` is the field four-eyes actually asks about |
| "Why was this release allowed into this environment in March?" | `GET /decisions/{id}`, and `POST /decisions/{id}/replay` to re-run it against the bundle bytes kept beside it |
| "What is deployed that would no longer be allowed in today?" | `GET /compliance/drift`, with `newly-failing` separated from what was waived at promotion |
| "What is currently being ignored, on whose authority, and until when?" | `GET /exceptions` for waivers; `GET /builds/{name}/vex` for suppressions, which are not the same thing (§10.9) |
| "Who has access to this project, and when did somebody last check?" | `GET /access/reviews`, and the signed artefact each closed cycle left behind |
| "Which environments break if this provider goes down, and how long have we got?" | `GET /compliance/dependents`, worst designation first, with the tightest RTO among them |
| "Give me all of it, for last quarter." | `GET /projects/{name}/audit-pack?from=&to=` |
| "Was the platform recording, signing and re-evaluating at all while this happened?" | `GET /compliance` — the posture behind every answer above, because a pack from a platform that was not recording is a very different document |

### 17.7 Keeping it current

A stale mapping is worse than none: it is read as a claim about a platform that
no longer exists, and every row a reader checks and finds wrong costs the rows
they did not check. The rule is therefore the same one the rest of this
repository uses for the links a test cannot hold — make it fail rather than
make it somebody's job to remember.

`TestTheMappingCoversEveryComplianceSurface` in `internal/api` reads this
document and refuses two kinds of drift:

- **A compliance endpoint nobody mapped.** Every route in the API's own
  enforcement table (`api.PolicyTable()`) whose path is one of this suite's —
  `/audit`, `/compliance/…`, `/decisions`, `/exceptions`, `/access/…`,
  `/policy/bundles`, `/platform/retention`, an artifact's attestations, gates
  and VEX, an environment's requirements and eligibility, promotions, and the
  audit pack — has to be named in §17. Adding one and not mapping it fails the
  API's tests, in the same shape as `TestEveryCallNamesARealAPIRoute` failing a
  CLI command that names a route which has moved.
- **A component with no row.** Every issue this document gives a section to
  has to appear in §17.4, so a phase that adds a section without adding its row
  is a failing build rather than a quiet omission.

What a test cannot hold is whether a row is *true* — whether the anchor is the
right anchor and the paraphrase is a fair one. That is the walk in
[CLAUDE.md](../CLAUDE.md)'s final pass, and §17.1 is what a reader is owed in
the meantime.

### 17.8 The same evidence answers more than one regime

Nothing in the mechanism is Swiss. The reference regime is specific because a
specific one forces real decisions, but the evidence layer is deliberately
built out of standards rather than out of Kitchen: in-toto Statements, DSSE
envelopes, OCI referrers, SLSA provenance, SPDX or CycloneDX, OpenVEX, Rego.

The practical consequence is that mapping this platform to a second regime is a
second table over the same evidence, not a second implementation. An
institution under DORA's ICT risk requirements, an operator inside NIS2's
scope, a vendor asked for NIST SSDF practices or an ISO 27001 audit of change
and access management is asking for artifacts this platform already produces
and can already export. Whoever writes that table gets §17.4's middle columns
unchanged and rewrites only the anchors — which is the whole argument for
having used somebody else's formats in the first place (§5.1).
