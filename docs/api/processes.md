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
| `type` | all | `worker`, `service` or `cron`. |
| `command`, `args` | all | Exec form — a list of words, never a shell line. Absent runs the image's own entrypoint, which is what a workload with its own image wants and never what a buildpacks-built one does. |
| `port` | service | The port it listens on, and the port its siblings reach it on — the same number, deliberately. **Required on a service and refused on anything else**: only a service is addressed, and a port on a worker would read back as a setting that took while nothing ever connected to it. |
| `build` | worker, service | This workload's own build — see [Several workloads, one commit](#several-workloads-one-commit). Absent means it runs the project's image with another command. Refused on a cron process. |
| `replicas` | worker, service | How many copies. `0` is allowed: a workload declared and parked, which is how one is turned off without losing its command. |
| `singleton` | worker, service | Two of this workload must never run at once — see below. Refuses `replicas` above 1, and refused on a cron process. |
| `cpu`, `memory` | all | Kubernetes quantities, applied as request and limit alike — the same two strings the web process takes. |
| `schedule` | cron | A five-field cron expression, **read in UTC**. Required on a cron process and refused on a worker or a service. |
| `concurrencyPolicy` | cron | `Allow`, `Forbid` (the default) or `Replace`. A job that takes longer than its interval is far more often running behind than meant to run twice. |
| `timeout` | cron | A Go duration bounding one run; an hour by default. It becomes the Job's `activeDeadlineSeconds`. |
| `health` | worker, service | A health check, the same shape the project's web process takes. A worker's **must name the `port`** it is made against, because a worker publishes none of its own; a service's falls back to its own port. Refused on a cron process: how a run went is its exit status, not a probe. |
| `previews` | all | Whether it runs in preview environments. **Off for a worker and a scheduled job unless asked for; on for a service unless it says otherwise** — see below. |

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

### Several workloads, one commit

A workload with a `build` is built from its own directory of the repository,
which is what makes a monorepo one project instead of four:

```json
{"name": "api", "type": "service", "port": 8080,
 "build": {"strategy": "dockerfile", "dockerfilePath": "Dockerfile",
           "dockerfileTarget": "api", "rootDirectory": "services/api"}}
```

| Field | What it means |
| --- | --- |
| `strategy` | `dockerfile` (the default) or `buildpacks`. There is **no `auto`**: detection's output is a framework, and what the platform does with a framework is fill in the web process's port and tell the buildpacks lifecycle what it is building — a workload has neither question open, since a service names its own port and a workload asking for buildpacks has said which builder to use. |
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
    }
  ]
}
```

`healthy` is the platform's own verdict — a worker with no ready replica, a
schedule whose most recent run failed — rather than something each client
derives, so the dashboard and the CLI cannot disagree about what a red dot
means.

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

## A scheduled job's runs

```http
GET /api/v1/environments/{name}/processes/{process}/runs
```

**Needs `viewer`.** The runs the cluster still holds, newest first. Each one is
the Job that was the run:

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
ones and collects the rest, so this list is short by design. **A run's output
is not**: the log store keys on the Job's name, so a collected run is still
readable.

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

The body is empty, and that is load-bearing: the Job is a copy of the CronJob's
own template, so a manual run is the schedule firing early rather than a
different thing that resembles it. Nothing from the request reaches the pod.

Answers `202` with the run:

```json
{ "name": "shop-production-nightly-report-manual-x7k2p", "phase": "Running", "startedAt": "2026-08-24T14:22:10Z" }
```

The run's concurrency policy still applies: a job set to `Forbid` that is
already running gets a second run the scheduler drops.

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

## From a terminal

```sh
kitchen processes                                  # what production runs besides the web process
kitchen processes --environment shop-pr-42         # a preview's, including what it will not run
kitchen processes runs nightly-report              # that job's recent runs
kitchen processes run nightly-report               # run it now
kitchen logs --run shop-production-nightly-report-29387520 --follow

# where one workload's siblings reach it
kitchen processes --json | jq '.items[] | select(.address) | {name, address}'
```

Declaring the list has no command of its own: it is a list of records with
commands, schedules and resources in it, and a flag-shaped spelling of that
would be worse than the JSON body. `kitchen api` carries it:

```sh
kitchen api PATCH /projects/shop --data @processes.json
```
