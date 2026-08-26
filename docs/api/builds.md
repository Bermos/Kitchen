# Kitchen — Builds

Starting a build, stopping one, and reading back what the platform recorded
about it: the layers it reused, the attestations it signed, and the quality
gates that ran over the artifact.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Triggering a build

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/builds
```

An empty body rebuilds the commit the project built last — a rerun after a
flaky build or a changed secret. To build a particular commit:

```json
{"sha": "abc123def456789", "branch": "main"}
```

The branch may be left out for a commit that has been built before; for one
that has not, it falls back to the project's production branch. Builds are
immutable, so a rebuild is always a new `Build` with a generated name
(`shop-bld-abc123def456-xk2p9`) rather than a mutation of the old one.

Answers `201` with the new build.

## Cancelling a build

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/builds/shop-bld-abc123def456-xk2p9/cancel
```

The build job is deleted, pod and all; the `Build` itself stays, phase
`Cancelled`, with who cancelled it in its condition — Builds are the history of
who asked for what, so cancellation never removes one. A build that already
finished answers `409`.

## Which pull request a build belongs to

`git.pullRequest` is the request the commit is part of, absent when it is part
of none the platform has heard of. It is what decides that a build's release
goes to a preview environment rather than nowhere.

It is not simply "which event created this build". A branch is usually pushed
before a request is opened for it, and every provider delivers the push first,
so the `Build` for the head commit is normally created by the push — with no
request anywhere in it. The request event that follows finds the build already
there and records itself on it, and this field answers from either source. The
build's `spec.git.pullRequest` in the cluster stays as the push created it,
because a Build's spec is immutable; the API reports what the platform knows,
not which webhook won the race.

Two consequences worth knowing:

- **A rebuild inherits it.** `POST /projects/{name}/builds` for a commit that
  has been built before copies the request across, so a rerun of a preview
  build is still a preview build.
- **The preview arrives whenever the request does.** A request opened days
  after the branch was pushed does not rebuild the commit: the release that
  build already produced is routed to the preview environment then.

## What a build reused

Every build carries `cache`, which is the platform's answer to "why did that
take four minutes":

```json
{
  "enabled": true,
  "warm": false,
  "ref": "harbor.example.com/kitchen/my-shop:buildcache",
  "mode": "max",
  "message": "nothing had been cached under harbor.example.com/kitchen/my-shop:buildcache yet, so this build had nothing to reuse"
}
```

`warm` is whether the cache existed when the build started, which is the honest
half: neither BuildKit nor the buildpacks lifecycle reports how many layers it
went on to reuse, so nothing here claims a hit rate. A cold build is not a
fault — it is the first of its scope, or the first after the cache was removed —
and `message` says which. `enabled: false` with a message is the platform having
turned the cache off for this build because the registry did not keep the last
one; without a message it is an installation that asked for no caching.

`mode` is empty on a buildpacks build: the lifecycle has one cache image and no
`max`/`min` to choose between.

## Why a build failed

A build in phase `Failed` carries `failure`:

```json
{
  "container": "creator",
  "exitCode": 51,
  "reason": "Error",
  "message": "creator exited 51",
  "log": [
    "Paketo Buildpack for Web Servers 0.24.0",
    "ERROR: failed to build: exit status 1"
  ]
}
```

The Kubernetes Job behind a build reports `Job has reached the specified
backoff limit`, which is true of every failed build there has ever been. This
is the answer to the question that sentence leaves.

`container` is the one that ended the build, and it is the useful half:
Kitchen's build pods clone in an init container and build in another, so
`clone` and `creator` are two different diagnoses. `exitCode` is absent when
nothing exited — a pod evicted before it ran, or an image that would not pull —
and `reason` is then the kubelet's or the scheduler's own word for it
(`Evicted`, `DeadlineExceeded`, `ImagePullBackOff`, `Unschedulable`), kept
unchanged so that searching for it finds this build.

`log` is a copy of the last lines that container printed, taken when the
failure was observed. It is not the log: the whole of it is at
`GET /builds/{name}/logs`, which is what to read while a build is running and
afterwards. What the copy is for is the case the log store cannot serve — a log
collector that never started, or a build that failed before its first line was
shipped — where those lines are the difference between a diagnosis and a
shrug.

Not every failed build has one. A build refused before it ever had a pod — an
unsupported strategy, a commit refused for want of review — has no container to
name, and its `Ready` condition carries the reason instead.

The field is on the build, which means it is readable by anyone who may read
the project. Reading the pod it was taken from is the operator's, and a build
that failed is not the operator's problem.

## An artifact's evidence

`GET /builds/{name}/attestations` answers everything attached to what the
build produced:

```json
{
  "subject": "registry.apps.example.com/shop@sha256:9d3f…",
  "verified": true,
  "attestations": [
    {
      "predicateType": "https://kitchen.bermos.dev/attestation/build-record/v1",
      "statement": {"_type": "https://in-toto.io/Statement/v1", "subject": [...], "predicate": {...}},
      "envelope": {"payloadType": "application/vnd.in-toto+json", "payload": "…", "signatures": [...]},
      "verified": true,
      "keyIDs": ["9f2c…"],
      "digest": "sha256:41a0…"
    }
  ]
}
```

`verified` on the set says whether signatures were checked at all, and on each
attestation whether one was accepted — a listing and a verification are
different things and a client that could not tell them apart would eventually
treat one as the other.

A build that produced no artifact digest answers `409`: it is a build nothing
can be said about, which is not the same as one with no evidence. A registry
that cannot be asked answers `502`.

The endpoint is a convenience. Everything it returns lives in the registry
against the artifact's digest, as DSSE envelopes attached through OCI 1.1
referrers, and is readable by anything that speaks them.

What is attached to a successful build is Kitchen's own build record and, from
the builder, SLSA provenance and a bill of materials — the last two harvested
from what BuildKit pushed and countersigned by the platform, because BuildKit
leaves them unsigned. The build itself carries an index of them without a
registry round trip, on `artifact.evidence`:

```json
"artifact": {
  "repository": "registry.apps.example.com/shop",
  "digest": "sha256:9d3f…",
  "attested": true,
  "keyID": "9f2c…",
  "evidence": [
    {"predicateType": "https://kitchen.bermos.dev/attestation/build-record/v1",
     "kind": "buildRecord", "source": "platform", "manifest": "sha256:41a0…"},
    {"predicateType": "https://slsa.dev/provenance/v1",
     "kind": "provenance", "source": "builder", "manifest": "sha256:41a0…"},
    {"predicateType": "https://spdx.dev/Document",
     "kind": "sbom", "source": "builder", "manifest": "sha256:41a0…"}
  ]
}
```

`kind` is a label derived from the predicate type so that a client does not
have to carry the vocabulary; the URI travels with it because the URI is the
authority. `source` says who made the claim — the platform signs both, so the
signature cannot tell them apart, and a claim about what a build did is worth
more when the thing that did the building made it.

## How a change was reviewed

A build carries what the git provider said about how its commit arrived, on
`source`:

```json
"source": {
  "provider": "github",
  "pullRequest": 42,
  "title": "Add checkout flow",
  "author": "alice",
  "mergedBy": "bob",
  "approvers": ["bob"],
  "selfApproved": false,
  "independent": true,
  "required": true,
  "checkedAt": "2026-08-19T10:02:11Z"
}
```

Every field is the provider's claim rather than the platform's observation,
which is why `provider` travels with them. `required` says whether the project
demanded review for this commit, so a build carrying none reads as "not asked
for" rather than "asked for and missing"; `selfApproved` and `independent` are
separate because a change its author approved has been approved, and whether
that is acceptable is a policy question.

`message` explains a check that could not be made — a provider outage, a
connection with no such capability. That is not a finding about the commit and
does not refuse anything.

A project sets `requirePullRequest` through `PATCH /projects/{name}`, which
needs `admin`. Where it is set, a production-branch commit the provider cannot
associate with an independently approved pull request is refused **before the
build job is scheduled**: the Build exists with reason `SourceUnreviewed` and
never runs. Accounts on the platform's `compliance.machineIdentities` allowlist
are exempt, and every use of the exemption is an audit record.

## Quality gates

A build carries what each gate did on `gates`, beside its artifact:

```json
"gates": [
  {"name": "trivy", "phase": "Completed", "source": "platform",
   "predicateType": "https://kitchen.bermos.dev/attestation/quality-gate/v1",
   "attested": true, "finishedAt": "2026-08-19T22:41:07Z"},
  {"name": "sast", "phase": "Failed", "source": "platform", "attested": false,
   "message": "the gate did not run: the scanner exited 137"}
]
```

`Completed` means the gate **ran**, whatever it found — a scanner reporting a
hundred critical vulnerabilities has completed, because it did its job.
`Failed` means it did not run and nothing is known either way. Nothing here
says whether the findings were acceptable: gates record facts, and whether a
fact is disqualifying is a property of the environment being deployed to.

`POST /builds/{name}/gates` ingests a result that was produced somewhere else —
typically a scanner the application's own CI already ran:

```json
{
  "gate": "trivy",
  "version": "0.58.0",
  "format": "trivy-json",
  "findings": { "Results": [ ... ] }
}
```

The findings are carried unmodified into a signed attestation attached to the
artifact's digest, and the answer says where it went. It is the one endpoint
whose body is not a handful of fields, so it takes up to 16 MiB rather than the
API's usual megabyte: a container scan of an ordinary application runs to
several.

The result is recorded as **reported by** the authenticated caller, and the
Build's `source` for it is `external`. The platform's signature means these
bytes were submitted by that identity at that moment and have not changed
since — not that the findings are true. A submission does not overwrite a gate
of the same name that the platform ran itself.

A build with no artifact digest answers `409`; a registry that cannot be
written answers `502`; an installation holding no signing key answers `409`,
because storing an unsigned result would leave something in the registry that
looks like evidence and is not.

## Exploitability assertions (VEX)

A gate or a rescan says what was **found**. A VEX statement says whether the
finding **applies here**: the component is not present, the vulnerable code is
not in the execute path, a mitigation is already in place. Without it, a daily
rescan of a real dependency tree produces enough noise that people stop
reading it.

`POST /builds/{name}/vex` attaches an [OpenVEX](https://openvex.dev) document
to the build's artifact:

```json
{
  "document": {
    "@context": "https://openvex.dev/ns/v0.2.0",
    "@id": "https://shop.example/vex/2026-08-24",
    "author": "security@shop.example",
    "timestamp": "2026-08-24T09:00:00Z",
    "statements": [
      {
        "vulnerability": {"name": "CVE-2026-1"},
        "products": [{"@id": "pkg:oci/shop@sha256:…"}],
        "status": "not_affected",
        "justification": "vulnerable_code_not_in_execute_path",
        "impact_statement": "the parser is never reached from our entry points",
        "expires": "2026-11-24T00:00:00Z"
      }
    ]
  }
}
```

The document is carried **verbatim** into a signed attestation under OpenVEX's
own predicate type — not a Kitchen one, so `cosign download attestation` reads
it back with the platform out of the loop. The predicate type is the submitted
document's own `@context`, which is how OpenVEX versions itself: a v0.1.0
document is attested as v0.1.0 rather than relabelled, because the URI is what
says which vocabulary to read a document with and rewriting it would be the
platform editing somebody else's assertion. `predicateType` on the `201` says
which one was used, and every reader here matches by prefix. Statuses are
`not_affected`, `affected`, `fixed` and `under_investigation`.

**`not_affected` requires a justification from OpenVEX's enumeration**:
`component_not_present`, `vulnerable_code_not_present`,
`vulnerable_code_not_in_execute_path`,
`vulnerable_code_cannot_be_controlled_by_adversary` or
`inline_mitigations_already_exist`. Free text in `impact_statement` or
`status_notes` is carried beside a justification and never instead of one — a
suppression whose reason cannot be counted cannot be reviewed in aggregate — and
a document that gives only free text answers `400` naming the enumeration.

`expires` is **Kitchen's term and not OpenVEX's**, read off a statement or off
the document as a default for all of its statements. It is inside the signed
bytes rather than beside them: an expiry supplied out of band would be an
unattributable edit to somebody else's assertion. An expired statement stops
suppressing at the next evaluation — a promotion, or the rescan pass — and is
still shown, marked, so that a finding coming back has a visible cause.

The submission is **audit-recorded before it is attached**, naming the
authenticated caller, the document's author and every vulnerability it touches,
and the Build records both: `author` is the document's claim about itself,
`submittedBy` is the platform's own observation. The index row is keyed on the
document's `@id`, so a corrected document restating the same assertions
replaces its row rather than adding one; `@id` is optional in OpenVEX, and a
document without one is keyed on its **envelope's digest** — the one name for
it that a reader of the evidence set also holds, and stable, unlike the
attachment manifest, which moves whenever anything else is attached. Who may submit at all is
`compliance.vex` on the platform singleton; whose statements an environment then
takes the word of is that environment's bundle parameters.

Refusals: a build with no artifact digest answers `409`; a platform with VEX
turned off answers `409`; an author the platform does not admit answers `403`;
a platform holding no signing key answers `409`, because an unsigned document in
the registry would look like evidence and not be; a registry that cannot be
written answers `502`.

`GET /builds/{name}/vex` is the other half:

```json
{
  "subject": "registry.example.com/shop@sha256:…",
  "verification": "verified",
  "statements": [
    {"vulnerability": "CVE-2026-1", "status": "not_affected",
     "justification": "vulnerable_code_not_in_execute_path", "justified": true,
     "author": "security@shop.example", "submittedBy": "grace@example.com",
     "expiresAt": "2026-11-24T00:00:00Z", "expired": false, "verified": true}
  ],
  "findings": [
    {"vulnerability": "CVE-2026-1", "severity": "critical", "package": "libfoo",
     "vex": {"status": "not_affected", "author": "security@shop.example", …}},
    {"vulnerability": "CVE-2026-3", "severity": "low", "package": "libbaz"}
  ]
}
```

Every finding from the artifact's newest vulnerability scan is listed, whether
or not something suppresses it, with the statement covering it. **The newest
scan alone**: the re-evaluation pass attaches one per interval and the registry
keeps them all, so reading every scan would answer a persistent CVE once per
day the release has been up. It is the same scan the policy engine judges —
one implementation of "the newest" — so this view explains the decision that
was actually made. `justified`,
`expired` and `verified` are facts about the statement and not a verdict:
whether it suppresses anything is the target environment's policy's question,
and the same statement can be honoured in staging and refused in production.
`verification` is `listed` rather than `verified` when the platform holds no key
to check signatures with.
