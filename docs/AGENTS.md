# Kitchen for agents

A reference for a coding agent working **inside an application repository that
deploys to Kitchen** — not for one working on Kitchen itself, which is
[CLAUDE.md](../CLAUDE.md).

It is written to be read whole. It is about 400 lines, which is cheaper than
the four documents you would otherwise open, and it says at each point which
of them has the rest.

Copy it into the repository you are working in, or fetch it:

```
https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/AGENTS.md
```

## What Kitchen is, in five sentences

Kitchen is a self-hosted platform that turns `git push` into a running URL. A
**project** is one repository; a **build** turns one commit into an image; a
**release** is that image frozen together with the configuration it was built
with; an **environment** is a release running somewhere with a URL. Production
is the environment the default branch deploys to; every pull request gets a
**preview** environment of its own, up to the ceiling the platform sets on how
many of them one project may have live at once — a request past it is told so
on the request and gets its preview on its next push once a slot is free.

The cluster underneath is not part of the interface. There is no `kubectl`
step in any workflow, no manifest to write and no namespace to name — if you
find yourself reaching for one, you are solving the wrong problem, and the
platform has a route and a screen for whatever it is.

## Are you in a Kitchen project?

Two files, either of which may be absent:

| File | What it means |
|---|---|
| `.kitchen/project.json` | The directory is linked to a project: `{"project": "shop", "api": "https://kitchen.apps.example.com"}`. Written by `kitchen link`, holds no credential, and is meant to be committed. Because it is committed, the `api` in it may only *choose* an installation this machine has signed in to — set `KITCHEN_API` where nothing has run `kitchen login`, which is every CI runner ([CLI.md](CLI.md#linking-a-directory)). |
| `kitchen.json` | The project declares its own build and runtime settings — see [Configuration](#configuration-kitchenjson). |

Neither being there does not mean the repository is not deployed by Kitchen:
a project can be created and configured entirely from the dashboard, and the
repository then holds nothing at all. `kitchen status` against a linked or
named project is the only reliable answer.

## The three machine-readable surfaces

Read these rather than guessing, and rather than trusting this document where
the two disagree — this one is written by hand and those are generated from
the code that runs.

### 1. `kitchen schema`

One JSON document describing the whole CLI: every command with its flags,
their types and defaults, the API endpoints each one calls, the shape of what
it answers with, the exit-code table, and a `protocol` block stating the
contract in prose. It always emits JSON, `--json` or not.

```sh
kitchen schema | jq '.commands[] | select(.path == "kitchen deploy")'
kitchen schema | jq -r '.exitCodes[] | "\(.code)\t\(.name)\t\(.meaning)"'
```

It is derived from the commands rather than written beside them, and a test
refuses a command that does not publish what it does, what it calls, what it
answers with and how to run it. So a command that exists is in there.

### 2. The JSON Schema for `kitchen.json`

```sh
kitchen config schema        # prints the URL
```

Published at
`https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json`.
Put it in the file's `$schema` key. A test in the platform fails when a key
exists in the schema and not in the parser, or the reverse.

### 3. `kitchen api`

```sh
kitchen api GET /projects/shop
kitchen api PATCH /projects/shop --data '{"replicas": 3}'
```

Any endpoint, authenticated, with the same failure shape as every other
command. It is why a route with no command of its own is never unreachable
from a terminal. [docs/API.md](API.md) is the route table and the
authorization model; `docs/api/<resource>.md` is the body of each one.

## The output contract

Every one of these is a property you can rely on, published in
`kitchen schema`'s `protocol` block:

- **`--json` puts JSON on stdout and nothing else.** `KITCHEN_JSON=1` is the
  same thing for a whole session. Use it always; parse stdout, never the
  human rendering.
- **A followed operation is NDJSON** — one object per line as things happen,
  each with a `type`. `kitchen deploy` and `kitchen logs` are the two.
- **A failure is one document**: `{"error": {"code", "message", "hint",
  "status", "doing"}}`, on stdout under `--json`, plus a non-zero exit.
- **Progress and warnings go to stderr**, as `{"type": "note"|"warning"}`
  objects under `--json`. Never mix them into what you parse.
- **Nothing blocks on a prompt.** Every question has a flag that answers it,
  and `--no-input` — implied whenever stdin is not a terminal, which is your
  case — turns a question into a failure naming the flag it wanted. You will
  never hang waiting for a `y/n`.

### Exit codes

Branch on these, not on message text. `kitchen schema` publishes the same
table.

| | | |
|---|---|---|
| `0` | ok | It did what it was asked |
| `1` | failed | A failure with no code of its own |
| `2` | usage | The command line is wrong |
| `3` | unauthenticated | No usable credential — `kitchen login` |
| `4` | forbidden | The account may not do this; the message names the role |
| `5` | notFound | No such object, **or one this account may not know exists** |
| `6` | conflict | Something changed first, or already exists, or is still in use |
| `7` | unavailable | The endpoint needs a capability this installation lacks |
| `8` | unreachable | DNS, TLS, connection refused, a timeout on the wire |
| `9` | buildFailed | A followed build ended Failed or Cancelled — the command worked, the build did not |
| `10` | notLinked | No project resolved: `--project`, `KITCHEN_PROJECT`, or `kitchen link` |
| `11` | timedOut | A wait ran out. Nothing was undone |
| `12` | deployFailed | A followed deploy ended Degraded — the build succeeded, the release did not take traffic, and what was serving before it still is |
| `130` | interrupted | SIGINT. What was already started keeps running |

`9` and `12` are the two worth special handling, and they are different
failures. `9` means your deploy reached the platform and the *application*
failed to build. `12` means it built and the platform refused to serve it —
almost always a deploy task, such as a schema migration, that failed before the
release could take traffic; the error's `message` names the task and its run.
Read the logs in both cases rather than retrying.

## Configuration: `kitchen.json`

A project's build and runtime settings, committed at its root directory and
read at every commit the platform builds. [docs/CONFIG.md](CONFIG.md) is the
whole of it; this is what an agent needs to hold.

```json
{
  "$schema": "https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json",
  "build": {"strategy": "auto"},
  "runtime": {
    "port": 3000,
    "health": {"path": "/healthz"}
  },
  "env": {"NODE_ENV": "production"},
  "processes": [
    {"name": "worker", "type": "worker", "command": ["node", "worker.js"]}
  ]
}
```

**Check it before you commit it.** This is the one command that reaches
nothing — no credential, no network — so it works anywhere:

```sh
kitchen config check --json
```

Exit `0` valid, `1` invalid with the reason, `5` no file. It runs the same
parser the platform runs, so a file it accepts is a file the build accepts.

### The rules that will bite you

- **The file wins over the dashboard for every setting it names**, at every
  build, and touches nothing it does not name. Adding a key takes that
  setting away from whoever was editing it in the UI. Removing a key gives it
  back.
- **Never put a credential in it.** Values are literal strings; a value
  pointing at a secret or a resource claim is refused by name. The file is
  committed, so anything in it is public. Credentials go through
  `kitchen env set` / `kitchen secret`, or the dashboard.
- **You cannot give a literal value to a variable the project binds to a
  secret or a provisioned database.** That fails the build rather than
  shadowing it. If `kitchen env list` shows a variable sourced from a secret
  or a claim, leave that name alone in the file.
- **`env` merges by name; `processes` replaces the whole list.** A process the
  file does not name is a process the project does not run.
- **It cannot set `build.rootDirectory`**, or anything about the project's
  standing: criticality, data class, access, promotion, preview protection.
  Those are the operator's and the project admin's, and a file arguing about
  them is refused with a sentence saying so.
- **An unrecognised key fails the build.** There is no forgiving mode. Run
  `kitchen config check` and you will never meet this.

## Making an application deployable

The platform reads the repository and decides how to build it. What you can
do to make that go well, in order of how often it is the problem:

1. **Listen on `$PORT`.** The platform sets it in every environment, ahead of
   the project's own variables, and a buildpacks-built image has no other way
   to be told. An application hard-coded to 3000 works only if the port
   happens to agree.
2. **Have a `Dockerfile`, or be something detection recognises.** A Dockerfile
   wins over everything. Otherwise the platform matches `package.json`
   dependencies (`next`, `nuxt`, `@sveltejs/kit`, `@remix-run/*`,
   `@nestjs/core`, `astro`, `react-scripts`, `vite`), then `go.mod`,
   `requirements.txt` / `pyproject.toml` / `Pipfile`, `Gemfile`, `pom.xml` /
   `build.gradle`, a `.csproj`, and last a bare `index.html`. A repository
   nothing matches **fails the build** saying so.
3. **Give it a health path** and declare it in `runtime.health.path`. Without
   one the platform can only make a TCP connect, which says the process
   started and not that it works. Return 2xx when the application is genuinely
   ready. **Put a schema migration in a `task` process**, not in the
   entrypoint: a task runs once per deploy and the release takes no traffic
   until it succeeds, where an entrypoint migration runs once per replica, at
   once, on every rollout. Write it forward-only and idempotent — nothing runs
   a "down" step, on a rollback or otherwise.
4. **Read configuration from the environment**, never from a file baked into
   the image. A release is one image deployed to production and to every
   preview, and the environment is the only thing that differs.
5. **Do not write to the container filesystem** and expect it to be there
   later. Persistence is a provisioned resource, not a disk.
6. **`KITCHEN_URL`** is set in every environment that has one: where *this*
   environment is published. A preview's hostname carries a pull request
   number nothing in the repository has heard of, so this is the only way an
   application can know its own address.

## The commands you will actually use

All of them take `--json`, and `--project NAME` where a project is needed.

```sh
kitchen status --json                       # what is running, and where
kitchen deploy --json                       # build and deploy the current commit
kitchen deploy --sha abc123 --json          # a particular commit
kitchen logs --json                         # what the application printed
kitchen logs --follow --json                # NDJSON, one line per line
kitchen env list --json                     # every variable, no values (see below)
kitchen env set NODE_ENV=production --json
kitchen rollback --json                     # to the previous release
kitchen builds --json                       # recent builds, newest first
kitchen config check --json
```

`kitchen deploy` streams NDJSON, exits `9` if the build fails and `12` if the
environment refused the release it produced; add `--detach` to return as soon
as it is queued.

### Values are never readable

`kitchen env list` prints every variable's name and whether it has a value,
**never the value**. No route on the platform answers one, so there is no
flag, no `env pull`, and no way to reconstruct a `.env` file. This is not an
obstacle to work around — do not try to read a value out of a running
container either.

The consequence for editing: `kitchen env set` sends the whole list back by
name, with a value only for the variables it is changing. That is what makes a
partial change possible against a route that replaces the list. Use the
command; do not hand-build the `PATCH`.

## Reading a failure

```sh
kitchen builds --json | jq '.items[0]'
```

A failed build answers `failure` with the container that stopped it, its exit
code, and the last of what it printed — the Job's own message is the same
sentence for every build that ever failed, so use this. `phase` is `Queued`,
`Running`, `Succeeded`, `Failed` or `Cancelled`; a build sitting in `Queued`
has a reason on its Ready condition, and the common ones are:

| Reason | What it is |
|---|---|
| `SourceUnreadable` | The platform cannot read the repository *right now*. It waits. Not yours to fix. |
| `RepositoryUnreadable` | The repository is not there, or the connection's credential cannot see it. Fails. |
| `FrameworkNotDetected` | No Dockerfile and nothing recognised. Fails. See [above](#making-an-application-deployable). |
| `ConfigInvalid` | The commit's `kitchen.json` is wrong, or conflicts with the project. The message says which. |
| `SourceUnreviewed` | The project requires a reviewed pull request for production-branch commits, and this one has none. |

## What you must not do

- **Do not use `kubectl`, and do not write Kubernetes manifests.** Whatever
  you are reaching for has a route and a screen. If it genuinely does not,
  that is a gap in the platform worth reporting, not a gap to route around.
- **Do not put a credential in `kitchen.json`, in the repository, or in a log
  line.** The platform refuses the first; nothing refuses the other two.
- **Do not retry a `forbidden` (4) with a different credential you found.**
  The message names the role the operation wanted; the answer is to ask
  whoever administers the project.
- **Do not treat `notFound` (5) as proof something does not exist.** The API
  answers "no such object" and "you may not know this exists" identically, on
  purpose.
- **Do not create a project, a connection or a domain to work around a
  failure.** Creating a connection is the operator's; creating a project needs
  a person's credential rather than a CI key; and a duplicate of either is a
  mess somebody has to clean up.
- **Do not disable a health check, lower a replica count, or turn off preview
  protection to make something green.** Each of those is a decision somebody
  made.

## Where the rest is

| | |
|---|---|
| [DEPLOYING.md](DEPLOYING.md) | The same ground for a person, end to end: an account, a project, a first deploy, variables, a domain, previews, logs, rollback. |
| [CONFIG.md](CONFIG.md) | `kitchen.json`: every key, the merge rules, and what a repository is not allowed to decide. |
| [CLI.md](CLI.md) | Every command in full, and the reasoning behind the machine-first contract above. |
| [API.md](API.md) | Authentication, the authorization model, the route table, the status codes. |
| `docs/api/<resource>.md` | One page per resource, with a runnable `curl` for each operation. |
| [CRDS.md](CRDS.md) | The operator's data model. You do not need it to deploy an application; it is where a "why did the platform do that" question ends up. |
| [../CLAUDE.md](../CLAUDE.md) | For an agent working on Kitchen itself, which is a different job. |
