# Workers and scheduled jobs

A Project deploys a web process: an image behind an HTTPRoute, on a URL. Real
applications also have a queue worker and a nightly job, and those are the same
image run differently — which is why they are a modelling question rather than
a build one. A Release is already an immutable image plus a config snapshot, so
a worker and a scheduled job add a list to that snapshot and nothing else.

- A **worker** runs continuously and is never addressed: a Deployment with no
  Service and no route. Nothing publishes it, so nothing needs a certificate
  for it.
- A **scheduled job** runs on a cron expression: a `batch/v1` CronJob, and one
  firing is a *run*.

There is no `web` in the list, and it is not an omission. The web process is
the project's own `runtime` — its port, replicas and resources — and it is
singular because the URL is: an environment publishes one hostname, one Service
and one route, and a second process claiming to be the web one would have to be
told which of those it got.

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
| `name` | both | A DNS label, unique within the project. It names the workload (`<environment>-<name>`) and is the log store's `process:`. `web` is refused. |
| `type` | both | `worker` or `cron`. |
| `command`, `args` | both | Exec form — a list of words, never a shell line. Absent runs the image's own entrypoint, which is what a worker with its own image wants and never what a buildpacks-built one does. |
| `replicas` | worker | How many copies. `0` is allowed: a worker declared and parked, which is how one is turned off without losing its command. |
| `cpu`, `memory` | both | Kubernetes quantities, applied as request and limit alike — the same two strings the web process takes. |
| `schedule` | cron | A five-field cron expression, **read in UTC**. Required on a cron process and refused on a worker. |
| `concurrencyPolicy` | cron | `Allow`, `Forbid` (the default) or `Replace`. A job that takes longer than its interval is far more often running behind than meant to run twice. |
| `timeout` | cron | A Go duration bounding one run; an hour by default. It becomes the Job's `activeDeadlineSeconds`. |
| `health` | worker | A health check, the same shape the project's web process takes — and it **must name the `port`** it is made against, because a worker publishes none of its own. Refused on a cron process: how a run went is its exit status, not a probe. |
| `previews` | both | Whether it runs in preview environments. **Off unless asked for** — see below. |

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

### Previews are off by default

`previews` is `false` unless a process says otherwise, and that is the decision
rather than an omission. A preview that emails customers nightly is a bad
afternoon; a preview worker draining the production queue is a worse one. A
preview shares the project's environment variables, so unless somebody set a
`previewValue`, it is pointed at whatever the production process is pointed at.

One rule for both types, opted into per process. A preview that should run its
worker says so.

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

Asking a worker for its runs is refused with `400`, not `404`: it has none
because it is already running, and a not-found would suggest a process that
plainly exists does not.

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
```

Declaring the list has no command of its own: it is a list of records with
commands, schedules and resources in it, and a flag-shaped spelling of that
would be worse than the JSON body. `kitchen api` carries it:

```sh
kitchen api PATCH /projects/shop --data @processes.json
```
