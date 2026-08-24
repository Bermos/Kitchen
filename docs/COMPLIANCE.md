# Kitchen — Compliance Design

> Status: **phases 1–4 implemented** (issues #126, #127, #128, #129, #130,
> #131, #132, #133, #134, #135, #136), and phase 5 in part (#137, #138). What
> is designed and not built is the rest of phase 5 — access review (#139),
> retention (#140), criticality (#141), export (#142) — and the mapping
> document (#143). This document is written so that adding them stays
> additive: each one attaches to something an earlier phase put in place, and
> the places where it attaches are named.

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
release, and it is never counted as compliant. For the same reason the answer
leads with `rescanning`: an empty drift view under a pass that is off means
*nobody is looking*, which is not the same answer as nothing being wrong.

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
  `status.artifact.evidence` is an index (§13), so a rescan replaces the
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

## 11. Configuration

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

---

## 12. Phases

| | |
|---|---|
| **1 — Foundations** | audit log (#126), artifact identity (#127) — **built** |
| **2 — Evidence production** | provenance + SBOM (#128), PR verification (#129), quality gates (#130) — **built** |
| **3 — Policy** | environment ownership (#131), OPA engine (#132), staged promotion (#133) — **built** |
| **4 — Continuous compliance** | rescan (#134), OpenVEX (#135), exceptions (#136) — **built** |
| **5 — Institutional surface** | data class (#137) — **built**, resource contract (#138) — **built**, access (#139), retention (#140), criticality (#141), export (#142) |
| **6 — The mapping doc** | #143, kept current |

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

---

## 13. Things that are true and easy to get wrong

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
- **`kitchen-audit-head` is load-bearing.** Deleting it does not lose the log —
  it is re-seeded from the table's own last record — but it does lose the
  anchor that would have shown a truncated tail.
