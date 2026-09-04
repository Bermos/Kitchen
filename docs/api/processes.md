# Workloads

A Project deploys a web process: an image behind an HTTPRoute, on a URL. Real
applications also have a queue worker, a nightly job and — for a repository
that ships more than one thing — an API the front end talks to and the internet
does not. All of those are workloads of **one project**, because a project is
one thing that ships as a whole.

That is the line, and it is the whole of the modelling decision (#271): *one
project equals one thing that ships as a whole*. A monorepo of genuinely
unrelated applications is several projects; a set of workloads that deploy and
roll back together is one. There is no tier above the project — it would double
every route in the authorization table, make the CLI's project argument
ambiguous and touch every screen — so a unit of four workloads is a project
with four entries in this list.

- A **worker** runs continuously and is never addressed: a Deployment with no
  Service and no route. Nothing publishes it, so nothing needs a certificate
  for it — and nothing *else's* Service reaches it either. Every pod an
  environment materializes carries the same environment label, so the pods
  behind the URL carry a second one, `kitchen.bermos.dev/component: web`, and
  that is what the environment's Service selects on. Without it a worker — no
  port, and none wanted — was an endpoint of the URL that refused every
  connection it was handed.
- A **service** runs continuously and *is* addressed, from inside the cluster
  and from nowhere else: a Deployment and a ClusterIP Service, and still no
  route. Its siblings reach it at `<environment>-<name>` in the project's
  application namespace, which they are handed as
  `KITCHEN_SERVICE_<NAME>` — so a preview's web process talks to the preview's
  own API and production's talks to production's.
- A **scheduled job** runs on a cron expression: a `batch/v1` CronJob, and one
  firing is a *run*.
- A **task** runs **once per deploy** and has to finish before any of that
  release takes traffic: a `batch/v1` Job, one run, no replicas and no
  schedule. It is where a schema migration goes — see
  [Work that runs before a release takes traffic](#work-that-runs-before-a-release-takes-traffic).

There is no `web` in the list, and it is not an omission. The web process is
the project's own `runtime` — its port, replicas and resources — and it is
singular because the URL is: an environment publishes one hostname, one Service
and one route, and a second process claiming to be the web one would have to be
told which of those it got.

**Publishing is the exception a project declares.** The web process is the one
workload with a URL; nothing in this list gets a route, whatever its type, and
a workload that should be on the internet is the web process. A workload is
therefore private by saying nothing, rather than by somebody remembering to
turn something off.

## Declaring them

The list is a project setting, written with the rest of them.

```http
PATCH /api/v1/projects/{name}
```

**Needs `admin`.** It is the same bar as the web process's replica count and
its resources, because it is the same decision: what this project runs, and how
much of it.

```json
{
  "processes": [
    {
      "name": "migrate",
      "type": "task",
      "command": ["npm", "run", "migrate"],
      "timeout": "10m"
    },
    {
      "name": "worker",
      "type": "worker",
      "command": ["node", "worker.js"],
      "replicas": 2,
      "memory": "512Mi"
    },
    {
      "name": "api",
      "type": "service",
      "port": 8080,
      "build": { "rootDirectory": "services/api" }
    },
    {
      "name": "nightly-report",
      "type": "cron",
      "schedule": "0 3 * * *",
      "command": ["node", "report.js"],
      "timeout": "30m",
      "concurrencyPolicy": "Forbid"
    }
  ]
}
```

| Field | Applies to | What it means |
| --- | --- | --- |
| `name` | all | A DNS label, unique within the project. It names the workload (`<environment>-<name>`) and is the log store's `process:`. `web` is refused. |
| `type` | all | `worker`, `service`, `cron` or `task`. |
| `command`, `args` | all | Exec form — a list of words, never a shell line. Absent runs the image's own entrypoint, which is what a workload with its own image wants and never what a buildpacks-built one does. |
| `port` | service | The port it listens on, and the port its siblings reach it on — the same number, deliberately. **Required on a service and refused on anything else**: only a service is addressed, and a port on a worker would read back as a setting that took while nothing ever connected to it. |
| `build` | worker, service, task | This workload's own build — see [Several workloads, one commit](#several-workloads-one-commit). Absent means it runs the project's image with another command. Refused on a cron process. |
| `image` | all | An image this platform did not build — see [A workload that runs a vendored image](#a-workload-that-runs-a-vendored-image). It excludes `build`: a workload is built here or published elsewhere, never both. Absent from both still means the project's own image run with another command. |
| `replicas` | worker, service | How many copies. `0` is allowed: a workload declared and parked, which is how one is turned off without losing its command. |
| `singleton` | worker, service | Two of this workload must never run at once — see below. Refuses `replicas` above 1, and refused on a cron process and on a task, neither of which has a second copy to overlap. |
| `cpu`, `memory` | all | Kubernetes quantities, applied as request and limit alike — the same two strings the web process takes. |
| `schedule` | cron | A five-field cron expression, **read in UTC**. Required on a cron process and refused on every other type — a worker and a service run continuously, and a task runs once per deploy. |
| `concurrencyPolicy` | cron | `Allow`, `Forbid` (the default) or `Replace`. A job that takes longer than its interval is far more often running behind than meant to run twice. |
| `timeout` | cron, task | A Go duration bounding one run; an hour by default. It becomes the Job's `activeDeadlineSeconds` — and for a task it is how long the deploy waits before calling it failed. |
| `health` | worker, service | A health check, the same shape the project's web process takes. A worker's **must name the `port`** it is made against, because a worker publishes none of its own; a service's falls back to its own port. Refused on a cron process and on a task: how a run went is its exit status, not a probe. |
| `previews` | all | Whether it runs in preview environments. **Off for a worker and a scheduled job unless asked for; on for a service and a task unless they say otherwise** — see below. |

A worker is probed only where it asked to be, which is the opposite of the web
process — every environment of a project is probed whether or not it declared
a check, because there is a port to fall back on. A worker with no health
listener has none, so its liveness is whether its process is still running:

```json
{"name": "worker", "type": "worker", "command": ["node", "worker.js"],
 "health": {"path": "/healthz", "port": 9000}}
```

The timings and their defaults are the web process's — see
[Changing a project's settings](projects.md#changing-a-projects-settings).

The write **replaces the whole list**; an empty list removes every process. It
is a project *declaration*, so it reaches an environment through the next
Release: what is running keeps the processes its own release declared until
something builds. That is the same rule the port and the replica count follow,
and it is what makes a rollback exact — the release being rolled back to runs
the worker command it was built with, not today's.

Idling is not among these fields: a project scales to zero as a whole, because
only the web process sits behind the interceptor that measures the request
pressure it is scaled on — [CRDS.md](../CRDS.md) states why a `service` and a
`worker` are never idled, and what to do when one workload must stay warm.

### A worker that must never run twice

`singleton` is [the project runtime's own declaration](projects.md#changing-a-projects-settings)
read for one process, and a worker is the workload it matters most to. Left
off, a worker is rolled out the way anything else is: at one replica the
default rolling update surges to a second copy and takes none away, so every
deploy runs two of it for a few seconds. For a queue consumer that is usually
fine. For a poller, a scheduler or an ingest loop it is the loop running twice
against a shared store — which is not an error at the time, and is a doubled
request rate nobody attributes to a deploy afterwards.

```json
{"name": "poller", "type": "worker", "command": ["node", "poll.js"], "singleton": true}
```

It becomes `strategy: Recreate` on the worker's Deployment: the old copy stops
before the new one starts. Nothing addresses a worker, so what the web process
pays as a gap in serving a worker pays as a few seconds of not consuming.

Two rules go with it, both at admission and at this route:

- **More than one replica is refused, not clamped.** A count quietly lowered
  reads back as a setting that did not take, and the project would go on
  believing it runs three.
- **A cron process cannot be one.** Whether two of its runs may overlap is
  `concurrencyPolicy`, whose default is already `Forbid`; a second spelling of
  that question would be a setting that read back and did nothing.

One replica does not imply it. A queue consumer at one replica is usually fine
overlapping, and inferring the constraint from the count would make the count
mean two things.

### Work that runs before a release takes traffic

A schema migration had nowhere to go, and a readiness probe is not where it
goes. The distinction is worth being precise about: a readiness check stops
traffic reaching a pod that is not ready. It does **not** stop the previous
release's pods being retired while a migration is half applied. Running the
migration from the application's own entrypoint — which is what teams do
instead — runs it once per replica, concurrently, on every rollout.

```json
{"name": "migrate", "type": "task", "command": ["npm", "run", "migrate"], "timeout": "10m"}
```

A task is a `batch/v1` Job that the environment's reconcile waits for:

- **Once per deploy, whatever the replica count.** One Job, one run. The
  environment records which Release the run was for, so however many times it
  reconciles, one release runs one migration.
- **Nothing of that release starts until it succeeds.** Not the web
  Deployment, not the workers, not the services, not the route. A unit deploys
  as one, so "takes traffic" means all of it.
- **A failure fails the deploy, visibly, with the output.** The environment
  goes `Degraded`, carries the run's own message on its `DeployTasksComplete`
  condition, reports a failed deployment status to the commit, and the release
  does not land — **whatever was serving keeps serving**. It is not a warning
  and it does not decay into one.
- **It is the environment's own run.** The same image, variables, claim
  bindings, volumes, pull secret and security posture the environment's other
  workloads get, so a preview's run touches the preview's own branch of the
  database and nothing else. It is not selected by the environment's Service:
  the pods carry the environment and process labels and no `component` label,
  which is what keeps a migration pod out of the URL's backends.
- **A rollback runs the work the release being rolled back to declared.** That
  release's own process list, its own image, its own command — the same rule
  every other workload follows.

Several tasks run **in the order they are declared**, one at a time: "migrate,
then seed" is a sentence, and running them together would make the second
depend on a race. A task behind one that has not succeeded does not start.

**Reversing a schema change is out of scope**, deliberately and permanently.
Forward-only, idempotent work is the contract; a rollback runs the older
release's task, and an application whose old code cannot read the new schema
has a problem no deploy-time hook can solve. Say so in the migration, not to
the platform.

**A task runs in previews unless it says otherwise**, which is the service's
default and for the same reason: a preview gets its own branch of the
database, or an empty one, and a branch nothing has migrated is a preview that
comes up broken.

Two deploys racing serialize rather than collide. If a release arrives while
the previous deploy's run is still going, the new one **waits for it** rather
than killing it: stopping a migration half way through to start another is the
failure this feature exists to prevent, not a tidier version of it. The wait is
bounded by the running task's own `timeout`.

A task that fails is not retried by the platform — `backoffLimit` is zero, so a
failed run is a failed deploy rather than a burst of pods. Retrying it is a
decision somebody makes: build again with the migration fixed, or ask for the
run again with [Running one now](#running-one-now).

### Several workloads, one commit

A workload with a `build` is built from its own directory of the repository,
which is what makes a monorepo one project instead of four:

```json
{"name": "api", "type": "service", "port": 8080,
 "build": {"strategy": "dockerfile", "dockerfilePath": "Dockerfile",
           "dockerfileTarget": "api", "rootDirectory": "services/api"}}
```

A workload that names no `strategy` is `auto`, which is the case a monorepo is:
`services/api` with a Dockerfile beside `services/worker` without one builds
both, and neither has to say which builder it is. What each workload's `auto`
resolved to is reported per workload by
[`GET /builds/{name}`](builds.md), as `detectedFramework`.

| Field | What it means |
| --- | --- |
| `strategy` | `auto` (the default), `dockerfile` or `buildpacks` — the project's own three. `auto` reads **this workload's** `rootDirectory` at the commit: a Dockerfile at its `dockerfilePath` wins and this is a dockerfile build; otherwise the framework detected there is built with buildpacks; otherwise the build fails naming this workload, the file it looked for and the strategy that would settle it. It resolves the builder alone — a workload names its own port and its own command, which are the other two things detection would answer — and it is never the project's `strategy` inherited: a workload's is its own. |
| `dockerfilePath` | The Dockerfile, relative to `rootDirectory`. Defaults to `Dockerfile`. |
| `dockerfileTarget` | Which stage of that Dockerfile produces this workload's image. Left out, the project's stage stands in — see below. |
| `rootDirectory` | The directory this workload is built from, **relative to the repository root** — not to the project's own root directory, since the whole point is that each workload names where it lives once. |

### Which stage each workload ships

One multi-stage Dockerfile that yields an API, a worker and a migration runner
is the ordinary shape of a monorepo, and it is the case
[`dockerfileTarget`](builds.md#which-stage-of-the-dockerfile-it-shipped) exists
for: without a stage, each of those workloads ships whichever stage the file
happens to end on — a build that succeeds and produces the wrong thing.

There is **one chain**, and every image the commit produces resolves through
it:

1. the workload's own `build.dockerfileTarget`, where it declared one;
2. the commit's own `kitchen.json` (`build.dockerfileTarget`);
3. the project's `dockerfileTarget`.

Nothing at the end of it is the file's last stage, which is what every build
shipped before any of this existed. A workload that names no stage therefore
**inherits the unit's** rather than resetting to the last stage: the unit names
its stage once, and each workload that differs says so. The project's own
setting is the web process's.

A workload whose `strategy` is `buildpacks` inherits nothing — the lifecycle has
no stages, so the unit's stage is not a stage of anything that image builds, and
a unit that named one would otherwise be unable to ship a single buildpacks
workload. One that names a stage *itself* keeps it and is refused for it, which
is the mistake worth reporting.

Both refusals name the workload, because one commit now fails on one of several
images:

- a stage the workload's Dockerfile does not declare fails the build with
  `DockerfileTargetNotFound`, naming the workload, its file and the stage;
- a stage on a workload whose strategy is `buildpacks` — which has no stages —
  fails the build with `DockerfileTargetNotSupported` **before any Job exists**,
  naming that workload. The whole unit is refused rather than the one workload:
  a release is all of it or none.

A name no stage could have — anything but a letter followed by letters, digits,
dots, dashes and underscores — is a `400` naming the workload and the field, on
the same rule and in the same words the project's own target is refused by.

`GET /builds/{name}` reports the stage each image was actually built to,
recorded when its Job was created; `GET /environments/{name}/processes` reports
what each workload *declared*, which is the release's and does not move when
the project's settings do.

**A workload's `rootDirectory` is that workload's build root**, on exactly the
terms [a project's is](projects.md#creating-a-project): it is what is built,
`dockerfilePath` is relative to it, and nothing above it is part of that
workload's build. So a workload in `services/api` whose build file is
`services/api/docker/prod.Dockerfile` sends `"rootDirectory": "services/api"`
and `"dockerfilePath": "docker/prod.Dockerfile"`, and a path that leaves what
its build sees — `../shared/Dockerfile`, `/Dockerfile` — is a `400` naming the
workload and the field rather than a build that fails without being able to
say why. Both paths are spelled once by the platform, so `services/api`,
`./services/api` and `services/api/` are one directory here exactly as they
are on the project.

What that produces:

- **One commit, one coordinated release.** Every workload that declares a build
  gets a Job of its own, created in the same pass and pushed to a repository
  beside the project's own (`<registry>/<project>-<workload>`). The Build
  succeeds only when all of them pushed, and the first one to fail fails the
  Build naming itself — three of four workloads a commit ahead of the fourth is
  worse than a deploy that did not happen. `GET /builds/{name}` lists them under
  `workloads`.
- **A rollback restores the exact set.** The Release records the digest each
  workload was built to (`workloads` on the release view) beside the process
  list it froze. Restoring it restores that set — a workload added since does
  not appear, and one whose image changed goes back to the image it had.
- **One thing to configure.** Environment variables, secrets and claim bindings
  are the project's, shared by every workload of the unit. That is the case
  that motivated the feature — workloads that genuinely share a database and a
  credential set — and it is what "the unit remains one thing to configure"
  means. A workload that needs its own variables is a second project.

A workload with no `build` runs the project's image with another command, which
is what a worker sharing the web process's codebase wants and is one build
rather than two.

### A workload that runs a vendored image

`build` says how a workload's image is produced from the repository, and its
absence used to mean one thing. `image` is the third answer: an image this
platform did not build.

```json
{"processes": [
  {"name": "cache", "type": "service", "port": 6379,
   "image": {"repository": "docker.io/library/redis", "tag": "7.4"}},
  {"name": "api", "type": "service", "port": 8080,
   "build": {"rootDirectory": "services/api"}}
]}
```

| Field | What it means |
| --- | --- |
| `repository` | Where the image lives, registry host included and without a tag or a digest. |
| `tag` | The version as the vendor publishes it. One of `tag` and `digest` is required. |
| `digest` | `sha256:` and sixty-four hex digits, pinning the exact content. Naming both means "this tag, and it must still be this content". |
| `connection` | A Connection with the `imageStore` capability holding a docker config for that registry. **Left out for a public image**, which is pulled anonymously. |

The unit above is the case this exists for: an upstream image as one workload
and a sidecar built from this repository as another, in one Release. It
deploys as one, previews the built half's pull requests, and **rolls back as
one** — the Release records the digest every workload resolved to, and
restoring it restores that exact set whether the platform built the image or
not.

**The credential is the image's, not the project's.** A workload with an
`image` pulls with the Connection named on it — or with nothing where it names
none — while everything the platform built still pulls with what the build
pushed under. The two are different registries and, for a vendored image,
different accounts.

`image` takes no strategy, no root directory and no Dockerfile, because there
is nothing to detect. A workload of a project **with no repository at all** can
only be vendored; declaring a `build` there is refused, since there is no
repository to build from — see
[a project whose software this platform did not build](projects.md#a-project-whose-software-this-platform-did-not-build).

### A project with no repository declares all of this here

A repository declares its workloads in [`kitchen.json`](../CONFIG.md), read
at the build root of every commit. **A project whose source is an image has no
repository, so it has no file** — and this route is not a fallback for it, it
is the whole of how its unit is declared. The same is true of the dashboard's
workloads editor and of `kitchen processes set`; #299 filed no command for
declaring workloads because the file was the real surface, and that reasoning
ends where the file does.

Nothing `kitchen.json` can say is file-only. Every field of it has a route,
and they are these three:

| `kitchen.json` | Over the API |
| --- | --- |
| `build.strategy`, `build.dockerfilePath`, `build.dockerfileTarget` | `buildStrategy`, `dockerfilePath`, `dockerfileTarget` on [`PATCH /projects/{name}`](projects.md#changing-a-projects-settings) — a project with no repository builds nothing, so all three are refused there |
| `runtime.port`, `replicas`, `singleton`, `notRequestDriven`, `command`, `args`, `previewArgs`, `resources.cpu`, `resources.memory`, `health`, `security` | the fields of the same names on `PATCH /projects/{name}` |
| `env`, `previewEnv` | `value` and `previewValue` on [`PATCH /projects/{name}/env`](projects.md#changing-a-projects-environment-variables), which also takes the references a committed file may not: a variable in a repository is public by construction |
| `processes` | `processes` on `PATCH /projects/{name}` — the list above, field for field, validated by the same code (`internal/appconfig`) so the file and the route cannot disagree about what a workload is |

The traffic goes the other way once. **`build.rootDirectory` is the one setting
the API takes and the file may not**: it is how the platform found the file, so
a file that moved it would have to be read before it could say where to read
it. A project with no repository has no build root to name either way.

**A project that has a repository is unaffected, and the file still wins.** The
API does not refuse a `processes` write on one — refusing it would leave the
dashboard's editor dark for the projects that most often want a fifth worker
added in a hurry — but the file is read at every build and its `processes`
replace the project's wholesale, so a write here holds only until the next
build. That is exactly what `build.strategy` and `runtime.port` already do, it
is what `GET /builds/{name}`'s `config.declares` reports, and it is what the
dashboard says above the form: *the repository has taken this over*. Change the
file instead.

**The list reads back the way it is written**, which is what lets a client edit
one workload without losing the others. Two fields carry that on their own:

- `replicas` is reported even when it is `0`, because zero is a count somebody
  chose — a workload declared and parked — and an omitted zero would have every
  editor turn it back on.
- `previews` is reported as the workload *declared* it, and is absent where it
  declared nothing and takes its type's default. What an environment does with
  it is `suspended` on that environment's own listing, which is a different
  question.

### Reaching one workload from another

A service is the only workload anything addresses, and it is addressed from
inside the cluster alone. Every workload of the environment — the web process,
the workers, the scheduled runs — is handed three variables per service:

| Variable | Value |
| --- | --- |
| `KITCHEN_SERVICE_<NAME>` | `http://<host>:<port>` |
| `KITCHEN_SERVICE_<NAME>_HOST` | the fully qualified name |
| `KITCHEN_SERVICE_<NAME>_PORT` | the port |

`<NAME>` is the workload's name upper-cased with its dashes turned into
underscores: `api-gateway` becomes `KITCHEN_SERVICE_API_GATEWAY`. There are
three of them and not one because the URL is right for anything speaking HTTP
and wrong for everything else — a database driver, a gRPC client, a queue reads
the host and the port without the platform pretending to know a protocol it was
never told.

They go in with `PORT` and `KITCHEN_URL`, ahead of the project's own variables,
so a project that sets one of them still wins. A **service's own `PORT` is its
own**, not the web process's: a buildpacks-built image listens on `$PORT`, and
the Service in front of a service targets the port it declared, so handing it
the web process's number would produce a workload listening where nothing
connects. A worker keeps the unit's `PORT`, because it publishes nothing. They name **this environment's**
services, so a preview's web process reaches the preview's own API: four
related workloads as four projects would yield four unrelated preview
environments per pull request, none of which could find the others, and that is
the whole argument for a unit.

A service the environment does not run gets no variables at all. An address
that resolves to nothing is worse than no address — the application would fail
and blame the network.

### Previews run the whole unit

`previews` reads differently by type, and both readings are decisions rather
than omissions.

For a **worker and a scheduled job** it is `false` unless the workload says
otherwise. A preview that emails customers nightly is a bad afternoon; a
preview worker draining the production queue is a worse one. A preview shares
the project's environment variables, so unless somebody set a `previewValue`,
it is pointed at whatever the production process is pointed at.

For a **service** it is `true` unless the workload says otherwise. A service is
addressed by its own environment's siblings and by nothing else, so leaving it
out of a preview protects nothing and breaks the preview: the web process comes
up pointed at a Service with no pods behind it. A preview of a multi-workload
unit has to be the whole set, or the preview is a misleading artifact rather
than a useful one.

For a **task** it is `true` unless the workload says otherwise, for the same
reason one level down: a preview gets its own branch of the database, and a
branch nothing has migrated is a preview that comes up broken. It runs against
the preview's own resources — the preview's bindings, never production's — so
the capability that would be dangerous is not one it has.

Either way, a workload the environment declares and does not run is listed with
`suspended` and a `reason` rather than left out.

## Reading what an environment runs

```http
GET /api/v1/environments/{name}/processes
```

**Needs `viewer`.** Answers the list the environment's *release* declared,
joined to what the reconciler last saw of each.

```json
{
  "items": [
    {
      "name": "worker",
      "type": "worker",
      "command": ["node", "worker.js"],
      "replicas": 2,
      "readyReplicas": 2,
      "memory": "512Mi",
      "workload": "shop-production-worker",
      "healthy": true
    },
    {
      "name": "api",
      "type": "service",
      "port": 8080,
      "replicas": 1,
      "readyReplicas": 1,
      "address": "http://shop-production-api.kitchen-shop.svc.cluster.local:8080",
      "image": "registry.example.com/kitchen/shop-api@sha256:9f2c…",
      "build": { "strategy": "dockerfile", "dockerfilePath": "Dockerfile", "dockerfileTarget": "api", "rootDirectory": "services/api" },
      "workload": "shop-production-api",
      "healthy": true
    },
    {
      "name": "nightly-report",
      "type": "cron",
      "schedule": "0 3 * * *",
      "concurrencyPolicy": "Forbid",
      "timeout": "30m0s",
      "workload": "shop-production-nightly-report",
      "active": 0,
      "lastRun": {
        "name": "shop-production-nightly-report-29387520",
        "phase": "Failed",
        "startedAt": "2026-08-24T03:00:04Z",
        "finishedAt": "2026-08-24T03:00:37Z",
        "durationSeconds": 33,
        "message": "BackoffLimitExceeded: Job has reached the specified backoff limit"
      },
      "lastFailure": {
        "name": "shop-production-nightly-report-29387520",
        "phase": "Failed",
        "startedAt": "2026-08-24T03:00:04Z"
      },
      "healthy": false
    },
    {
      "name": "migrate",
      "type": "task",
      "command": ["npm", "run", "migrate"],
      "timeout": "10m0s",
      "deploy": "complete",
      "lastRun": {
        "name": "shop-production-migrate-7",
        "phase": "Succeeded",
        "startedAt": "2026-08-24T14:19:02Z",
        "finishedAt": "2026-08-24T14:19:24Z",
        "durationSeconds": 22
      },
      "healthy": true
    }
  ]
}
```

`healthy` is the platform's own verdict — a worker with no ready replica, a
schedule whose most recent run failed, a deploy task that failed — rather than
something each client derives, so the dashboard and the CLI cannot disagree
about what a red dot means.

`deploy` is a **task's** own field and is absent on everything else. It says
what the task is doing to *this* deploy — `pending`, `running`, `complete` or
`failed` — which is not the same question as how its last run went: a run
recorded against another release has not happened for this one, however well it
went, which is exactly what a rollback looks like. `failed` is a release that
did not land, and what was serving before it still is.

`address` is where a service answers **inside the cluster**, and it is the
same value its siblings read out of `KITCHEN_SERVICE_<NAME>`. It is not a
public URL and there is not one: nothing in this list is published. `image` is
present only for a workload built from its own directory — everything else runs
the release's own image.

`lastFailure` is kept until a **later failure** replaces it, never until a
success does. A job that fails four nights in five must not read as healthy on
the fifth, and a `CronJob` whose pods fail silently is the classic way this
whole feature disappoints.

A preview lists the project's whole set, with the ones it does not run marked
`suspended` and carrying the `reason`. A shorter list would read like a bug.

An environment on a release the reconciler has not reached yet answers with the
declaration and no live state. That is not reported as unhealthy: nothing is
known, which is a different thing from something being wrong.

## The runs of a scheduled job, or of a task

```http
GET /api/v1/environments/{name}/processes/{process}/runs
```

**Needs `viewer`.** The runs the cluster still holds, newest first — a
schedule's firings, or a **task's one run per deploy**, which reads as the
history of this environment's deploys. One endpoint for both, because a run is
a run: what it printed and how it ended does not change with what started it.
Each one is the Job that was the run:

```json
{
  "items": [
    {
      "name": "shop-production-nightly-report-29387520",
      "phase": "Failed",
      "startedAt": "2026-08-24T03:00:04Z",
      "finishedAt": "2026-08-24T03:00:37Z",
      "durationSeconds": 33,
      "message": "BackoffLimitExceeded: Job has reached the specified backoff limit"
    }
  ]
}
```

Phase is `Running`, `Succeeded` or `Failed`, read off the Job's **conditions**
rather than its failed-pod count: a run that hit its timeout was killed rather
than observed failing, and has no failed pod to count.

A run is not retried. `backoffLimit` is zero and the restart policy is `Never`,
so a scheduled run that failed is a failed run — the schedule is what tries
again. A backoff limit above zero would turn one nightly failure into a burst
of pods and one activity entry that arrived minutes late.

The platform keeps the last three successful runs and the last five failed
ones of a schedule, and the last five runs of a task, and collects the rest, so
this list is short by design. A **run started by hand** belongs to no schedule
and so falls off no history limit; it is kept for seven days after it finishes
and then collected, which is long enough for whoever started it to come back
to it. **A run's output is not** bounded by any of that: the log store keys on
the Job's name, so a collected run is still readable.

```http
GET /api/v1/environments/{name}/logs?run=shop-production-nightly-report-29387520
```

`?process=` narrows to everything one worker or scheduled job wrote, and
`?run=` to one firing. Both compose with the window, the search and `--follow`
exactly as the other log filters do, and both are terms in the query language
too: `process:worker level:error`.

Asking a worker or a service for its runs is refused with `400`, not `404`: it
has none because it is already running, and a not-found would suggest a
workload that plainly exists does not.

## Running one now

```http
POST /api/v1/environments/{name}/processes/{process}/runs
```

**Needs `developer`** — the same bar as redeploying, and for the same reason.
It runs the project's own code with the project's own credentials at a moment
of somebody's choosing, and a person who may push a commit that changes what
the job does cannot be meaningfully stopped from running the job.

The body is empty, and that is load-bearing. Nothing from the request reaches
the pod; the only caller-supplied values are the two names in the path.

For a **scheduled job** the Job is a copy of the CronJob's own template, so a
manual run is the schedule firing early rather than a different thing that
resembles it. Answers `202` with the run:

```json
{ "name": "shop-production-nightly-report-manual-x7k2p", "phase": "Running", "startedAt": "2026-08-24T14:22:10Z" }
```

The run's concurrency policy still applies: a job set to `Forbid` that is
already running gets a second run the scheduler drops.

For a **task** it means *run that again for the release this environment is
on*, which is how a deploy a failed migration stopped is picked back up once
the cause is gone. The API creates nothing: the run is the deploy's — the
environment's variables, its resources, and the same gate in front of the
release — so what it writes is the one fact the reconciler decides from, and
the reconciler makes the run. If it succeeds the deploy carries on by itself.
The answer names the run that is about to exist, which is what `202` is for:

```json
{ "name": "shop-production-migrate-3", "phase": "Running" }
```

The failed run is left exactly where it is, so its output and its message stay
readable beside the new attempt. A task whose run is **still going** is refused
with `400` naming that run: the deploy is already waiting for it, and a second
migration beside the first is what running this once per deploy exists to
prevent. The wait is bounded by the task's own `timeout`.

## Failures are visible without `kubectl`

Every run that reaches a terminal state lands in the activity feed as
`run.succeeded` or `run.failed`, naming the project, the environment, the
process and the run, with the run's duration as its value; a run started by
hand also announces itself as `run.started`. The feed entry is what makes the
*absence* of an entry mean something.

The most recent failure additionally stays on the environment's status until a
later failure replaces it, which is what the environment screen reads. Between
the two, a nightly job that stopped working is something a person trips over
rather than something they have to go and check.

A **deploy task** is louder still, because a failed one means the release never
landed: the environment goes `Degraded`, its `DeployTasksComplete` condition
carries the run's own message, the commit's deployment status reports a
failure, and the workloads panel says so in a banner above the list with the
button that runs it again beside it.

## From a terminal

```sh
kitchen processes                                  # what production runs besides the web process
kitchen processes --environment shop-pr-42         # a preview's, including what it will not run
kitchen processes runs nightly-report              # that job's recent runs
kitchen processes run nightly-report               # run it now
kitchen processes runs migrate                     # what the migration did on the last few deploys
kitchen processes run migrate                      # try it again, which resumes the deploy
kitchen logs --run shop-production-nightly-report-29387520 --follow

# where one workload's siblings reach it
kitchen processes --json | jq '.items[] | select(.address) | {name, address}'
```

Declaring one is `kitchen processes set`, which reads the list, changes the
workload it names, and sends the rest back as they came:

```sh
kitchen processes set worker --type worker --command node --command worker.js --replicas 2
kitchen processes set api --type service --port 8080 --build-root services/api
kitchen processes set cache --type service --port 6379 --image docker.io/library/redis:7.4
kitchen processes set nightly --type cron --schedule "0 3 * * *" --timeout 30m
kitchen processes set worker --replicas 0          # declared and parked
kitchen processes rm nightly --yes
```

Declaring *all* of them at once has no command, and deliberately: a list of
records with commands, schedules and resources in it has no flag-shaped
spelling worth having, so `kitchen api` carries that.

```sh
kitchen api PATCH /projects/shop --data @processes.json
```
