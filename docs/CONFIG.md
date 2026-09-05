# Kitchen — `kitchen.json`

A project's build and runtime settings, committed beside the code they
describe. Kitchen's answer to `vercel.json`.

It is the third way to configure a project, after the dashboard and the
[REST API](API.md), and the only one that travels with the commit. That is the
whole of what it is for:

- **A build of an old commit builds it the way that commit asked to be built.**
  A rollback replays the configuration its release was frozen with, not
  today's.
- **A change to how the application runs arrives in the change that needs it.**
  A pull request that adds a worker adds the worker's declaration in the same
  diff as the worker's code, and its preview runs with it.
- **A project moved to another installation arrives configured.** What is left
  to fill in is what the file is not allowed to carry, which is the short list
  below.

The file is optional. A project without one is configured entirely in the
dashboard, exactly as it was before this existed.

## Where it goes

`kitchen.json`, at the **project's root directory** — the top of the
repository for most projects, and `apps/web/kitchen.json` for a project whose
root directory is `apps/web`. It is read at every commit the platform builds,
at that commit.

That is also why the file **cannot set the root directory**. The platform has
to know where the project is before it can read anything, so a file that moved
the directory it was read from would have to be read before it could say where
to read it. It stays on the project, where the new-project form already asks
for it — and putting a project's own file next to its own code is what a
monorepo wants anyway.

## The shortest useful one

```json
{
  "$schema": "https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json",
  "runtime": {
    "port": 3000
  }
}
```

`$schema` is what makes an editor complete the fields and flag a typo before
it is committed. `kitchen config schema` prints the URL. The platform reads
the key only to ignore it: what a file may say is decided by the release that
builds it, not by a document fetched at build time.

## Check it before you push

```sh
kitchen config check
```

It reads `kitchen.json` in the working directory, refuses it the way a build
would, and lists what it declares. It needs no credential and reaches no
network — it is the same parser the operator runs, compiled into the same
release — so it belongs in a pre-commit hook or a pull request's own checks.
Anything it accepts, the build accepts; anything it refuses would have failed
a build several minutes in. `--json` puts the answer on stdout and nothing
else.

The one thing it cannot tell you is whether the file conflicts with the
project it will be built for, which is the last section on this page.

## What it can set

Everything here is optional, and **a key left out keeps whatever the project
has**. A key that is there wins, at every build, until the file stops saying
it.

### `build`

| Key | What it does |
|---|---|
| `strategy` | `auto`, `dockerfile` or `buildpacks`. `auto` reads the repository and decides — a Dockerfile wins, and everything else recognised goes to buildpacks. |
| `dockerfilePath` | The Dockerfile, relative to the project's root directory — which it may not leave, since the root directory is all a build sees. Used when the strategy is, or resolves to, `dockerfile`. |
| `dockerfileTarget` | Which stage of that Dockerfile produces the image to run — BuildKit's `--target`. Leave it out to ship the file's last stage. A stage the file does not declare fails the build; so does naming one on a commit built with `buildpacks`, which has no stages. It is the project's own — the web process's — and the stage every workload that names none of its own is built to. |

```json
{"build": {"strategy": "dockerfile", "dockerfilePath": "docker/prod.Dockerfile", "dockerfileTarget": "web"}}
```

A multi-stage Dockerfile ends on whichever stage was written last, and that is
often not the runtime — a file that also builds a test image or a toolchain
ships one of those, with a green build and nothing to notice. `dockerfileTarget`
is how a commit says which artifact it meant, and it travels with the commit:
rebuilding an old one builds the stage that commit asked for.

One file yielding several artifacts is the whole point of it, so each of
[`processes`](#processes) can name its own stage. There is one chain, and every
image the commit produces resolves through it: the workload's own
`build.dockerfileTarget`, else this file's, else the project's setting. A
workload that names none inherits the unit's rather than resetting to the last
stage — except one built with `buildpacks`, which inherits nothing, since the
lifecycle has no stages to inherit into.

### `runtime`

| Key | What it does |
|---|---|
| `port` | The port the application listens on, and the value of `PORT` in every environment. Leave it out to take the detected framework's — 3000 for Next.js, 8000 for Python, 8080 for a static site. |
| `replicas` | Copies in production environments. Previews always run one. |
| `singleton` | Two of this workload must never run at once, so a deploy stops the old copy before starting the new one. Refuses `replicas` above 1 rather than clamping it. |
| `notRequestDriven` | This workload does work nobody asked for, so no environment of the project is ever idled to zero — previews included, which is where it matters. |
| `command`, `args` | Replace the image's entrypoint and its arguments, in exec form: a list of words, never a shell line. |
| `previewArgs` | Replace `args` in preview environments — same commit, same artifact, different flags. |
| `resources` | `{"cpu": "500m", "memory": "512Mi"}`, applied as request and limit alike. |
| `health` | What the platform asks before it sends anyone to the application. `{}` is the default: a TCP connect to the port. |
| `security` | The posture every workload of this project runs under. `{}` is the platform's default, which they run under anyway. |
| `init` | What the web process needs done inside the volumes it mounts, before its own container starts. A named workload's own is on its entry of [`processes`](#processes). |

```json
{
  "runtime": {
    "port": 8000,
    "replicas": 3,
    "command": ["gunicorn"],
    "args": ["app:app", "--bind", "0.0.0.0:8000"],
    "resources": {"cpu": "500m", "memory": "512Mi"},
    "health": {"path": "/healthz", "periodSeconds": 5}
  }
}
```

`health` takes `path`, `port`, `periodSeconds`, `timeoutSeconds`,
`failureThreshold` and `startupFailureThreshold`. A `path` makes it an HTTP
check, where a 2xx or 3xx is the application saying it is working; without one
the check is a TCP connect, which is a weaker claim and much better than
asserting a readiness nothing established. It is deliberately not `GET /`.

`security` is where an application asks to run more tightly than the platform
makes it. A repository is the right place to say it: an image knows whether it
can survive a read-only root filesystem, and the commit that makes it able to
is the commit that should declare it.

```json
{
  "runtime": {
    "security": {
      "runAsNonRoot": true,
      "runAsUser": 1001,
      "runAsGroup": 1001,
      "fsGroup": 1001,
      "readOnlyRootFilesystem": true,
      "dropCapabilities": ["ALL"]
    }
  }
}
```

It applies to the web process, the workers and the scheduled runs alike —
they are one image, and a posture describes the image rather than the command
it is started with.

**A project that says nothing still gets a posture**, and it is the
platform's: the container runtime's own seccomp profile, and no privilege
escalation. It is deliberately not the tightest one available. An image that
writes a cache, a socket or a temporary file into its own filesystem is
ordinary and a great many run as root, so a default that tightened those
would break a working application on upgrade with nothing said anywhere — the
three that would, `readOnlyRootFilesystem`, `runAsNonRoot` and
`dropCapabilities`, are asked for here instead. `allowPrivilegeEscalation`
goes the other way: it is the one thing the platform tightens, and setting it
puts back the setuid binary an image needs.

`fsGroup` is the one that goes with a volume rather than with the image. A
freshly provisioned volume comes up owned by `root:root`, so a workload that
declared `runAsUser: 1001` is handed one it cannot write: it starts, reads as
healthy, and fails on its first write. `fsGroup` is the gid the kubelet chowns
the volume to before the container starts, and it is what makes a non-root
workload and a volume claim work together at all. `0` is the volume's own
ownership left alone, the reading `runAsUser`'s zero has.

`fsGroupChangePolicy` is when that chown happens, and the default is
deliberately Kubernetes' own — `Always`, which walks the whole volume every
time a pod starts. `OnRootMismatch` skips the walk when the volume's own root
already has the right ownership, which is what turns a minutes-long start on a
large volume into a fast one; the price is that a subtree left behind by a
previous uid stays unwritable, and the failure arrives long after the change
that caused it. That is a trade a project makes knowingly, so the platform
does not make it for one. It needs an `fsGroup` to apply and is refused
without one.

A workload that cannot start under what it asked for says so on its
environment, naming the constraints in force, rather than sitting in
`CrashLoopBackOff` with the reason three layers down.

#### `runtime.init` — a volume the process cannot start on

A [volume claim](api/claims.md) hands a workload an empty filesystem, and a
good deal of software will not start on one: it wants its directory tree to
exist, and sometimes a configuration file it can then rewrite for itself.
`init` is what the platform does inside that volume before the process runs.

```json
{
  "runtime": {
    "init": [
      {
        "volume": "config",
        "directories": [
          {"path": "custom_components"},
          {"path": "secrets", "mode": "0700"}
        ],
        "seed": [{"file": "configuration", "path": "configuration.yaml"}]
      }
    ]
  }
}
```

`volume` names one of the volume claims **this workload mounts** — a claim
names the one process that mounts it, so each workload declares its own. Every
path is relative to that claim's mount path, and a leading slash or a `..` is
not spellable: there is no path here that leaves the volume.

The two steps are the whole vocabulary, and there is deliberately no third that
takes a command. `directories` are created if they are absent and **left
exactly as they are** if they are there, mode and owner both. `seed` copies one
of this project's [`files`](#files) in, and **only where the destination does
not exist** — so a second deploy never clobbers what the application wrote,
which is what makes running this on every start the same as running it once.

A file used only as a seed is one with **no `path`**: it is placed in no
container, because a mounted config file is read-only and, mounted where the
seed writes, would shadow the copy the application then owns.

Nothing is chowned. The steps run in an init container in the workload's own
pod under this project's own `runtime.security`, so a directory they create
comes out owned by the process that will use it — which is what
`security.fsGroup` and `security.runAsUser` already decide. `mode` is octal
written **as a string**, because JSON's numbers are not octal.

It is frozen into the release with the rest of the runtime, so a rollback
restores the tree and the seeds that release started with. A declaration that
cannot be honoured — a volume this workload does not mount, one it mounts
read-only, a seed from a file the project does not declare — stops the deploy
with the reason on the environment; so does a step that fails, in the step's
own words.

### `env` and `previewEnv`

```json
{
  "env": {"NODE_ENV": "production", "LOG_FORMAT": "json"},
  "previewEnv": {"NODE_ENV": "development"}
}
```

Variables **merge by name** rather than replacing the project's list: a name
the file declares wins, a name only the project has is kept, and a name only
the file has is added. That is the opposite of what the file does to
processes, and the reason is not symmetry — a project's variables are how it
reaches its database and its object store, and those arrive as references the
file is not allowed to write. A file that replaced the list would unbind them,
and the failure would be a running application that cannot reach anything.

`previewEnv` replaces a value in preview environments. Every name in it must
also be in `env`: a preview value replaces a value, so the variable has to
have one.

**Values are literal strings and nothing else.** See
[what it cannot set](#what-it-cannot-set).

### `processes`

The workloads the project ships besides its web process —
[the same declaration the API takes](api/processes.md).

```json
{
  "processes": [
    {"name": "migrate", "type": "task", "command": ["npm", "run", "migrate"], "timeout": "10m"},
    {"name": "worker", "type": "worker", "command": ["node", "worker.js"], "replicas": 2},
    {"name": "api", "type": "service", "port": 8080, "build": {"rootDirectory": "services/api", "dockerfileTarget": "api"}},
    {"name": "cache", "type": "service", "port": 6379, "image": {"repository": "docker.io/library/redis", "tag": "7.4"}},
    {"name": "nightly", "type": "cron", "schedule": "0 3 * * *", "command": ["node", "nightly.js"]}
  ]
}
```

| Key | What it does |
|---|---|
| `name` | A DNS label, and not `web` — the web process is the project's own runtime, and this list is what it ships besides it. |
| `type` | `worker` (runs continuously, never addressed), `service` (runs continuously, addressed by the rest of the unit and never published), `cron` (runs on a schedule) or `task` (runs once per deploy, and the release takes no traffic until it succeeds — where a schema migration goes). |
| `command`, `args` | Exec form, as above. |
| `port` | A service's listening port, and the port its siblings reach it on. Required on a service and refused on anything else. |
| `build` | This workload's own build: `strategy` (`auto`, `dockerfile` or `buildpacks`, defaulting to `auto`), `dockerfilePath`, `dockerfileTarget`, and `rootDirectory` relative to the repository root. `auto` is the project's own default read over *this workload's* root directory: a Dockerfile there wins and this is a dockerfile build; otherwise the framework detected there is built with buildpacks; otherwise the build fails with a message naming this workload, the file it looked for and the `strategy` that would settle it. It resolves the builder alone — a workload names its own port and command, which are the other two things detection would answer — and it does not inherit the project's `strategy`. That directory is the workload's build root — `dockerfilePath` is relative to it and nothing above it is part of the build — so a path that leaves it is refused here, exactly as `build.dockerfilePath` is for the project. `dockerfileTarget` is which stage of that file to ship, and it falls back to the project's stage rather than to the file's last one; a stage on a `buildpacks` workload is refused naming that workload. Absent means it runs the project's image with another command. Refused on a `cron`. |
| `init` | What this workload needs done inside the volumes it mounts before its own container starts — [`runtime.init`](#runtimeinit--a-volume-the-process-cannot-start-on) for a named workload, on the same terms. Every type takes it, a `task` included. |
| `image` | An image this platform did not build, and the third answer to the question `build` asks: `repository` (registry host included, without a tag or a digest), one or both of `tag` and `digest`, and an optional `connection` naming the Connection it is *pulled* with — left out for a public image, which is pulled anonymously. It excludes `build`: a workload is built from this repository or published elsewhere, never both. It also takes an optional `signature` — `publicKeySecret`, `identity`, `issuer` — saying whose signature on the image is acceptable; the platform looks either way, and a key is what makes a `verified` answer reachable (see [COMPLIANCE.md §18.5](COMPLIANCE.md)). A unit may mix the two, and they ship in one release that records the digest each workload resolved to, so the whole of it rolls back together. |
| `replicas` | A worker's or a service's copy count. Zero is a workload that is declared and parked. |
| `singleton` | Two of this workload must never run at once, so a deploy stops the old copy before starting the new one. Refuses `replicas` above 1, and refused on a `cron` — that question is `concurrencyPolicy` — and on a `task`, which is one run per deploy. |
| `cpu`, `memory` | Kubernetes quantities. |
| `schedule` | A five-field cron expression, read in UTC. Required for `cron`, refused on every other type. |
| `concurrencyPolicy` | `Allow`, `Forbid` (the default) or `Replace`, when a run is due and the last one has not finished. |
| `timeout` | A Go duration bounding one run. An hour by default — and for a `task`, how long the deploy waits for it before calling it failed. |
| `previews` | Run this workload in preview environments too. A worker and a scheduled job are off unless asked for; a service and a task are on unless they say otherwise, because a preview missing one of its own services — or with a database branch nothing migrated — is a broken preview. |
| `health` | A worker's or a service's health check. A worker's must name its `port` — it publishes none — and a service's falls back to its own. Refused on a scheduled process and on a task, whose verdict is the run's exit status. |

A `task` is the one entry here that is not a thing that keeps running: it is
work that happens once per deploy and finishes before any of that release
serves a request, which is why a schema migration belongs in it and not in the
application's entrypoint. A run that fails stops the deploy where it stands and
leaves whatever was serving serving; tasks run in the order they are declared;
and **reversing a schema change is out of scope** — forward-only, idempotent
work is the contract, and a rollback runs the task the older release declared.
[The workloads page](api/processes.md#work-that-runs-before-a-release-takes-traffic)
is the whole of it.

A `build` here is how a commit that adds a workload builds it: the file is read
at the commit under build, so the set of images a commit produces is the set
that commit declared. Every one of them ships in the same release.

Declaring `processes` **replaces the project's whole list** rather than merging
into it. A workload is defined by the code it runs, so one the commit no longer
names is one whose command may no longer be in the image; merging would keep it
running until somebody noticed.

There is no per-workload idling setting here, and that is deliberate: scale to
zero is the project's policy because only the web process is idled — see
[the Project's `scaleToZero`](CRDS.md) for why, and what to do when one
workload must stay warm.

### `files`

The configuration files this commit places into its workloads — what software
configured by a file at a fixed path rather than by environment variables
needs, and [the same declaration the API takes](api/files.md).

```json
{
  "files": [
    {
      "name": "configuration",
      "path": "/config/configuration.yaml",
      "content": "logger: info\nhttp:\n  server_port: 8123\n"
    },
    {
      "name": "worker-conf",
      "path": "/etc/worker.toml",
      "content": "queue = \"jobs\"\n",
      "workloads": ["worker"]
    }
  ]
}
```

Each file is mounted **read-only at its path**, into every workload it names —
`web` for the web process, and a workload's own name for anything in
[`processes`](#processes). A file that names none reaches all of them, which is
what a vendored application's single config file wants. Only that one path is
replaced; the rest of the directory stays as the image left it.

`path` may be **left out**, and then the file is placed in no container at all:
such a file exists to be copied into a volume by a workload's
[`init`](#runtimeinit--a-volume-the-process-cannot-start-on). A mounted config
file is read-only, so one mounted where the seed writes would shadow the copy
the application then owns — which for Home Assistant's `configuration.yaml` is
the difference between an application that can be configured and one that
cannot.

`content` is **required here**, unlike on the API, because a committed
declaration has nowhere else to have put it. The file is placed exactly as
written — nothing is substituted into it from the environment, and
[the API page](api/files.md#why-they-are-not-templated-from-the-environment)
says why that is a decision rather than a gap.

Declaring `files` **merges onto the project's list by name**, unlike
`processes` and like `env`. The reason is `env`'s rather than symmetry: a
project may hold a *secret* file — one whose content is a credential the
platform keeps where nothing reads it back — and a list that replaced would
take the declaration away and leave the application starting without it. A
name this file declares that the project holds as a secret fails the build,
for the same reason a variable may not shadow a bound one.

They are frozen into the release with everything else, so a rollback restores
the file that release ran with.

### `volumes`

The persistent volumes this commit needs: which [resource
claim](api/claims.md), mounted where, by which process.

```json
{
  "volumes": [
    { "name": "config", "process": "web", "mountPath": "/config" },
    { "name": "media", "process": "web", "mountPath": "/media",
      "source": "bind", "accessMode": "ReadOnlyMany" }
  ]
}
```

**This is the one entry that declares a requirement and never makes one.**
Everything else on this page describes the code and the file may set it. A
volume claim is the project asking the platform for storage — and for
`source: bind`, for storage the platform did not create and does not own —
which is the project's standing rather than a fact about the commit. A file
that could make one would let a pull request mount somebody's NAS export into
its own preview, which is [No credential, ever](#what-it-cannot-set) wearing a
different hat. So `size`, `storageClass` and `bind` are refused here **by
name**, rather than arriving as an unknown field: they are the first things
somebody reaches for, and "unknown field" would be a true answer that explains
nothing.

What the file declares, the build checks against the project's claims, and
each disagreement fails the build with what to change:

| The file says | The build says |
|---|---|
| a claim the project does not have | the claim to make, and the volume claims the project does have |
| a `process` or `mountPath` the claim disagrees with | both, side by side |
| a `source` the claim disagrees with | both — a commit written against twelve terabytes of existing media is not the same application as one handed a fresh empty disk |
| an `accessMode` the claim disagrees with | both — read-only and read-write are the difference between an application that works and one that fails on its first write |

`source` and `accessMode` are optional and declare no opinion when left out.
The middle row is the failure this is actually for: the code writes to `/data`,
the claim mounts `/var/data`, and everything deploys green until the first
restart takes the data with it.

Make the claim in the dashboard or with `kitchen api POST /claims`, and name
it here.

## What it cannot set

The file lives in a repository, and **a preview builds a commit from a pull
request** — which anybody who can open one can write. So its reach is exactly
the settings that describe the code, and stops at everything that describes
the project's standing in the platform.

- **No credential, ever.** A variable takes a literal string. Pointing one at
  a Secret or a resource claim is refused by name, because this file is
  committed and everything in it is public whether or not the platform agrees.
  Those bindings are set in the dashboard, or with `kitchen env set` and
  `kitchen secret` — see [the CLI](CLI.md).
- **No shadowing a bound variable.** A name the file declares that the project
  already takes from a secret or a claim fails the build. Letting the file win
  would let a pull request repoint a database URL at a host it chose; letting
  the project win would leave a value in the repository that reads as though
  it applies and does not.
- **Nothing about the project's standing.** Criticality, data classification,
  residency, RTO and RPO, access grants, promotion stages, `requirePullRequest`,
  whether previews exist, whether they are protected and **how many of them may
  be live at once**: all of it is the project's owners' and the operator's. A
  repository arguing about them is the argument this rule exists to refuse —
  and the preview ceiling most of all, since a file that could raise it would
  be a pull request voting on how much of the cluster its own preview may take.
  It is `previews.max` on the project and `previews.maxPerProject` on the
  platform; [docs/api/projects.md](api/projects.md) and
  [docs/api/settings.md](api/settings.md) carry both.
- **No secret configuration file.** [`files`](#files) declares a file and its
  content; it cannot mark one `secret`, which is refused by name. Whether the
  platform holds a credential for this project is the project's standing
  rather than a fact about the code, and the declaration would be a claim a
  pull request got to make. A secret file is declared in the dashboard or with
  `kitchen files set --secret`.
- **No asking for storage.** [`volumes`](#volumes) declares the volumes the
  code needs and the build checks them; it cannot ask for one to be cut, and
  it cannot name somebody's existing volume — `size`, `storageClass` and
  `bind` are refused by name. Which disk a project is given, and whose
  existing data it may mount, is the project's standing, and a file that could
  say it would let a pull request mount a NAS export into its own preview.
- **Not the root directory**, for the reason in [Where it goes](#where-it-goes).
- **Not the git connection, the repository, the production branch or the
  registry.** A file cannot say which repository it is in.
- **No notification subscription.** Where the platform sends an account of
  what this project is doing — and the key those payloads are signed with — is
  the project admin's, on a route that never reads the key back. A file that
  could add one would be a pull request choosing where the project's activity
  is posted. See [docs/api/notifications.md](api/notifications.md).

**Nothing here is file-*only*, and that is deliberate.** Everything this file
can set has a route as well — the build settings and the runtime on
`PATCH /projects/{name}`, the variables on `PATCH /projects/{name}/env`, the
workloads on `processes` — because a project whose source is an image has no
repository, so it has no file, and a setting reachable only from a file would
be a setting such a project could never make.
[Workloads](api/processes.md#a-project-with-no-repository-declares-all-of-this-here)
maps the two onto each other field by field. The traffic runs the other way
once: `build.rootDirectory` is the API's and not the file's, for the reason in
[Where it goes](#where-it-goes). And [`volumes`](#volumes) sets nothing at
all — it declares what the code needs and the build holds it against the
claims `POST /claims` made, which is the route behind it.

## What a bad file does

It **fails the build**, with the line to fix, and it fails it before anything
is scheduled. Unreadable JSON, a key the platform has never heard of, a value
out of range, and any of the refusals above are all final — the same commit
will not parse differently next time — so the build fails rather than waiting.

A key nothing recognises is refused rather than dropped, which is worth
stating on its own: the file exists so that committing a change to it changes
the deploy, and a typo'd key that silently did nothing would make that untrue
in exactly the case where somebody is watching for the change and does not get
it.

The one failure that is not final is the repository not being **readable**
right now — a provider that is down, a token that stopped working. The commit
did nothing wrong, so its build waits, exactly as framework detection's does.

The Ready condition's reason is `ConfigInvalid` either way you meet it: at the
start of a build, or at the end, where a conflict between the file and the
project is settled. The end is where the shadowing refusal lands, because the
file is read before the build and the release is written after it.

## Where it shows up afterwards

- **On the build.** `GET /builds/{name}` answers `config` with the file's path
  and everything it declared — see [Builds](api/builds.md#what-the-commit-configured-for-itself).
  The build screen says the same thing.
- **On the project's settings screen.** A notice above the form lists the
  settings the repository has taken over, so that nobody edits a field the
  next build sets back. The dashboard is still where the rest of them live.
- **In the release.** The merged result is what the Release's snapshot froze,
  which is what makes a rollback replay the configuration its commit declared.
  `kitchen rollback` shows the difference before it asks.

## Decisions

| | |
|---|---|
| **The file wins for what it names** | A value written in a file that is read on every build is a value that takes effect, or the file is decoration. The alternative — the dashboard wins, the file is a default — means committing a change to it usually does nothing, silently, which is the worst of the three options. |
| **It is per commit, not a write to the project** | The file configures the build that read it and the release that build produced. It never writes back to the Project, so there is no loop between two writers and no moment where a merged pull request has silently changed a setting for the environment that is already running. |
| **`env` merges, `processes` replaces** | The two lists fail differently. An unbound variable is an application that cannot reach its database; a stale worker is a command that may no longer exist. Merging is right for the first and wrong for the second. |
| **`files` merges, like `env`** | For `env`'s reason and not for symmetry: a project may hold a secret file this file may not declare, and replacing the list would take that declaration away. The failure would be an application starting without the credential file it is configured by, which is the same shape an unbound variable has. |
| **The values are not answered back** | `config` on a build names the settings, not what they were set to. They are already in the release's snapshot and on the environment, and a second copy is a second thing to disagree with the first. |
| **No `buildCommand` or `outputDirectory`** | Kitchen has neither concept: a buildpacks build runs the project's own `build` script and the framework decides the directory a static site is served from, and a Dockerfile build is the Dockerfile. Adding them to this file would be adding them to the platform, which is a different change with a different argument. |
| **The schema is hand-written and checked against the parser** | The wording is half of what the schema is for, so it is not generated — and `internal/repoconfig/schema_test.go` fails when a key exists on one side and not the other, which is the only way the two can drift. |

## Open

- **No `kitchen config diff`.** `kitchen config check` says what the file
  declares; it does not say what that changes about the project it will be
  built for, because that needs the project and the check is deliberately
  offline. A second command that took a credential could answer it.
- **One file per project, not per environment.** A file cannot say "this in
  production, that in staging" beyond `previewEnv` and `previewArgs`. Whether
  the next thing is per-environment blocks or a promotion-stage overlay is
  undecided, and neither is needed until somebody has three stages.
- **Nothing generates a starter file.** `kitchen config check` refuses to
  invent one, and `kitchen projects create` does not write one. Detection
  already answers most of what a first file would say, so an initialiser would
  mostly write down the defaults — which is exactly the file that then goes
  stale.
