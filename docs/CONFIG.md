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
| `dockerfilePath` | The Dockerfile, relative to the project's root directory. Used when the strategy is, or resolves to, `dockerfile`. |

```json
{"build": {"strategy": "dockerfile", "dockerfilePath": "docker/prod.Dockerfile"}}
```

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

A workload that cannot start under what it asked for says so on its
environment, naming the constraints in force, rather than sitting in
`CrashLoopBackOff` with the reason three layers down.

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

The workers and scheduled jobs the project runs besides its web process —
[the same declaration the API takes](api/processes.md).

```json
{
  "processes": [
    {"name": "worker", "type": "worker", "command": ["node", "worker.js"], "replicas": 2},
    {"name": "nightly", "type": "cron", "schedule": "0 3 * * *", "command": ["node", "nightly.js"]}
  ]
}
```

| Key | What it does |
|---|---|
| `name` | A DNS label, and not `web` — the web process is the project's own runtime, and this list is what it runs besides it. |
| `type` | `worker` (runs continuously) or `cron` (runs on a schedule). |
| `command`, `args` | Exec form, as above. |
| `replicas` | A worker's copy count. Zero is a worker that is declared and parked. |
| `singleton` | Two of this worker must never run at once, so a deploy stops the old copy before starting the new one. Refuses `replicas` above 1, and refused on a `cron` — that question is `concurrencyPolicy`. |
| `cpu`, `memory` | Kubernetes quantities. |
| `schedule` | A five-field cron expression, read in UTC. Required for `cron`, refused on a `worker`. |
| `concurrencyPolicy` | `Allow`, `Forbid` (the default) or `Replace`, when a run is due and the last one has not finished. |
| `timeout` | A Go duration bounding one run. An hour by default. |
| `previews` | Run this process in preview environments too. Off unless asked for. |
| `health` | A worker's health check, which must name its `port` — a process publishes none. Refused on a scheduled process, whose verdict is its run's exit status. |

Declaring `processes` **replaces the project's whole list** rather than merging
into it. A worker is defined by the code it runs, so a worker the commit no
longer names is one whose command may no longer be in the image; merging would
keep it running until somebody noticed.

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
  whether previews exist and whether they are protected: all of it is the
  project's owners' and the operator's. A repository arguing about them is the
  argument this rule exists to refuse.
- **Not the root directory**, for the reason in [Where it goes](#where-it-goes).
- **Not the git connection, the repository, the production branch or the
  registry.** A file cannot say which repository it is in.

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
