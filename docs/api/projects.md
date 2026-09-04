# Kitchen — Projects

A project and everything that belongs to it. Its environment variables are a
route of their own rather than a field of its settings, because a whole route
is the unit of authorization here.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Creating a project

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop", "repo": "acme/shop", "connection": "gh", "registry": "harbor"}' \
  https://kitchen.apps.example.com/api/v1/projects
```

A project is a name, a repository in the provider's `owner/name` form, and the
two Connections it builds and stores images with — `connection` needs the
`gitSource` capability, `registry` needs `imageStore`. Optional fields with
their defaults:

```json
{"productionBranch": "main", "previews": true}
```

### A project whose software this platform did not build

A project's source is one of two things, and exactly one of them is sent: a
repository this platform builds, or an image somebody else published.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "home-assistant", "image": {
        "repository": "ghcr.io/home-assistant/home-assistant",
        "tag": "2026.9.1",
        "signature": {"identity": "releases@home-assistant.io",
                      "publicKeySecret": "home-assistant-cosign"}}}' \
  https://kitchen.apps.example.com/api/v1/projects
```

`image.repository` is where the image lives, registry host included and
without a tag or a digest. One of `image.tag` and `image.digest` is required —
a tag is what a vendor publishes, a digest pins the exact content, and naming
both means "this tag, and it must still be this content". `image.connection`
is a Connection with the `imageStore` capability holding a docker config for
that registry, and it is **left out for a public image**, which is pulled
anonymously.

It is deliberately not the same credential as `registry`. That one is where
this platform *pushes* what it builds; a vendored image is *pulled* from
somewhere the platform never writes, and often from somewhere it holds no
account at all.

Everything a repository needs is refused rather than ignored on such a
project, here and on the settings PATCH: `registry`, `connection`,
`productionBranch`, `requirePullRequest`, `previews`, and the four that say
how a commit becomes an image — `buildStrategy`, `dockerfilePath`,
`dockerfileTarget` and `rootDirectory`. A `400` naming the field is the
answer, because each of them would otherwise read back as a setting that took
and do nothing.

What such a project *does* declare is everything else: its runtime, its
variables and its workloads, each of which a repository would have put in
`kitchen.json` and this one has nowhere to put — see
[a project with no repository declares all of this here](processes.md#a-project-with-no-repository-declares-all-of-this-here).

**Previews are refused in words.** They are environments for pull requests and
this project has no repository to open one against, so `{"previews": true}` is
a `400` saying so, and the Project's own `Previews` condition says the same
thing to anyone who never asked. A preview that silently never appears reads
as a fault.

`image.signature` says whose signature on the image is acceptable:
`publicKeySecret` names a Secret in the platform namespace holding the
vendor's key under `public.pem`, `identity` the signer the signature must
name, and `issuer` the OIDC issuer that certified it (which narrows an
identity and is refused without one). An empty `signature` object is refused
too — an expectation that says nothing reads as one being enforced. It is optional — the platform
looks either way, because whether a vendor signs at all is worth recording —
and it can also be declared once on the pulling Connection, where it belongs
when a whole registry is one vendor's; what an image declares wins over that,
whole rather than field by field. **A key is what makes a `verified` result
reachable**: a keyless signature's certificate is a claim by whoever issued
it, and this platform holds no root to chain it to, so an identity with no key
beside it reads `unverifiable` and says why. [COMPLIANCE.md
§18.5](../COMPLIANCE.md) is the model.

**A Build still exists.** Creating the project produces one, and it resolves
the digest the image reference names and freezes it onto a Release without
running a builder — so `status.artifact`, the evidence index, the quality
gates, the audit chain, the build screens and the CLI keep working unchanged.
Nothing fakes a commit: the Build names no SHA and no branch.

**It is attested, never assumed.** The Build harvests what the vendor
published about the digest, checks each statement describes *that* digest,
restates it and signs it under the platform's key as `vendor-asserted`;
generates a bill of materials where the vendor published none and signs it as
`platform-observed`; records what became of the vendor's own signature; and
signs an adoption record saying who admitted this digest, from where and when.
What each of those is, and where evidence cannot be attached at all, is
[builds.md](builds.md#an-artifact-the-platform-did-not-build) and
[COMPLIANCE.md §18](../COMPLIANCE.md).

**What moves it afterwards is a new digest under the tag it follows.** The
settings PATCH changes a project's settings, and which image it runs is not
one of them. The platform asks the registry on an interval whether the tag
still names the digest it acquired, and takes a new one where it does not —
see [acquiring a new digest](#acquiring-a-new-digest) below, which is also how
to ask now.

**A unit may mix the two.** A project built from a repository can carry a
workload that runs an upstream image, declared on the workload rather than on
the project — see
[a workload that runs a vendored image](processes.md#a-workload-that-runs-a-vendored-image).
They ship in one Release, deploy together, and roll back together: the Release
records the digest every workload of the unit resolved to, vendored ones
included.

`rootDirectory`, `dockerfilePath` and `dockerfileTarget` may be set here too,
which is what `POST /connections/{name}/detect` exists to get right: the
preflight reads the repository the way a build would — down to the stages the
Dockerfile declares — and a build context it showed to be wrong is corrected on
the form rather than after the first build has failed.

**The root directory is the build root**, and `dockerfilePath` is relative to
it — so a project whose application is in `apps/shop` and whose build file is
`apps/shop/docker/prod.Dockerfile` sends `"rootDirectory": "apps/shop"` and
`"dockerfilePath": "docker/prod.Dockerfile"`. The commit's own
[`kitchen.json`](../CONFIG.md) is read there too, and both build strategies
mean the same directory by it: the container build is handed it as its entire
context, and the buildpacks lifecycle is pointed at it. Nothing above it is
part of the build, so a path that leaves it — `../shared/Dockerfile`,
`/Dockerfile` — is a `400` here and on the settings PATCH rather than a build
that fails without being able to say why.

**`dockerfileTarget` is which stage of that Dockerfile to ship** — BuildKit's
`--target`, and absent means the file's last stage, which is what every build
did before the field existed. It is here because the last stage is frequently
not the runtime: a file that also builds a test image or a toolchain ends on
whichever of them was written last, and a build of the wrong one *succeeds*.
The preflight answers `stages` for exactly this reason, so the choice is made
from the names the file declares. A name no stage could have — anything but a
letter followed by letters, digits, dots, dashes and underscores — is a `400`
here and on the settings PATCH; a name the Dockerfile does not declare is not
knowable until the build, and fails it with a message naming the stages the
file has. Naming a target on a project built with `buildpacks` fails the build
too: the lifecycle has no stages, so the only thing it could do with a target
is ignore it, and an ignored target is the same wrong image.

This is the **web process's** stage, and the one every
[workload](processes.md#which-stage-each-workload-ships) that names none of its
own is built to. A workload names its own with `build.dockerfileTarget`, on the
same rule and in the same words.

From a terminal this is [`kitchen projects create`](../CLI.md#creating-a-project),
which runs the preflight, creates the project and links the directory to it in
one command — and takes the repository and the name from the checkout it is run
in.

The name has to work as a DNS label of at most 46 characters, because
everything the platform derives from it — the application namespace, release
names, generated hostnames — has to fit Kubernetes' 63-character limit.
Naming a Connection that does not exist, or one without the needed
capability, is a `400`; a Connection the operator has not assessed yet is
accepted, and the project's own conditions report whether it fits.

**Project names are one flat namespace under the platform's base domain**, and
they are first-come-first-served. Every URL the platform generates is a
subdomain of that domain, so there is no scope a second `shop` could be
qualified with — and the second person to want the name is told so in words
rather than being handed the API server's account of an object in a namespace:

```json
{"error": "the project name \"shop\" is taken: names are one flat namespace under the platform's base domain, since every URL the platform generates is a subdomain of it, so they are first-come-first-served — choose another name"}
```

Answers `201` with the new project. The operator takes it from there:
namespace, webhook, and — once the first build of the production branch
lands — the production environment.

**That first build is created by the platform, not by the next push.** As soon
as the project's source and registry connections are usable, the
`ProjectReconciler` resolves the production branch's current tip and creates
one Build of it, recording the fact in `status.initialBuildRef` so it happens
exactly once. Without it, a project created from a repository nobody was about
to commit to would sit at "no builds yet" until somebody pushed an empty
commit to wake it up. The Build carries the same deterministic name the
webhook receiver would give it — `<project>-bld-<sha[:12]>` — so a push that
arrives at the same moment is the same object rather than a second build of
one commit.

**Creating a project is self-service, and the account that creates one becomes
its `admin`** — written into `spec.access` on the new Project, not implied, so
that `kubectl get project -o yaml` and a git diff both tell the whole truth
about who may do what with it. The grant is part of the create itself, one
request carrying both, so there is never an instant in which a project exists
that nobody administers.

## Changing a project's settings

`PATCH /projects/{name}` edits what a project already is. Every field is
optional and absent ones keep their value:

```json
{"productionBranch": "trunk", "previews": true, "previewsProtected": false, "previewsMax": 3,
 "buildStrategy": "dockerfile", "dockerfilePath": "build/Dockerfile", "dockerfileTarget": "web", "rootDirectory": "apps/shop",
 "port": 8080, "replicas": 3, "cpu": "250m", "memory": "512Mi"}
```

`cpu` and `memory` are Kubernetes quantities and set request and limit alike;
an empty string clears one. `rootDirectory`, `dockerfilePath` and
`dockerfileTarget` mean here exactly what they mean on the create above — the
build root, a path relative to it, and the stage of that file to ship — and are
refused with a `400` for the same reasons, and on a project that builds
nothing for one more. An empty `dockerfileTarget` clears
the target, which is the file's last stage again. The
repository and the two connections are deliberately not editable: rebinding a
project to another repository is a different project.

### The preview ceiling

`previewsMax` is how many preview environments this project may have **live at
once**, overriding the platform's
[`previewsMaxPerProject`](./settings.md). Leaving it unset takes the
platform's, which is what almost every project should do — the ceiling is a
fact about the cluster, and a project raising its own is a project taking room
from its neighbours. It is here because "almost" is doing real work in that
sentence: the one project a platform exists for, with twelve reviewers and no
claims, is exactly the project whose ceiling should differ from the estate's.

`0` is no ceiling for this project. A **negative** number clears the override,
so the project takes the platform's again — 0 is a setting here and cannot also
mean "unset", the same way an empty string clears a text field that can also be
meaningfully empty.

A pull request that would exceed the ceiling **gets no preview**, and is told
so on the request: a commit status under `kitchen/<project>/preview` and the
preview comment, written under the marker the preview itself would have used.
A preview that already exists is never refused — the ceiling bounds how many
previews a project may have, not how many times each may be deployed to.
Production environments and anything promoted are never counted.

`GET /projects/{name}` answers what the ceiling is doing:

```json
{"previewsMax": 3,
 "previewCapacity": {"live": 3, "max": 3,
                     "refused": [{"pullRequest": 61, "commit": "ab12cd34ef56", "at": "2026-09-03T18:04:00Z"}]}}
```

`previewCapacity` is what the operator last measured. `refused` is the pull
requests that asked for a preview while the project sat at the ceiling, oldest
first and bounded at twenty. **It is a record, not a queue**: each gets its
preview on its next push, once another preview has closed and freed a slot.
The project also carries a `PreviewCapacity` condition, absent where there is
no ceiling.

`notRequestDriven` declares that this workload does work nobody asked for, and
turns idling off for every one of the project's environments — previews
included, which is where it matters, because previews idle by default. Scale to
zero is request-driven by construction, so an idle environment stops doing
*everything*, not only serving: a background loop, a poller or an ingest job
goes quiet with it, and the gap that leaves in whatever it was collecting is
indistinguishable from the upstream having been down. The environment says so
in its `ScaleToZero` condition, with reason `NotRequestDriven`, rather than
leaving it to be inferred from the absence of a scaled object.

It takes effect immediately rather than with the next release, like the
project's `scaleToZero` policy and unlike the rest of the runtime: whether an
environment may be parked is a decision about the environment as it stands
today, and a rollback must not quietly start parking one again.

`singleton` declares that two of this workload must never run at once. It
sets the Deployment's strategy to `Recreate` — the old copy stops before the
new one starts — and it **refuses `replicas` above 1**, rather than clamping
it:

```json
{"error": "this project declares its workload a singleton, so it cannot run 3 replicas: set replicas to 1, or turn singleton off"}
```

The refusal is the point. A clamped value reads back as a setting that did not
take, and a project would go on believing it runs three. The same rule is on
the CRD as a `x-kubernetes-validations` rule, so a write that does not come
through this route is refused too; the API checks it as well so the caller
gets a sentence instead of CEL. Either field may be sent alone — it is the
resulting combination that is checked.

The trade is a gap in serving during a deploy, and it is the correct one for
a workload that cannot overlap: an application with a poller, a scheduler or
an ingest loop in the same binary as the web server runs that loop twice
against a shared store for the few seconds a rolling update overlaps the two.
Duplicate work is the mild version; duplicate rows in a table something reads
as a record of what happened when is not an error at the time and not
obviously wrong afterwards. Leader election stays the application's problem —
not overlapping it during a deploy the platform itself initiated is the
platform's.

This is `spec.runtime`, so it is the *web* process that is declared here. A
worker declares it on its own entry in `processes`, and the arrangement that
moves a poller out of the web binary into one is exactly the arrangement that
needs it — see
[a worker that must never run twice](processes.md#a-worker-that-must-never-run-twice).

`command`, `args` and `previewArgs` are how the application is started, in
exec form — a list of words, never a shell line, so nothing is split, quoted
or handed to a shell:

```json
{"command": ["./server"], "args": ["--config=prod.toml"], "previewArgs": ["--config=fake.toml"]}
```

Absent, the image's own entrypoint runs, which is what an image built for this
project usually wants and never what a buildpacks-built one does. Each field
replaces its whole list and `[]` clears it, where leaving the field out keeps
whatever it had.

`previewArgs` replaces `args` in preview environments and is the sibling of an
environment variable's `previewValue`: a preview runs against a fake or a
seeded data source where production runs against the real one, from the same
commit and the same artifact — the artifact is built once and never rebuilt,
so "build a second image for previews" is not available and an entrypoint
script translating variables into flags is exactly the per-project boilerplate
this deletes. It replaces the list rather than extending it, because removing
a flag is half of what a preview wants — and an empty override is no override,
exactly as an empty `previewValue` is, which is how one is taken away.

All three are snapshotted into the release, so a rollback restores the
arguments it ran with — arguments are configuration, and a rollback that
restored the image but not the flags would have restored the wrong thing.
`GET /releases/{name}/config-diff` reports them alongside the port and the
replica count. The `PORT` contract is unchanged: a buildpacks-built image
still reads it.

`health` is what the platform asks the application before it sends anyone to
a new pod — on every deploy, and on every rollback, which is the one deploy
path that must not add a second outage to the one it is fixing:

```json
{"health": {"path": "/healthz", "port": 9000, "periodSeconds": 10,
            "timeoutSeconds": 2, "failureThreshold": 3, "startupFailureThreshold": 30}}
```

Every field is optional. **A project that declares nothing is still probed**:
absent a `path` the check is a TCP connect to the port the application is
published on, which is a weaker claim than an HTTP 200 and a far better one
than the readiness the platform used to assert without checking. It is
deliberately not `GET /` — plenty of applications answer that before they are
ready, and one that 404s there would never become Ready at all. `port` is only
for a check served somewhere other than the application's own port; the four
numbers are seconds and counts, and `0` on any of them takes the platform's
default (10, 2, 3 and 30). Sending `{}` restores the default check.

`startupFailureThreshold` is separate from `failureThreshold` on purpose:
slow startup is a legitimate state, and a threshold loose enough to tolerate
it is too loose to catch a wedge afterwards. A declared `path` also buys a
liveness probe — a TCP connect cannot tell a wedged application from a working
one, so restarting on it would kill healthy containers for nothing.

Reading a project back reports the check with every timing resolved, because
"what is actually checked, how often" is the question, and an empty field
answers it only for somebody who already knows the defaults. Previews inherit
it with the rest of the runtime, and it is snapshotted into every release.

`security` is the posture every workload of the project runs under — the web
process, its workers, its services and its scheduled runs, since a posture
describes how a container runs rather than the command it is started with:

```json
{"security": {"runAsNonRoot": true, "runAsUser": 1001, "runAsGroup": 1001,
              "fsGroup": 1001, "readOnlyRootFilesystem": true,
              "dropCapabilities": ["ALL"]}}
```

Every field is optional and `0` or `false` on any of them is the platform's
default, so sending `{}` takes a declared posture back off. `runAsUser` and
`runAsGroup` at `0` are the image's own ids left alone, which is not the same
as asking to run as root and does not read like it when the project is read
back. Capabilities are the kernel's spelling without the `CAP_` prefix, or the
single entry `ALL`; there is deliberately no list to add one, since the
platform drops none by default and a project that could add one would grant
its own container more than its image asked for.

`fsGroup` is the gid that owns the volumes the workloads mount, and it is the
field that makes a declared user and a volume claim work together: a freshly
provisioned volume comes up owned by `root:root`, so a workload running as
1001 is handed one it cannot write — it starts, reads as healthy, and fails on
its first write. The kubelet chowns the volume to this gid before the
container starts. `0` is the volume's own ownership left alone, on the same
reading as `runAsUser`. `fsGroupChangePolicy` is when that chown happens:
absent is Kubernetes' own default, `Always`, which walks the whole volume on
every start, and `OnRootMismatch` skips the walk when the volume's root
already matches — fast on a large volume, at the price of a subtree left by a
previous uid staying unwritable. The default is not moved, because that trade
is the project's to make. It applies only alongside an `fsGroup` and is
refused without one.

**A project that declares nothing still runs under a posture**, and reading a
project back reports it resolved — the runtime's own seccomp profile, and no
privilege escalation, which are the two hardenings a working image does not
notice. The three the issue behind this named — a read-only root filesystem,
dropped capabilities, a non-root user — are **not** defaulted: an image that
writes a cache or a socket into its own filesystem is ordinary and a great
many run as root, so tightening those by default would break a working
application on upgrade with nothing said. `allowPrivilegeEscalation` is the
one that goes the other way — the platform denies it, and setting it puts
back the setuid binary an image needs.

`declared` on the read is the posture in words, one phrase per constraint
beyond the default. It is the same list an environment's condition names when
a workload cannot start under it: a container the kubelet refuses — an image
that would run as root under `runAsNonRoot` — reports
`WorkloadAvailable=False` with reason `ContainerRefused` and the kubelet's own
sentence, and one that starts and exits repeatedly under a declared posture
reports `RestartingUnderPosture`. Neither is left as a `CrashLoopBackOff` with
the cause three layers down.

The posture is snapshotted into every release, so a rollback restores the one
that release ran under, and it can equally be declared in the repository —
see [kitchen.json](../CONFIG.md), where the commit that makes an image able to
run read-only is the commit that says so.

`processes` is what the project ships *besides* its web process — its queue
workers, its scheduled jobs, and the services the rest of the unit talks to
over the cluster network and the internet does not. It belongs on this route
rather than one of its own because it is the same decision as the port and the
replica count above it: what this project runs, and how much of it. That is
also why there is no tier above the project: a repository that ships four
things is one project with four entries here, deployed and rolled back as a
whole. The write replaces the whole list, and an empty list removes every
workload. See [Workloads](processes.md) for the fields, for how one workload
reaches another, for a workload built from its own directory of the
repository, for which of them a preview runs, and for reading what an
environment is actually running.

`files` is what the project's workloads are configured by when a variable will
not do — software the platform did not build is usually configured by a file
at a fixed path. It is on this route for the same reason `processes` is: what
this project runs, and what it is configured with, is one decision made by one
person at one role. The write replaces the whole list, and a file whose
`content` the request leaves out keeps what the platform holds — which is what
lets a client that was never shown a *secret* file's content send the rest of
the list back. See [Configuration files](files.md) for the fields, for why
secrecy is a flag rather than a second list, for why nothing is templated into
a file, and for the route a secret file's content is written on.

`dataClass` classifies the data the project handles — `public`, `internal`,
`confidential` or `strictlyConfidential`, in ascending order; `""` removes
the classification, and absent means unclassified, shown as such and never
defaulted. It is the top of the classification hierarchy: a
[claim](connections.md)'s class may not exceed it, environments the platform
creates inherit it, and a release flip onto an environment rated below it is
refused — outright on environments without requirements, and as the named
`dataclass-le-environment` rule where a policy bundle is pinned. Reclassifying is always
allowed — environments that now sit below the class read as non-compliant in
the [inventory](audit.md#the-classification-inventory) rather than the
correction being refused — and every change is audit-logged privileged, with
the previous value.

**Environment variables are not on this route.** They are the developer's day
job where the project's own settings are the admin's, and a whole route is the
unit of authorization here — so they have one of their own, below. A body that
carries `env` is a `400` naming it, rather than a field quietly dropped:

```json
{"error": "environment variables are not changed here any more: send them to PATCH /projects/shop/env, which needs developer rather than admin"}
```

Settings land in the next release's snapshot — what is already running keeps
the configuration it was released with until the next deploy.

## Changing a project's environment variables

`PATCH /projects/{name}/env` carries one field, and it replaces the whole
list:

```json
{"env": [
   {"name": "PUBLIC_URL", "value": "https://shop.example.com", "previewValue": "https://preview.invalid"},
   {"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}},
   {"name": "DATABASE_URL", "fromClaim": {"name": "shop-db", "key": "url"}}]}
```

A variable is a literal `value` (with an optional `previewValue` used in
previews), a `fromSecret` reference, or a `fromClaim` reference; naming more
than one source is a `400`. `{"env": []}` clears every variable, which somebody
may well mean; a body with no `env` at all is a `400` rather than the same
thing, because that is a client that forgot the field and not one asking for an
empty list.

A value goes in and never comes back out. Reading a project reports whether a
variable has one, not what it is:

```json
{"env": [
   {"name": "PUBLIC_URL", "set": true, "previewSet": true},
   {"name": "API_KEY", "set": false, "previewSet": false,
    "fromSecret": {"name": "shop-api-key", "key": "key"}}]}
```

A plain variable is exactly where somebody in a hurry pastes an API key, so it
is held to the same rule as a connection's credential. Replacing the whole list
therefore does not mean sending the values back: a variable whose `value` the
request leaves out keeps the one it already has, and an empty `value` clears it
— the bargain the credential fields make too. Repointing a variable at a
`fromSecret` or a `fromClaim` drops the value it used to carry, since the
reference is what replaces it.

The answer is the project, so a client that changed a variable renders the new
list without a second read. Variables land in the next release's snapshot, like
every other project setting.

A `fromSecret` usually names one of [the project's own
secrets](secrets.md) — the credentials Kitchen did not mint, written through a
route of their own and never read back. That is what a credential should be
rather than a literal `value`: the project's configuration then holds a
reference, and rotating the credential is one write that touches no
configuration at all.

## Acquiring a new digest

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/home-assistant/acquisitions
```

The vendored equivalent of a rebuild, for a project with no commit to name. An
empty body means "ask the registry what the tag this project follows names
now, and take it". Naming a digest takes exactly that one:

```json
{"digest": "sha256:1f0c…"}
```

Answers `202` with the Build that will carry it — an *acquisition*: a Build
that resolves the digest, freezes it onto a Release and runs no builder. What
it resolved, from which reference, when, and what it replaced is on that
Build's `status.acquisition`, which is what
[the builds page](builds.md#a-build-that-acquired-an-image) is about.

**`admin`, where a rebuild is a `developer`'s.** A rebuild runs the commit the
project already has through the same builder. An acquisition takes a new
artifact from a third party's registry onto this platform, and the body may
name the digest outright — which is a decision about where the software comes
from rather than about running the build of it again.

Two refusals, both `400` and both saying which:

- A project **built from a repository** acquires nothing; what moves it is a
  commit, so the answer names `POST /projects/{name}/builds`.
- A `digest` that is not `sha256:` and sixty-four hex digits. The registry
  vocabulary has one spelling of a manifest digest, and guessing which
  algorithm a bare hex string meant is not the API's to do.

**Nothing here is needed for the ordinary case.** The platform polls: one
registry manifest HEAD per watched reference per
`Kitchen.spec.builds.imagePollInterval` (ten minutes by default), and an
acquisition where a tag has moved. This route is for the impatient and for a
vendor's own pipeline, which knows the digest it has just published and would
rather say so than be discovered.

**A project pinned to a digest is never polled**, and this route still works
on it: pinning means the platform will not move the project on its own, not
that it refuses to be moved.

## Who is on a project

Membership is a project `admin`'s to *change*, which is the point of it: adding
somebody to `shop` does not go through whoever installed the platform. An
operator holds `admin` on every project, so they can do it too — they need no
rule of their own here, and neither does anybody else.

**Reading the list is a `viewer`'s**, because knowing who else is on a project
is part of knowing what the project is: a viewer who opened the People tab and
was refused on load would be reading a screen about a project they can
otherwise see in full. Only the three writes want `admin`. The same split
applies to [the CI keys](#keys-for-ci), which are the same list with its
non-human half shown.

All four methods answer on one path, `/projects/{name}/members`, and it is the
readable form of `spec.access` on the Project:

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
{"items": [
   {"subject": "user_01H8X…", "email": "grace@example.com", "role": "admin"},
   {"subject": "user_01J2Q…", "email": "anna@example.com", "role": "developer"}]}
```

`subject` is the issuer's `sub` and is the canonical identifier; `email` is
informational, so a list of opaque strings still reads. (The two swap round for
an entry hand-written against an address — see
[AUTH.md](../AUTH.md#where-membership-lives) — where `subject` carries the address
and `email` is usually empty.)

**Adding somebody names them by address, and the platform resolves it.**

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"email": "anna@example.com", "role": "developer"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
{"subject": "user_01J2Q…", "email": "anna@example.com", "role": "developer"}
```

The address is turned into the account's `sub` at the identity provider before
anything is written, because the address is what a person can type and the
`sub` is what a token will actually carry. An address the identity provider
does not know is a `404` — *they have to sign in to Kitchen once before they
can be given a role on a project* — rather than a grant that would sit on the
project matching nobody. Somebody who is already a member is a `409`; change
their role rather than adding a second entry.

`subject` is the other way in, and takes an identifier as given:

```json
{"subject": "svc_ci", "role": "developer"}
```

That is for an identity with no address to resolve, and for an installation
federated to an issuer that serves no account directory, where resolving an
address answers `503` saying exactly this. Exactly one of `email` and
`subject` is required, and a `subject` that looks like an address is refused:
pass it as `email`, so it is resolved rather than stored as the weaker
verified-address grant. A CI key is a machine account and so is one of these
grants, but it is not written this way — [`POST
/projects/{name}/keys`](#keys-for-ci) creates the account, the credential and
the grant together, which is the only way to end up with all three.

**A member is addressed by `subject` in the body, not in the path**, on both of
the writes that change one:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"subject": "user_01J2Q…", "role": "admin"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members

curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  -d '{"subject": "user_01J2Q…"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
```

A `sub` is opaque and may contain `/`, `%` or `#`; every path segment this API
addresses an object by is a Kubernetes name, and adding a percent-encoding rule
that only bites on the accounts with awkward identifiers is worse than a
`DELETE` that carries a body. `PATCH` answers `200` with the grant, `DELETE`
answers `204`, and a subject the project has no grant for is a `404`.

**The last `admin` cannot be removed or demoted.** Both writes refuse with a
`409` that says what would fix it:

```json
{"error": "anna@example.com is the only admin on shop, and a project with no admin has nobody left who can add one: make somebody else an admin first, then remove this one"}
```

An operator is not counted as a substitute. They could indeed repair such a
project, but a project whose only listed admin is gone is exactly the abandoned
project the rule exists to prevent: everyone working on it would have to go and
find an operator to get anything changed, which is the bottleneck self-service
membership was built to remove.

Every membership write is recorded in the [audit log](./audit.md#the-audit-log) as an update
to the `Project`, with the member, the role and whether they were added,
changed or removed. A grant is the most consequential thing an admin can do to
a project short of deleting it, and — like a deletion — removing one leaves no
trace anywhere else once the entry is gone. The writes also carry the caller's
`resourceVersion`, so two admins editing the list at the same time get a `409`
rather than one of them silently overwriting the other's decision.

## Keys for CI

A key is a member of the project, so its routes sit next to the membership
ones and want the same roles: **`admin` to issue or revoke one**, because that
is adding and removing a member, and **`viewer` to list them**, because the
listing is the membership list with its non-human half shown and carries no key
value — only the prefix the issuer keeps of one, which is useless as a
credential.

**A key is owned by a machine account created for it.** That is the part worth
knowing, because the obvious reading is wrong: the identity provider's api-key
plugin runs with `enableSessionForAPIKeys`, and the session it mints for a key
is a session for *the account the key belongs to*. So the `sub` in the token a
key is exchanged for is its owner's, and granting "the key's subject" a role
would grant it to whoever created the key, on their own account. Every key
therefore gets an owner of its own — an account that is not a person, holds
that one key, and exists only to have a `sub` the project can grant a role to.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "nightly"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys
{"name": "nightly", "subject": "user_01K4M…",
 "email": "shop.nightly@machines.kitchen.local", "role": "developer",
 "prefix": "9f3a1c", "created": "2026-08-19T09:12:44Z",
 "key": "9f3a1c…"}
```

**`key` is in that response and in no other.** It is stored hashed, exactly as
every other key at the issuer is, so a lost key is deleted and reissued rather
than looked up. Every read answers the `prefix` alone, which is enough to tell
two keys apart and useless as a credential.

**Creating writes both halves, or neither.** The key at the issuer and the
grant in `spec.access` are the whole of the feature: a key nothing has granted
anything to authenticates and can do nothing, which reads as a broken platform.
So if the grant cannot be written the key is taken back before the request
answers, and in the one case where it cannot be taken back either, the error
says so and names the key rather than leaving a credential nobody knows about.

`role` is optional and defaults to `developer`. `viewer` is the other value, for
a key that only reads; `admin` is refused, because admin is the role that issues
keys and a credential in a build pipeline that can mint its own successors is one
nobody can account for. A narrower role than `developer` — a `deployer` that can
build and promote and nothing else — is
[deliberately open](../AUTH.md#machine-accounts) and would arrive as another value
here.

That reasoning has one other consequence, and it is why `POST /projects`
refuses a key: **a machine account may not create a project.** Refusing to
issue an `admin` key means nothing if a key can create a project it is already
the admin of and issue keys there instead. It is also the only route that asks
what kind of account is calling rather than what role it holds — everything
else about a key is an ordinary grant on an ordinary project. See
[AUTH.md](../AUTH.md#machine-accounts) for how the distinction is drawn, and
why it never grants anything.

**A key name is a DNS label**, lowercase letters, digits and dashes, at most 32
characters — it addresses the key in the path, and it is half of the machine
account's own address at the issuer. One name per project: the same name twice
is a `409`, because two credentials behind one grant would make "revoke that
key" ambiguous. The clash is decided against the issuer's own list *before*
anything is recorded, so the audit log never carries "the key `nightly` was
issued for `shop`" for a request that issued nothing.

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys
{"items": [
   {"name": "nightly", "subject": "user_01K4M…",
    "email": "shop.nightly@machines.kitchen.local", "role": "developer",
    "prefix": "9f3a1c", "created": "2026-08-19T09:12:44Z",
    "lastUsed": "2026-08-19T09:40:02Z"}]}

curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys/nightly
```

`role` on a listed key is read from `spec.access`, not from anything stored on
the key. A key listed with no role is one whose grant has been removed — it can
authenticate and do nothing, and the listing says so rather than hiding it.

**`DELETE` revokes and un-grants, in that order**, and answers `204`. The
credential goes first because that is the half that matters: a grant naming an
account that no longer exists is a line to tidy up, and a key that still works
is not. Both writes are recorded in the [audit log](./audit.md#the-audit-log) as updates
to the `Project`, the same way a membership change is.

**Keys and people are one list.** A key's grant appears in
`GET /projects/{name}/members` like anybody else's, carrying `"kind": "key"`
and the key's name so it reads as what it is rather than as a stranger with an
odd address:

```json
{"subject": "user_01K4M…", "email": "shop.nightly@machines.kitchen.local",
 "role": "developer", "kind": "key", "name": "nightly"}
```

`kind` is derived from the address and is a display rule only — no access
decision anywhere reads it, and a role is resolved from the subject alone.

An installation federated to an issuer of its own serves no key endpoints: all
three answer `503` saying so, because keys are that issuer's to hand out.

## Deleting a project

```sh
curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop
```

Answers `202`: the operator's finalizer deregisters the git webhook, tears
down the project's environments (production included), garbage-collects its
builds, releases, domains and claims, and removes the application namespace.
There is no undo, which is why the dashboard makes you type the project's name
first.
