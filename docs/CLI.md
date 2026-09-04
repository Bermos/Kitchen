# Kitchen — the `kitchen` CLI

The command line client for a Kitchen installation: link a working directory to
a project, deploy the current commit, follow what the platform does with it,
read the logs, change the environment variables, roll back.

It is a **client of the [REST API](API.md)** and nothing else. It holds no
kubeconfig, talks to no cluster, and knows nothing about the platform that the
API did not just tell it — everything it can do, the dashboard can do, because
both are clients of the same routes. It ships from this repository so that it
is versioned with the platform it drives, the way the chart and both images
already are.

```sh
kitchen link --project shop
kitchen deploy
kitchen logs --follow
kitchen rollback
```

## Built to be driven

The CLI is meant to be run by a person **and** by something that is not a
person, and the second is the harder requirement, so it comes first. Five
things follow from it, and they are the contract:

- **`kitchen schema` is the whole surface, as one JSON document.** Every
  command, every flag with its type and default, which API endpoints each one
  calls, what shape it answers with, the exit codes, the environment variables
  and the files it keeps. A caller that has never run `kitchen` can read that
  and drive all of it, without scraping `--help` and without guessing.
- **`--json` on every command**, with a shape that does not depend on whether a
  terminal was attached. stdout carries the answer and nothing else; progress
  and warnings go to stderr.
- **Nothing ever blocks on a prompt.** Every question has a flag that answers
  it, and `--no-input` — implied whenever stdin is not a terminal — turns a
  question into a failure that names that flag. The Bubble Tea views are only
  ever drawn when stdout is a terminal *and* `--json` is off, so they cannot
  appear in a pipe.
- **Exit codes are a contract**: one per kind of failure, published in the
  schema, never reused. A script can branch on `$?` without parsing anything.
- **`kitchen api` reaches whatever has no command yet**, so a route added to the
  API is usable from the command line the day it lands.

```sh
kitchen schema | jq -r '.commands[] | "\(.path)\t\(.summary)"'
kitchen schema deploy | jq '.commands[0].flags'
kitchen schema | jq '.exitCodes'
```

The schema is **derived, not written**: the commands are walked, the flags are
read off the parser, and the output shapes are read off the Go structs the CLI
decodes into. A test walks the tree and fails if a command carries no
description, no examples or no output mode — and another checks every endpoint
a command claims to call against the API's own enforcement table
(`internal/api/policy.go`). The published surface cannot fall behind the real
one.

## Installing

**There is no binary release to download.** The chart and both images are
published under every tag; the CLI is not built or uploaded by any workflow —
it is a [fourth artifact job nobody has written yet](#open) — so `go install`,
which builds it from the tag you name, is the whole of it. That needs a Go
toolchain of at least the version in [`go.mod`](../go.mod), and nothing else:

```sh
go install github.com/Bermos/Kitchen/cmd/kitchen@latest    # the newest release
go install github.com/Bermos/Kitchen/cmd/kitchen@v0.15.0   # a particular one
```

That writes `kitchen` into `go env GOBIN`, or `$(go env GOPATH)/bin` when GOBIN
is unset — which has to be on your `PATH`:

```sh
export PATH="$PATH:$(go env GOPATH)/bin"
```

**Pin the tag to the installation you talk to.** `@latest` is the newest tag in
this repository, which is not necessarily the release your cluster is running:
one tag versions the chart, both images and this, so `kitchen version` and the
version in the dashboard's sidebar should read the same. The CLI is a REST
client and the API is versioned, so a mismatch is usually harmless — but a
command for a route your installation does not serve yet fails at the route,
which is a confusing way to learn it.

From a checkout, the Makefile builds the same binary:

```sh
make cli            # builds bin/kitchen
make cli-install    # installs it into GOBIN
```

Either way, `kitchen version` says what you got:

```sh
kitchen version                          # kitchen 0.15.0 (go1.26.6, linux/amd64)
kitchen version --json | jq -r .version  # 0.15.0
```

The Makefile stamps that number with the linker, from the same `LDFLAGS` the
images use; `go install` passes no linker flags, so the binary reports the
module version the toolchain recorded in it instead. A `go build ./cmd/kitchen`
in a working directory has neither, and says `dev`.

## Signing in

**The CLI authenticates with an API key**, exchanged at the platform's identity
provider for the short-lived token the API actually sees — the flow
[API.md](API.md#getting-a-token) documents for CI, used here for a laptop as
well. Issue one from a project's People tab in the dashboard, or with `POST
/projects/{name}/keys`.

```sh
kitchen login --api https://kitchen.example.com --api-key-stdin < key
kitchen whoami
```

The key never reaches the operator: it is exchanged at the issuer, and only the
JWT travels to the API. So a leaked key is revoked in one place — delete it at
the issuer, and the operator has nothing to invalidate.

What a key may do is a project role on a machine account, which is the
narrowest credential the platform can issue: a key made for `shop` can deploy
`shop`, cannot see that any other project exists, and cannot create one —
`kitchen projects create` is refused for a key, because a project's creator
becomes its admin and an admin issues keys. The preflight that command runs
first (`POST /connections/{name}/detect`) is refused for a key as well, along
with the repository listing beside it: both read through the *platform's* git
credential, and both exist to fill in a form a key may not submit. The
preflight is advice, so what a key sees is a warning about it and then the
refusal of the create itself. That is the right shape for a CLI, which is
exactly where a too-broad token ends up on a laptop.

In CI, skip `login` entirely:

| Variable | What it is |
|---|---|
| `KITCHEN_API_KEY` | A key to exchange for a token, per command |
| `KITCHEN_TOKEN` | A token somebody already exchanged, used as it is |

### Why there is no browser sign-in

A CLI signing a *person* in wants either the OAuth device authorization grant
or a loopback redirect with PKCE. Neither is available today, and both are
decisions for `auth/` rather than things the CLI can assume:

- **The device grant does not exist at the issuer.** Kitchen's identity provider
  is better-auth with `@better-auth/oauth-provider`, and that plugin implements
  no device authorization endpoint at all — there is nothing for the CLI to
  poll, and nothing to advertise in the discovery document.
- **A loopback redirect needs a client seeded for it.** The provider refuses a
  client whose redirect URIs are on more than one host, and a port is part of a
  host — so `http://127.0.0.1:<port>/callback` could only be registered for one
  fixed port, which may be in use. A public `kitchen-cli` client would have to
  be seeded the way `kitchen-ui` is (`auth/src/seed.ts`), with that decision
  made deliberately — and it would have to join the platform's client list on
  both sides, because a token naming a client the API does not know as the
  platform's is refused (AUTH.md, "The operator API").

So the API key is the whole of it, and that is a smaller credential than a
person's token would be. If the issuer grows a device endpoint, `kitchen login`
gains a browser flow and the key path stays for CI.

### The platform commands are the dashboard's for now

A key is a role on one project, and four command families call nothing but the
platform's own surface, which the API answers to the operator role alone:

| Command | What it calls |
|---|---|
| `kitchen backup` (and `list`, `run`) | `POST /platform/backup`, `GET`/`POST /platform/backup/runs` |
| `kitchen retention` | `GET /platform/retention` |
| `kitchen access identities/reviews/show` | `GET /access/identities`, `GET /access/reviews`, `GET /access/reviews/{name}` |
| `kitchen audit-pack` | `GET /projects/{name}/audit-pack` |

**There is no credential `kitchen login` can store that runs any of them.**
`kitchen api` is not the way round it either: it carries the same token, so a
platform route is no more reachable through it than through the command. The
escape hatch works for routes; it does not work for roles.

So they are **declared the dashboard's for now**
([#208](https://github.com/Bermos/Kitchen/issues/208)), rather than shipped as
commands that are published, documented and silently unable to work. Three
things say it, and all three come from one place in the code
(`internal/cli/dashboard_only.go`), so they cannot come to say three different
things:

- each one's `--help` carries the statement and names the screen;
- `kitchen schema` publishes it per command as `needs.platform` — the screen,
  its path, and why the credential in hand cannot;
- running one with a project key exits `4` like any other permission failure,
  and the refusal carries the API's own sentence *plus* the screen that can do
  it.

```console
$ kitchen retention
Error: reading the platform's retention needs the operator role; you are a member
  a project API key holds a role on one project, and this command calls the
  platform's own surface, which needs the operator role — no credential
  `kitchen login` can store holds one. Do it in the dashboard, under
  Platform → Settings, under Retention (/platform/settings); see docs/CLI.md
```

```sh
kitchen schema backup | jq '.commands[0].needs.platform'
```

Which commands these are is not a list anybody keeps: a command declares itself,
and a test checks the declaration against the API's own route table
(`internal/api/policy.go`) in both directions. A route that stops being the
operator's fails the CLI's tests rather than leaving a command pointing at a
dashboard nobody needs.

**Everything else works with a key**, including four that look like they belong
in the table above and do not. `kitchen drift`, `kitchen decisions`,
`kitchen criticality` and `kitchen exceptions` are cross-project reads rather
than platform ones: they answer for the projects the caller can see, which for
a key is the one project it holds a role on. An operator gets the whole
installation from the same call.

**A platform-scoped key is the real answer**, and it is
[#349](https://github.com/Bermos/Kitchen/issues/349) — designed together with
[#318](https://github.com/Bermos/Kitchen/issues/318), because a key today is a
full identity-provider session and both changes decide what a key *is*. Doing
them one on top of the other would mean designing the same thing twice. The
fresh-install bootstrap loop — a key comes from a route that needs a key — is
part of that issue as well.

## Linking a directory

```sh
kitchen link --project shop
```

writes `.kitchen/project.json` at the root of the working copy:

```json
{"project": "shop", "api": "https://kitchen.example.com"}
```

It holds no credential — the project's name and which installation it is on —
so committing it is a reasonable thing to want: everybody working on the
repository deploys the same project. Every command finds it by walking up from
the current directory, so they work from a subdirectory too.

Which project a command is about is resolved in this order, and every failure
that cannot resolve one says all three:

1. `--project`
2. `KITCHEN_PROJECT`
3. the link file

Which installation, likewise: `--api`, `KITCHEN_API`, the link file, and then
the machine's current installation from `kitchen login`.

Run without `--project` on a terminal, `kitchen link` offers the projects this
account can see and lets you pick one. Without a terminal it names the flag and
lists the choices in the failure, rather than waiting for an answer nobody can
give.

## The commands

| Command | Does | Calls |
|---|---|---|
| `kitchen login` | Store a credential for an installation and check it works | `GET /config.json`, the issuer's `/token`, `GET /me` |
| `kitchen logout` | Forget a stored credential. Does not revoke it | — |
| `kitchen whoami` | Who the credential is, and its platform role | `GET /me` |
| `kitchen link` | Associate this directory with a project | `GET /projects`, `GET /projects/{name}` |
| `kitchen projects create` | Create a project from this repository, checking its layout first | `GET /connections`, `POST /connections/{name}/detect`, `POST /projects` |
| `kitchen status` | The project: environments, phases, URLs, recent builds | three reads, joined |
| `kitchen deploy` | Build this commit and follow the deploy | `POST /projects/{name}/builds` and the follow |
| `kitchen cancel` | Stop a build that is still running | `POST /builds/{name}/cancel` |
| `kitchen logs` | An environment's or a build's logs, `--follow` to tail | `GET /environments/{name}/logs`, `GET /builds/{name}/logs` |
| `kitchen processes` | The workloads an environment runs besides its web process, and (`runs`, `run`) one workload's run history and running it now — a scheduled job off its schedule, or a deploy task again | `GET /environments/{name}/processes`, `GET`/`POST /environments/{name}/processes/{process}/runs` |
| `kitchen env list/set/rm` | The project's environment variables | `PATCH /projects/{name}/env` |
| `kitchen secret list/set/rm` | The project's own secrets — credentials the platform did not mint | `GET /projects/{name}/secrets`, `PUT`/`DELETE /projects/{name}/secrets/{secret}` |
| `kitchen rollback` | Put an environment back on an earlier release, saying what that changes first | `GET /releases/{name}/config-diff`, `PATCH /environments/{name}` |
| `kitchen promote` | Ask for a release to land on an environment; the policy decides | `POST /projects/{name}/promotions` |
| `kitchen promotions` | What promotions were asked for and what became of them | `GET /projects/{name}/promotions`, `GET /promotions/{name}` |
| `kitchen projects` | The projects this account can see, with its role on each | `GET /projects` |
| `kitchen builds` | The project's builds, newest first, why the failed ones failed and why a running one is not moving | `GET /projects/{name}/builds` |
| `kitchen attestations` | The signed evidence attached to one image a build produced (`--workload`) | `GET /builds/{name}/attestations` |
| `kitchen gates list/submit` | What ran over an artifact, and submitting a result from elsewhere | `GET /builds/{name}`, `POST /builds/{name}/gates` |
| `kitchen vex list/submit` | What has been asserted about an artifact's findings applying here, and asserting it | `GET /builds/{name}/vex`, `POST /builds/{name}/vex` |
| `kitchen decisions list/show/replay` | The stored policy decisions, and re-running one from its stored inputs | `GET /decisions`, `GET /decisions/{id}`, `POST /decisions/{id}/replay` |
| `kitchen drift` | What is deployed right now that no longer meets its environment's bar | `GET /compliance/drift` |
| `kitchen criticality` | What supports each designated function, and (`dependents`) what breaks without one third party | `GET /compliance/criticality`, `GET /compliance/dependents` |
| `kitchen access identities` † | Who holds what on the platform, and which grants look like they belong to nobody | `GET /access/identities` |
| `kitchen access reviews/show` † | The recertification cycles and what each one decided | `GET /access/reviews`, `GET /access/reviews/{name}` |
| `kitchen retention` † | How long the platform keeps each class, and how far back each one goes | `GET /platform/retention` |
| `kitchen audit-pack` † | Export one project's whole compliance answer for a window, signed, as files on disk | `GET /projects/{name}/audit-pack` |
| `kitchen releases` | The project's releases — what there is to roll back to | `GET /projects/{name}/releases` |
| `kitchen environments` | The project's environments and where they answer | `GET /projects/{name}/environments` |
| `kitchen api` | Any endpoint of the API, authenticated | anything |
| `kitchen schema` | The whole CLI as JSON | — |
| `kitchen version` | Which release of the CLI this is | — |
| `kitchen backup` † | Take a backup of the platform and write it to a file | `POST /platform/backup` |
| `kitchen backup list` † | What the platform's backup destination holds right now | `GET /platform/backup/runs` |
| `kitchen backup run` † | Take one to the destination now, rather than to this machine | `POST /platform/backup/runs` |

† The dashboard's for now: no credential `kitchen login` can store runs it — see
[The platform commands are the dashboard's for now](#the-platform-commands-are-the-dashboards-for-now).

### Creating a project

```sh
kitchen projects create
```

is the whole of it in a checkout: the repository comes from `origin`, the name
from the repository, and the two connections from the platform when it offers
only one that can do each job. It ends by writing the same
`.kitchen/project.json` `kitchen link` writes, so the directory is linked to the
project it just made. It replaces an existing link on the same terms `kitchen
link` does: a directory already deploying another project is asked about first
(`--yes` answers that question too, and with no terminal it is a failure naming
the flag rather than a silent rewrite), the answer's `replaced` says which
project was taken over, and `--link=false` writes no link at all.

Before the project is written, the repository is read through the same
preflight the dashboard's new-project dialog runs — `POST
/connections/{name}/detect` — and what it found is printed and carried in the
answer:

```json
{"project": {"name": "shop", "repo": "acme/shop", "productionBranch": "main", "role": "admin"},
 "detection": {"detected": true, "framework": "next", "strategy": "buildpacks", "port": 3000},
 "path": "/home/anna/shop/.kitchen/project.json"}
```

That is the point of the command rather than a `kitchen api POST /projects`.
**Creating a project starts a build of its production branch immediately**, so
`--root-directory`, `--dockerfile` and `--dockerfile-target` are sent with the
project rather than patched onto it: a monorepo whose application is in
`apps/shop` would otherwise fail one build before anybody realised what to
correct — and a multi-stage Dockerfile whose last stage is not the runtime would
not fail at all, it would ship the wrong image and report success.

A repository the platform recognised nothing in — and that has no Dockerfile
either — is a question rather than a refusal, since the detector reads one
commit and the person is looking at the whole repository. A repository it could
not read at all comes back as `"unreadable": true` and is the same question in
different words: it is not there, or the connection's credential cannot see it,
and the sentence the preflight gives names which connection was asked rather
than describing a directory nothing ever got as far as. Like every other
question here it has a flag that answers it, so `--yes` creates the project and
nothing ever waits:

```sh
kitchen projects create shop --repo acme/mono --root-directory apps/shop \
  --connection github --registry kitchen --yes --json
```

The preflight also settles which branch production deploys from: with no
`--production-branch`, the repository is read at its own default branch and the
project is created on that one — which is why the command works the same on a
repository whose trunk is called `master` or `trunk` as on one called `main`.

`--dockerfile-target` is which stage of a multi-stage Dockerfile to ship;
leaving it off ships the file's last stage. The preflight answers the stages the
file declares, so naming one it has none of is the third question the command
asks — `--yes` answers that one too, because the person may be naming a stage
the change they are about to push adds.

The preflight is advice, so a platform that cannot reach the provider to give
any is reported on stderr and the project is still created. The flags are
`--repo`, `--connection`, `--registry`, `--production-branch`, `--previews`,
`--root-directory`, `--dockerfile`, `--dockerfile-target`, `--link` (on by
default) and `--yes`; leaving `--previews` off leaves the platform's default
alone rather than turning previews off.

There is no repository picker. The command takes `owner/name` from the checkout
or from `--repo`, which is why `GET /connections/{name}/repositories` has no
command of its own.

### Deploying

```sh
kitchen deploy
```

builds the commit the working copy is on. The platform's build clones from the
git provider, so the commit has to have been pushed; a dirty tree and an
unpushed commit are both warnings on stderr rather than refusals, because the
platform's answer is the authoritative one.

`--sha` and `--branch` say it explicitly, which is how this runs somewhere that
is not a checkout at all — a CI job with only the metadata, or an agent working
from a description of the change. `--rebuild` builds whatever the project built
last, for a rerun after a flake or a changed secret.

Following is a renderer for something the API already does: build logs stream
over Server-Sent Events, phases are a poll of the build, and the release and the
environment are two more objects that appear when they appear. On a terminal it
is drawn under a spinner with the build's output scrolling above it; in a pipe
it is NDJSON:

```sh
kitchen deploy --json --timeout 30m
{"type":"build","build":{"name":"shop-bld-abc123def456-xk2p9","phase":"Queued",…}}
{"type":"log","line":{"timestamp":"…","message":"#4 [1/6] FROM docker.io/library/node…"}}
{"type":"build","build":{"…":"…","phase":"Succeeded"}}
{"type":"release","release":{"name":"shop-rel-42",…}}
{"type":"environment","environment":{"name":"shop-production","phase":"Live",…}}
{"type":"result","ok":true,"url":"https://shop.example.com","build":{…},"environment":{…}}
```

**The exit status is the deploy's**: `0` when it worked, `9` when the build
failed or was cancelled, and `12` when the build succeeded and the environment
settled `Degraded` — the release was refused, and what was serving before it
still is. A failed deploy task is the case that makes the difference: the
platform is working exactly as designed when it stops the deploy where it
stands, and a pipeline that read `0` there was being told a half-applied
migration had shipped.

```sh
kitchen deploy --json
{"type":"environment","environment":{"name":"shop-production","phase":"Degraded",…}}
{"type":"result","ok":false,"build":{…},"environment":{…}}
{"error":{"code":"deployFailed","message":"shop-production ended degraded: migrate failed before this release could take traffic … Run shop-production-migrate-1: exit status 1","hint":"…"}}
```

The reason is the environment's own condition, so it names the task, its run and
what the run said; on a terminal it is the same sentence on stderr. `ok` on the
result event is the whole deploy's verdict — the same fact the exit status
carries — rather than the build's alone.

**`Degraded` on its own is not the verdict.** An environment carries the phase
its last reconcile left on it, so a retry of a failed deploy reads `Degraded`
for the moment between the release being promoted onto it and the platform
looking at the new one; and a `Degraded` whose conditions say a deploy task is
still running has not finished. Both are waited on. It is a settled `Degraded` —
one that holds, with nothing on the environment saying work is in flight — that
exits `12`.

Everything else about the environment stays a fact in the result rather than in
the status. A build of a branch nothing promotes produces a release no
environment picks up, and an environment that has not gone live inside
`--environment-timeout` has not gone live yet; both exit `0`, and the result
carries the environment's phase and URL either way.

### Environment variables

Two things about the API shape this command, and both are deliberate (see
[the API reference](api/projects.md#changing-a-projects-environment-variables)):

- **A value goes in and never comes back out.** Reading a project reports
  whether a variable has one, not what it is. So `kitchen env list` prints the
  whole list and reveals nothing, and there is no `env pull` — there is nothing
  to pull.
- **The write replaces the whole list**, and a variable whose `value` the
  request leaves out keeps the one it already has. That is what makes a
  one-variable change possible without reading any values: the CLI sends every
  variable back by name and a value only for the ones it is changing.

```sh
kitchen env set LOG_LEVEL=debug DATABASE_POOL=10
kitchen env set API_URL=https://api.example.com --preview API_URL=https://api.invalid
kitchen env set --from-secret API_KEY=kitchen-project-secrets:shop-api-key
kitchen env set --from-claim DATABASE_URL=shop-db:url
kitchen env set --from-file .env
kitchen env rm LOG_LEVEL --yes
```

Variables land in the next release's snapshot: what is already running keeps the
configuration it was released with until the next deploy.

### The project's own secrets

A credential the platform did not mint — a database the project runs itself, an
API key, an SMTP password — goes in a secret rather than in a variable's value.
The variable then holds a reference, so the credential is never part of the
project's configuration (see
[the API reference](api/secrets.md)).

```sh
kitchen secret list
kitchen secret set SMTP_PASSWORD                        # prompts, without echoing
kitchen secret set SMTP_PASSWORD --value-file ./smtp    # or from a file
pass show smtp | kitchen secret set SMTP_PASSWORD --value-stdin
kitchen secret rm SMTP_PASSWORD --yes
```

`set` both sets a new secret and rotates an existing one: it is the same write,
and the only way to find out which it would be first is to read a value, which
nothing can. The value comes from `--value-file`, `--value-stdin`, `--value`, or
a prompt that does not echo — and with no terminal to prompt at, the failure
names those flags rather than waiting.

Nothing here reads a value back, so `kitchen secret list` prints the whole list
and reveals nothing. What it does print beside each name is the reference, so
the line that follows never has to be typed from memory:

```sh
kitchen env set --from-secret SMTP_PASSWORD=kitchen-project-secrets:SMTP_PASSWORD
```

Unlike a variable, a rotated secret reaches what is already running: the
platform restarts whatever reads it.

### Logs

```sh
kitchen logs                                  # the production environment's last 200 lines
kitchen logs --follow                         # and everything after them
kitchen logs --since 1h --search error --json
kitchen logs --build shop-bld-abc123def456-xk2p9
kitchen logs --environment shop-pr-42 --follow --timeout 10m
kitchen logs --process worker --since 15m       # one of the project's workers
kitchen logs --run shop-production-nightly-report-29387520
```

`--since` and `--until` take an RFC 3339 timestamp or a duration (`15m`, `2h`)
read as "that long ago". A bounded page and a followed tail answer the same
shape — one line per log line — so `--follow` changes how long the command runs
and nothing else.

`--process` narrows to one of the project's workers or scheduled jobs, and
`--run` to one firing of a schedule. A run's output outlives the run itself:
the platform keeps a handful of finished Jobs and collects the rest, but the
lines stay for the whole container-log retention, so last month's failed report
is still readable by name.

A build of a commit that produces several images is several Jobs, and their
output is one tail: each line names the workload that wrote it, and `--json`
carries the Job it came from as `run` — which `--run` also narrows to, when
one workload's build is the only one worth reading.

### Workloads

A project deploys a web process, and it may also declare other workloads:
**workers**, which run continuously and are never addressed; **services**,
which run continuously and are reachable from the rest of the unit and from
nowhere outside the cluster; **scheduled jobs**, which run on a cron expression
in UTC; and **tasks**, which run once per deploy and have to finish before any
of that release takes traffic.

```sh
kitchen processes                             # what production runs besides the web process
kitchen processes --environment shop-pr-42    # a preview's, including what it will not run
kitchen processes runs nightly-report         # that job's recent runs, newest first
kitchen processes run nightly-report          # run it now, off the schedule
kitchen processes runs migrate                # what the migration did on the last few deploys
kitchen processes run migrate                 # try it again, which resumes the deploy
```

What is listed is the *release's* workload list, so an environment that has
been rolled back lists the workloads that release declared — and, for one built
from its own directory, the image that release built. A preview lists the
project's whole list, with the ones it does not run marked suspended: a worker
and a scheduled job run in previews only if they were opted in, because a
preview that emails customers nightly is a bad afternoon, and a service runs
unless it was opted out, because a preview missing one of its own services is a
broken preview.

A task's row says what it is doing to the deploy rather than how a workload is
— `--json` carries it as `deploy`: `pending`, `running`, `complete` or
`failed`. `failed` is a release that never landed, and what was serving before
it still is; the run's own message is on the row. Running it again is how a
deploy stopped by a failed migration is picked back up once the cause is gone,
and the platform carries on by itself if it succeeds.

A service's address is on its row and in `--json` as `address`, which is what
somebody wiring two workloads together is after — it is the same value the
environment's other workloads read as `KITCHEN_SERVICE_<NAME>`, with `_HOST`
and `_PORT` beside it:

```sh
kitchen processes --json | jq '.items[] | select(.address) | {name, address}'
```

Declaring the list has no command of its own. It is a list of records with
commands, schedules, ports, builds and resources in it, and a flag-shaped
spelling would be worse than the JSON body — so it goes through `kitchen api`,
alongside the rest of the project's settings:

```sh
kitchen api PATCH /projects/shop --data '{"processes":[
  {"name":"migrate","type":"task","command":["npm","run","migrate"],"timeout":"10m"},
  {"name":"worker","type":"worker","command":["node","worker.js"],"replicas":2},
  {"name":"api","type":"service","port":8080,"build":{"rootDirectory":"services/api"}},
  {"name":"nightly-report","type":"cron","schedule":"0 3 * * *","command":["node","report.js"]}
]}'
```

A `task` is where a schema migration goes: it runs once per deploy however many
copies of the other workloads there are, several of them run in the order they
are declared, and a run that fails stops the deploy where it stands. `timeout`
is how long the deploy waits for it. Reversing a schema change is deliberately
out of scope — forward-only, idempotent work is the contract, and a rollback
runs the task the release it goes back to declared.

A workload with a `build` is built from its own directory of the repository, so
one commit produces several images that ship as one release and roll back
together — which is what makes a monorepo one project rather than four. Without
one it runs the project's image with another command. `kitchen builds` and
`kitchen api GET /builds/<name>` list what a commit produced under `workloads`.

`build.dockerfileTarget` on such a workload is which stage of its Dockerfile to
ship — the per-workload counterpart of `--dockerfile-target` on `kitchen
projects create`, and it goes through `kitchen api` for the same reason the
rest of the record does. A workload that names none is built to the project's
stage rather than to the file's last one, and each row under `workloads` says
which stage that image was built to.

It replaces the whole list, and — like the environment variables and the port —
it reaches an environment through the next release. What is running keeps its
own workloads until something builds.

A worker's `health` goes on the same record. It has to name the port it is
checked on, because a worker publishes none of its own:

```sh
kitchen api PATCH /projects/shop --data '{"processes":[
  {"name":"worker","type":"worker","command":["node","worker.js"],
   "health":{"path":"/healthz","port":9000}}
]}'
```

So does `singleton`, which is the declaration a poller moved out of the web
process needs: two of this workload must never run at once, so a deploy stops
the old copy before starting the new one instead of overlapping the two. It
refuses more than one replica rather than clamping the count, and a scheduled
job is refused it — whether two of *its* runs may overlap is
`concurrencyPolicy`. `kitchen processes` prints it on the workload's row.

```sh
kitchen api PATCH /projects/shop --data '{"processes":[
  {"name":"poller","type":"worker","command":["node","poll.js"],"singleton":true}
]}'
```

### The rest of a project's settings

The same reasoning covers everything else `PATCH /projects/{name}` takes, and
it is a decision rather than a gap: the port, the replica count, the compute
request, the classification and the four runtime declarations below are all
one JSON body written occasionally by an admin, and a flag-shaped spelling of
them would be a second surface to keep in step with the first.

```sh
kitchen api PATCH /projects/shop --data '{
  "health": {"path": "/healthz"},
  "command": ["./server"], "args": ["--config=prod.toml"],
  "previewArgs": ["--config=fake.toml"],
  "singleton": true,
  "notRequestDriven": true}'
```

`health` is what the platform asks before it sends anyone to a new replica;
`command`, `args` and `previewArgs` are how the image is started, with the
preview override the sibling of an environment variable's preview value;
`singleton` says two of the *web* process must never run at once, and refuses
more than one replica rather than clamping it — a worker says the same thing on
its own entry, above; `notRequestDriven` says the workload does work nobody
asked for, so none of its environments idle down to no pods.
[Projects](api/projects.md#changing-a-projects-settings) is the whole of what
the body takes.

### The settings the repository keeps

A project can carry its build and runtime settings in a `kitchen.json` beside
its code, read at every commit the platform builds. `kitchen config` is the
half of that which lives in a terminal:

```sh
kitchen config check                          # the file in this directory
kitchen config check apps/web/kitchen.json    # one in a monorepo
kitchen config schema                         # the URL for the file's own $schema
```

`check` refuses the file the way a build would and lists what it declares, in
the dotted form the API and the dashboard name settings in:

```console
$ kitchen config check
kitchen.json is valid, and takes over 4 settings from the project:
  build.strategy
  env.NODE_ENV
  processes
  runtime.port
```

```console
$ kitchen config check --json
{"path":"kitchen.json","declares":["build.strategy","env.NODE_ENV","processes","runtime.port"]}
```

**It is the one command here that reaches nothing at all** — no platform, no
credential, no network — which is what makes it usable in a pre-commit hook or
a pull request's own checks. It runs the operator's parser, compiled into the
same release, so a file it accepts is a file the build accepts and a file it
refuses would have failed a build several minutes in. It exits `0` for a valid
file, `1` for an invalid one, and `5` when there is no file at all: a project
configured entirely in the dashboard is the ordinary case, and a hook running
this over every repository should be able to tell "nothing to check" from
"what is here is wrong".

What it cannot tell you is whether the file conflicts with the project it will
be built for — a variable the project takes from a provisioned database cannot
be given a literal value in the file, and that is settled against the project,
not against the file. The build says so; this does not.

[docs/CONFIG.md](CONFIG.md) is what the file may and may not say.

### Rolling back

Rollback is not a special operation: a `Release` is an immutable snapshot of an
image digest and the configuration it runs with, so pointing an environment at
an older one puts back exactly what was running.

```sh
kitchen releases              # what there is to move to
kitchen rollback              # back one, from the environment's own history
kitchen rollback shop-rel-41  # or to a named release — a promotion is the same call
```

**It says what the move changes before it asks.** A confirmation that repeats
the release name back is not a safety mechanism — the release name is the one
thing whoever typed it already knew — so above the prompt goes the comparison
of the two configuration snapshots: which environment variables change, go away
or appear, and what moves in the runtime and the process list.

```
shop-rel-42 → shop-rel-41
  ~ NEXT_PUBLIC_CDN                  the value differs
  − FEATURE_BULK_IMPORT              a value → unset
  ~ replicas                         3 → 2
  migrate runs again before this release serves anything, and nothing runs a down step
Move shop-production from shop-rel-42 to shop-rel-41? [y/N]
```

The last line is the one entry that is an action rather than a difference: a
rollback runs the deploy tasks the release it goes back to declared, whether or
not anything about them changed, and the environment takes no traffic until
they succeed.

No value appears there, because none is fetched: the API compares the two
snapshots itself and answers with the verdict alone — see
[`config-diff`](api/environments.md#what-a-move-would-change). `--yes` and
`--json` both mean nobody is being asked, so neither pays for the comparison;
and a comparison that cannot be read is said out loud but never stops the move,
since a safety feature that lengthens an outage is not one.

Against an environment that declares requirements the move is not made on the
spot: the platform answers with the promotion it became, phase `Pending`, and
the policy engine decides whether the release lands. That is still a rollback
without a rebuild — a `Release` is immutable, so re-promoting an old one puts
back exactly what ran.

### Promoting

```sh
kitchen promote shop-rel-41 --environment shop-staging
kitchen promotions --phase Blocked      # what is stuck, and which rules block it
kitchen promotions shop-promo-4kd92     # one promotion whole
```

A promotion is a request: the platform evaluates the target environment's
requirements against the artifact's stored evidence, records the decision, and
applies the move only if the policy allows. A blocked one names the unmet
rules by id, and its `decisionID` leads to `kitchen decisions show` for the
full fired list and the replayable input. `--reason` puts your own words into
the audit record — an emergency move should have one.

### Evidence

```sh
kitchen attestations shop-bld-7
kitchen attestations shop-bld-7 --workload api
kitchen attestations shop-bld-7 --json | jq '.attestations[].predicateType'
```

**A commit that builds more than one image has one artifact per workload**,
each attested about its own digest, so this reads one of them. Without
`--workload` you get the project's own image — the only one a single-workload
project has, and what this command has always printed. `kitchen api GET
/releases/shop-rel-42` answers the release-level question instead: whether
*every* image the release deploys is attested, and which are not.

Everything it prints is read out of the registry, keyed to the artifact's
content digest through OCI referrers rather than out of a Kitchen table — so
`cosign download attestation` and `cosign verify-attestation` answer the same
thing with the platform out of the loop, which is what makes the evidence
survive an installation that stops using Kitchen.

Each attestation says who made the claim. Kitchen's build record is the
reconciler's account of a build it orchestrated; SLSA provenance and the bill of
materials come from the builder and are countersigned by the platform. The
signature on all of them is the platform's, so the signature cannot tell them
apart — `source` on the build's own `artifact.evidence` can.

`verified` means a signature was accepted by a key the platform holds. A set
read where it holds none reports itself as a listing rather than a verification,
and the two print differently on purpose.

### Quality gates

```sh
kitchen gates list shop-bld-7
trivy image --format json shop:latest | kitchen gates submit shop-bld-7 --gate trivy --findings -
trivy image --format json shop-api:latest |
  kitchen gates submit shop-bld-7 --gate trivy --workload api --findings -
```

`Completed` means the gate ran, whatever it found; `Failed` means it did not run
and nothing is known either way. Neither says whether the findings were
acceptable — that is decided at promotion, against the environment being
deployed to.

A gate is a claim about an **image**, so `list` prints one row per gate per
image the commit produced, with `IMAGE` naming which — `web` for the project's
own. `submit --workload` says which image a result from elsewhere ran over; a
result about one image is recorded against that image and nothing else.

### Exploitability

```sh
kitchen vex list shop-bld-7
kitchen vex list shop-bld-7 --workload worker
kitchen vex submit shop-bld-7 --workload worker --document @not-affected.openvex.json
```

A scanner says what was found; an OpenVEX statement says whether it applies
here — the component is not present, the vulnerable code is not in the execute
path, a mitigation already covers it. Without it, a daily rescan of a real
dependency tree reports enough that people stop reading it.

A statement is about an **image**, so `--workload` names which one. "This CVE
does not apply" said about the API is not a claim about the worker, which may
not carry the package at all; without the flag both commands mean the
project's own image.

`list` prints every finding beside the statement covering it, its
justification, its author, who submitted it, and one word for what the platform
can establish: `current`, `expired`, `unverified` or `unjustified`. It is never
the word "suppressed" — whether a statement suppresses anything is the target
environment's policy's question, and the same statement can be honoured in
staging and refused in production. A suppressed finding is still listed, which
is the point of the view.

`submit` sends the document as the exact bytes its author wrote; the platform
signs those bytes and records, separately, the identity that sent them. A
`not_affected` statement must give one of OpenVEX's five justifications —
`component_not_present`, `vulnerable_code_not_present`,
`vulnerable_code_not_in_execute_path`,
`vulnerable_code_cannot_be_controlled_by_adversary`,
`inline_mitigations_already_exist` — and free text alone is refused, because a
suppression whose reason cannot be counted cannot be reviewed. Submitting needs
`admin` on the project: an assertion that stops a finding counting is nearer to
approving a break-glass exception than to reporting a scan, so a project's CI
key cannot file one.

### Policy decisions

```sh
kitchen decisions list --verdict blocked
kitchen decisions show 0d9a1f7e-…
kitchen decisions replay 0d9a1f7e-…
```

Every verdict the platform reaches is a stored decision citing the policy
bundle and the input it was computed from, both by digest. `replay` re-runs a
historical decision from those stored bytes and reports whether the verdict
reproduces — the command succeeds either way; `match` in the answer is the
finding. Replaying writes a decision of kind `replay`, so it needs developer
on the decision's project.

`submit` is for a scanner the pipeline already ran. The findings are sent as the
exact bytes the tool wrote, and the result is recorded as reported by the
credential that sent it: the platform signs it but did not witness it, and a
policy that trusts only what the platform ran itself can tell the difference.
Do not pass the scanner a flag that makes it exit non-zero on findings — the
pipeline would fail on a fact rather than on a decision.

### Compliance drift

```sh
kitchen drift
kitchen drift --project shop --all --json
```

What is deployed right now that would not be allowed to deploy today. Every
deployed release is re-evaluated on a schedule against a current vulnerability
database, through the same policy path a promotion uses — no rebuild and no
redeploy — and this reads the result.

The `status` column is the whole point of the view, because a blocked verdict
looks the same whichever of these it is:

- `newly-failing` — a rule that did not fire when this release was promoted
  fires now. The artifact did not change; a vulnerability database did.
- `waived-at-promotion` — a rule that fired at promotion too and was waived by
  a break-glass grant that has since run out. Nothing new was found.
- `waived` — still clearing the bar, but only because a grant is waiving what
  fires. Compliant by grace, and dated.
- `not-evaluated` — no current re-evaluation stands for this pair, either
  because nothing has ever re-checked it or because the last scan did not run.
  It is a finding about the platform rather than about the release, and it is
  never counted as compliant.

Compliant pairs are left out unless `--all` asks for them, and the answer leads
with whether the pass is running at all: an empty table under a pass that is off
means *nobody is looking*, which is not the same answer as nothing being wrong.

The exit code stays zero on a non-empty answer. Drift is a finding, not a
failure of the command — a command that failed on a finding gets turned off the
first week it finds something — so a nightly `kitchen drift --json` that opens a
ticket on a non-empty `items` is the shape this is for.

### Criticality, and what depends on what

```sh
kitchen criticality --criticality critical --json
kitchen criticality dependents --provider neon --json
```

The first is the function-to-resource mapping: every designated function with
the environments, releases, resource claims, connections, domains and third
parties standing behind it. It is the operational-resilience register an
institution would otherwise assemble by hand across four systems, and it is a
nightly job away from being kept current — the answer is derived from the
reconciled graph on every request, so there is nothing to keep in step.

The second walks it backwards, which is the question asked during an incident
by somebody who has a terminal and not a browser: every environment that
depends on one Connection or on one third party, worst designation first, with
the tightest recovery objective among them. Exactly one of `--connection` or
`--provider`. Nothing depending on it is an empty answer and exit 0.

**Kitchen does not decide what is critical and does not set the tolerances.**
Designating a project or an environment is a rare, deliberate write made by
somebody who is not in a terminal at the time, so it has no command of its own
and goes through `kitchen api`:

```sh
kitchen api PATCH /projects/shop --data '{"criticality":"critical","rto":"1h","rpo":"5m"}'
kitchen api PATCH /environments/shop-production/requirements --data '{"criticality":"critical","rto":"15m"}'
```

### Access, and who reviewed it

```sh
kitchen access identities --json
kitchen access identities --orphaned --json
kitchen access reviews --historical --json
kitchen access show access-review-8x2kd --json
```

One row per grant, not per account: an account holding admin on three projects
is three rows, because those are three decisions for a reviewer. `--orphaned`
narrows to the grants that are **both** dormant and unknown to the identity
provider — either alone has an innocent reading, the pair does not — which is
the list worth acting on, and the reason this is a command rather than a
`kitchen api` line. A monthly job that opens a ticket on a non-empty answer is
the shape it is for; the exit code stays zero, because a command that failed on
a finding gets turned off the first week it finds one.

Two words to read carefully in the answer. `inactive` is the audit log's, and
the audit log records writes, so an account that only ever reads looks dormant
and is not. `directoryConsulted: false` means nothing at all is claimed about
whether a grant belongs to anybody — a federated issuer serves no account
directory, and "we could not ask" is not "nobody is behind it".

Opening a cycle and deciding a grant are writes with a person's name on them,
so they are spelled out rather than made muscle-memory:

```sh
kitchen api POST /access/reviews --data '{"scope": "all", "reason": "the annual audit"}'
kitchen api PATCH /access/reviews/access-review-8x2kd --data '{"decisions":
  [{"subject": "user_7", "grant": "shop", "decision": "revoke", "note": "left in June"}],
  "close": true}'
```

Closing is what carries out the revocations and mints the retained artefact.

All of these need the operator role: the answer is the whole installation's
access in one document. So all of them are
[the dashboard's for now](#the-platform-commands-are-the-dashboards-for-now) — no credential
`kitchen login` can store holds that role, and the screen that can is
**Platform → Audit**, under Access recertification. See
[docs/api/access.md](api/access.md).

### Retention

```sh
kitchen retention
kitchen retention --json
```

One row per class of what the platform keeps: container logs, build logs,
flows, metrics, traces, requests, cluster events, the activity feed and the
audit log. Each row is the rule in force, whether somebody set it or it is
inherited, and — the column that matters — the oldest row the last retention
sweep found. That last one is the claim retention actually makes: nothing of
this class is older than this.

It is worth having in a pipeline for the same reason `kitchen drift` is. "How
long do you keep container logs, and can you show me that nothing older than
that is there" arrives with a deadline attached, and this answers it in one
call.

An installation keeping audit records for less than the platform's 90-day floor
prints the override that says who decided so and why. An override nobody can
see is not an override, it is a setting.

**Changing it is left to `kitchen api`, deliberately.** A retention change is a
records decision rather than an operational one, and putting the audit floor's
override behind a flag would make typing it easier than thinking about it:

```sh
kitchen api PATCH /platform/retention --data '{"buildLogs": 180}'
kitchen api PATCH /platform/retention --data '{"audit": 60,
  "auditFloorOverride": {"reason": "…", "approvedBy": "cto@example.com"}}'
```

See [docs/api/platform.md](api/platform.md) for the bodies and what the floor
refuses.

Reading it needs the operator role, so this command is
[the dashboard's for now](#the-platform-commands-are-the-dashboards-for-now): the same table is
on **Platform → Settings**, under Retention, and that is also where changing it
has a form.

### The audit pack

```sh
kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01
kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01 --format html
```

One project's whole compliance answer for one window, as files on disk: the
inventory, the change log with the author and the approvers of every release,
the promotions and the decisions behind them with the full inputs they can be
replayed from, the evidence attached to each artifact, the break-glass
exceptions, the recertification cycles that reviewed this project's access,
what is running that no longer meets its bar, the project's slice of the audit
log, and every signed statement the platform holds that has no registry to
live in.

**Two files by default**, and that is the whole reason this is a command
rather than a `kitchen api` invocation: the pack and the DSSE envelope that
signs it are one deliverable and useless apart, and somebody assembling them
out of two `curl`s eventually ships the pack without the envelope. The pack's
own verification block carries the four commands that check them with this
platform switched off.

**Both ends of the window are required.** A pack that ended "now" could not be
reproduced, and reproducibility is the point: two exports of the same window
are the same bytes unless the evidence changed. A bare date is read as midnight
UTC, since that is what a quarter's boundaries actually are.

The dashboard has the button, and the button is the point of the feature — an
auditor should not need a terminal. This is for the other half of the same
problem: evidence produced quarterly is produced on a schedule, and a pack that
only happens when somebody remembers to click is one somebody will eventually
not remember to click.

```sh
# fail the job when the window is not fully covered
kitchen audit-pack --project shop --from 2026-01-01 --to 2026-04-01 --json |
  jq -e '.truncated | not'
```

`truncated` is true when the pack answers for less than it was asked for —
retention has removed part of the window, or a section hit its cap — and
`coverage` carries the platform's own words for it. `digest` is computed from
the bytes that were written rather than taken from the platform's header, so a
mismatch with `servedDigest` is visible rather than assumed away.

It needs the operator role, for the reason the route does: a pack folds three
operator-only reads into a project's evidence — so this command is
[the dashboard's for now](#the-platform-commands-are-the-dashboards-for-now) too, and the button
on **Platform → Audit** is what takes one today. The
scheduled-export case above is exactly what
[#349](https://github.com/Bermos/Kitchen/issues/349) would restore. See
[docs/api/audit-pack.md](api/audit-pack.md).

### Anything else

```sh
kitchen api GET /domains
kitchen api POST /domains --data '{"host":"shop.example.com","environment":"shop-production"}'
kitchen api GET /builds/shop-bld-abc-xk2p9/logs --stream
```

The path may be written with or without the `/api/v1` prefix; the body is JSON,
literally, from `@file`, or from `-` for stdin. The refusal a route gives is
printed as the platform wrote it, and the exit status is this CLI's usual one —
so a script branches on the outcome without parsing the body.

### Backing the platform up

One archive: every Kitchen object, every secret in the platform namespace, and
the identity provider's database. Telemetry is not in it — ClickHouse is not
backed up and is not expected to survive — and the archive's own manifest says
so, which is what this prints.

```sh
kitchen backup                                   # into the current directory
kitchen backup /backups/kitchen.tar.gz --force   # somewhere else, overwriting
```

The reason this exists next to the dashboard's button is scheduling: a backup
that only happens when somebody remembers to click is not a backup. `--json`
answers one object naming the file and what went into it, read back off the
file rather than remembered — which is enough for a cron job to check that the
archive it just took carries the accounts as well as the objects:

```json
{"file": "/backups/kitchen-backup-prod-2026-08-19T090000Z.tar.gz", "bytes": 41203,
 "platformVersion": "0.9.0", "objects": 37, "secrets": 9, "accountRows": 214}
```

The archive is a credential. It holds every secret the platform has, in the
clear; keep it where you would keep the cluster's root credentials, and off the
cluster it came from. Taking one is recorded in the platform's audit log as an
`export`.

#### The scheduled half

The platform takes its own backups once a schedule and a destination are set —
see [docs/BACKUP.md](BACKUP.md). Two commands read and drive that:

```sh
kitchen backup list    # what is at the destination, read from the destination
kitchen backup run     # take one to the destination now
```

`kitchen backup list` reads the bucket rather than the platform's status: the
status says what the last run believed it did, and this says what a recovery
would actually find. **The age of the newest archive here is the check worth
alerting on**, rather than the exit code of the last run — a run that exits 0
having uploaded nothing looks healthy, and only the bucket disproves it. Every
object is marked with whether it is one this platform wrote, so a bucket that
holds other things does not leave anybody guessing what retention would touch.

`kitchen backup run` answers as soon as the run has been *started*, with the
name of the Job doing the work. It is what makes a destination testable: press
it on the day you configure one, rather than finding out at 02:00 six weeks
later.

Configuring the destination stays `kitchen api PUT
/platform/backup/destination` — a one-time operator setup with a credential in
it, which is exactly the fallback that escape hatch exists for — and the
schedule and retention are `kitchen api PATCH /settings` with
`backupSchedule`, `backupSuspend`, `backupKeepLast` and `backupKeepDays`.

There is no `kitchen restore`, and there cannot be: a restore happens into a
cluster whose accounts database is gone, so the credential this CLI signs in
with is inside the archive and there is nobody left to run it. The chart renders
a Job for that instead — [docs/BACKUP.md](BACKUP.md) is the procedure, and CI
runs it on every change.

Reading what an archive *would* carry, without taking one, is
`kitchen api GET /platform/backup` — which needs the operator role like the
export itself, so both are
[the dashboard's for now](#the-platform-commands-are-the-dashboards-for-now).
**Platform → Backup** has the button; the scheduling this command exists for is
what
[#349](https://github.com/Bermos/Kitchen/issues/349) is about.

## Output, exactly

There are two modes and they never mix.

**`--json`.** stdout carries JSON and nothing else. A command that answers with
one thing writes one document; a command that follows something writes one JSON
object per line, each with a `type`, ending in a `result`. Progress and
warnings go to stderr as `{"type":"note"}` and `{"type":"warning"}` objects.

**Text.** Whatever reads best for a person: styled when stdout is a terminal,
the same words unstyled when it is not, so `kitchen logs | grep` is not full of
escape sequences.

A failure is one shape in both modes — on stdout under `--json`, on stderr
otherwise:

```json
{"error": {"code": "forbidden", "message": "you have viewer on shop; redeploying needs developer",
           "status": 403, "doing": "moving shop-production to shop-rel-41"}}
```

`message` is the API's own sentence where the API refused, because it is written
to be read by whoever sent the request. `hint` says what would fix it.

### Exit codes

| Code | Name | When |
|---|---|---|
| 0 | `ok` | It did what it was asked |
| 1 | `failed` | A failure with no code of its own |
| 2 | `usage` | The command line is wrong: an unknown flag, a missing argument, a value that does not parse |
| 3 | `unauthenticated` | No usable credential, or the platform refused the one there was |
| 4 | `forbidden` | This account may not do this; the message names the role it wanted |
| 5 | `notFound` | No such object — or one this account may not know exists, which the API answers identically on purpose |
| 6 | `conflict` | Something changed it first, it already exists, it already finished, or something still uses it |
| 7 | `unavailable` | A capability the endpoint needs is not installed |
| 8 | `unreachable` | The API could not be reached at all |
| 9 | `buildFailed` | A followed build ended Failed or Cancelled. The command worked; the build did not |
| 10 | `notLinked` | No project could be resolved |
| 11 | `timedOut` | A wait ran out. Nothing was undone |
| 12 | `deployFailed` | A followed deploy ended Degraded: the build succeeded and the release did not take traffic. What was serving before it still is |
| 130 | `interrupted` | SIGINT. Whatever was already started on the platform keeps running |

`kitchen schema | jq '.exitCodes'` is the same table, from the CLI itself.

## Environment and files

| Variable | Meaning |
|---|---|
| `KITCHEN_API` | The installation to talk to |
| `KITCHEN_PROJECT` | The project to act on |
| `KITCHEN_TOKEN` | A platform token, used as it is |
| `KITCHEN_API_KEY` | A key to exchange for one |
| `KITCHEN_JSON` | `1` turns `--json` on for a whole session |
| `KITCHEN_NO_INPUT` | `1` turns `--no-input` on |
| `KITCHEN_CONFIG_HOME` | Where the credential file lives |

| File | Holds |
|---|---|
| `<config dir>/kitchen/auth.json` | One entry per installation: the issuer, the API key, and the last exchanged token with its expiry. Mode `0600` |
| `<working copy>/.kitchen/project.json` | The project this directory deploys to, and the installation. No credential |

The exchanged token is cached until it expires, so a script running twenty
commands exchanges once. Losing that cache costs one request; a machine that
cannot write it carries on and exchanges every time.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Where it lives | This repository, released with the platform | One tag versions the chart, both images and the CLI; a client and the API it depends on move together, and the schema test can check the CLI's claims against the API's own route table |
| Sign-in | An API key exchanged at the issuer | The device grant does not exist in the issuer's OAuth provider, and a loopback client would have to be seeded for one fixed port. A key is also the narrowest credential the platform issues — a role on one project |
| Machine-first output | `--json` everywhere, NDJSON for anything followed, an error envelope, fixed exit codes | A CLI that can only be read by a person is a CLI that has to be screen-scraped, and a scraped surface is one nobody can change |
| The schema | Derived from the commands, and tested against the API's route table | A published surface that is written by hand falls behind the real one; this one cannot, and an endpoint that moves fails a test rather than a user |
| The escape hatch | `kitchen api`, reaching any endpoint | The API is larger than this CLI and always will be. Nothing the platform can do should be unreachable from a terminal because nobody has written a subcommand yet |
| Bubble Tea | Only when stdout is a terminal and `--json` is off | The follower prints log lines *above* the status block, so they land in the terminal's scrollback rather than in a frame that gets redrawn — and none of it can reach a pipe |
| Prompts | Every question has a flag; `--no-input` is implied off a terminal | A command that hangs waiting for an answer in CI is worse than one that refuses and says which flag it wanted |
| Following a deploy | Poll the build, stream its logs, then watch for the release and the environment | All four are things the API already answers; the CLI renders them and drives nothing |
| The exit status of a deploy | The build's, plus `12` for an environment that settled `Degraded` | "Did my build pass", "was my release refused" and "is it live yet" are three questions. The first two are answers a pipeline has to be able to branch on, so they are codes; the third is a fact about the environment, and stays in the result where a caller reads the phase and the URL |
| When `Degraded` counts as refused | Only once it has settled: it holds, and no condition says work is in flight | A phase is what the last reconcile left behind, so a retry reads `Degraded` for a moment before the platform looks at the new release. Stopping at the first `Degraded` would report that as a failure — and stopping at none of them is the hole this closes |
| Credentials on the command line | `--api-key-file` and `--api-key-stdin` preferred, `--api-key` documented as visible in the process list | The convenient spelling should not be the one that leaks |
| A project's settings | No command; `kitchen api PATCH /projects/{name}` | One JSON body written occasionally by an admin — a port, a replica count, a health check, a security posture, a process list, arguments, a classification, the Dockerfile stage to ship (which `projects create` does carry, since the first build starts with the project). A flag per field would be a second surface to keep in step with the first, and a list of records with commands and schedules in it has no flag-shaped spelling worth having |
| The platform commands | Declared the dashboard's for now, in `--help`, in `kitchen schema` and in a refusal that names the screen | A key is a role on one project and those routes need the operator role, so no credential this CLI can store runs them — and `kitchen api` carries the same token, so it is no way round a *role*. Shipping them published and silently unrunnable was the state [#208](https://github.com/Bermos/Kitchen/issues/208) found; a platform-scoped key is the real answer and is [#349](https://github.com/Bermos/Kitchen/issues/349), designed with [#318](https://github.com/Bermos/Kitchen/issues/318) because both decide what a key is |
| Account management | No command, and none possible | Changing a password, or ending a session, is done at the identity provider against its session cookie — and this CLI holds a key, never a session. It is not an endpoint `kitchen api` reaches either, because that reaches the operator API and these are not on it ([AUTH.md](AUTH.md), "Managing an account") |

## Open

- **Browser sign-in**, once `auth/` decides between the device grant and a
  seeded loopback client — see [Signing in](#why-there-is-no-browser-sign-in).
- **A credential for the platform commands.**
  [#349](https://github.com/Bermos/Kitchen/issues/349): a platform-scoped key,
  narrower than the operator role, designed together with
  [#318](https://github.com/Bermos/Kitchen/issues/318) — a key today is a full
  identity-provider session, and both changes decide what a key *is*. Until it
  lands, `kitchen backup`, `kitchen retention`, `kitchen access` and
  `kitchen audit-pack` are
  [the dashboard's](#the-platform-commands-are-the-dashboards-for-now), and say
  so themselves. The fresh-install bootstrap loop is part of that issue too.
- **Released binaries.** The CLI builds from source and installs with `go
  install`; attaching cross-compiled binaries to the GitHub release is a fourth
  artifact job in `publish.yml`, and the release only goes live once every
  artifact exists ([CONTRIBUTING.md](../CONTRIBUTING.md)).
- **Shell completions.** Cobra can generate them; the schema is the machine
  surface, and completions are the human one that has not been asked for yet.
- **`kitchen domains` and `kitchen claims`.** Both are reachable through
  `kitchen api` today. They earn commands of their own when the domain
  reconciler lands ([API.md](API.md), write surface) and when somebody types
  them often enough. The `oidcClient` claim type did not change that: it added
  no route and renamed none — `POST /claims` gained three optional fields —
  so `kitchen api POST /claims` reaches it, and a `kitchen claims` command
  would still be one command family for two kinds of claim rather than the
  other way round. The self-hosted Postgres provider did not change it either,
  for the same reason: `POST /claims` gained one optional nested object and
  `POST /connections` made `credential` optional, and neither is a route.
  The `volume` claim type is the same decision a third time: one required
  nested object on `POST /claims`, no new route, and `kitchen api POST
  /claims` carries it. So are `objectStore`, `inngest` and `redis`, which
  between them added five claim types to that route and not one route — which
  is the point of a claim type being a row in a table rather than a surface of
  its own. Requiring `admin` for `deletionPolicy: Delete` is the same decision
  once more, and the cheapest kind of it: it added no route, renamed none, and
  left both rows' own requirements where they were — so `kitchen api POST
  /claims` and `kitchen api DELETE /claims/{name}` reach both ends of it, and
  the API's `403` naming the field and the role it wants is printed as it
  stands, through the one error shape every command already answers with. A
  flag would only be a way to spell a role the caller either holds or does not.
  `kitchen env set --from-claim` already reads any of their bindings
  by name, and it did not have to learn what a bucket or a queue is to do it.
  Asking for a database with an extension is one line as it is:

  ```sh
  kitchen api POST /claims --data '{"name":"maps-db","project":"maps",
    "connection":"postgres","type":"postgres",
    "postgres":{"version":"17","extensions":["postgis"],"storage":{"size":"40Gi"}}}'
  kitchen api GET /claims?project=maps | jq '.[] | {name, phase, postgres}'
  ```

  A claim refused for an extension nothing can supply answers `Failed` with
  the reason in its `Ready` condition, which is on that same JSON — so the
  refusal reaches a terminal without a command being written for it.
- **`kitchen addons`.** The one surface here that *did* add routes — five of
  them, `GET`, `POST`, `PATCH` and `DELETE` under `/addons` — and still gets
  no command, which is a decision rather than an omission by default. What an
  addon is, is a dependency the platform installs into its own cluster: that
  sits beside installing the chart and following the bootstrap link, in the
  cluster-bootstrap exception, and it is the operator's rather than something
  anybody does in the normal running of a project. Turning one on is one
  line, and reading what the cluster has is another:

  ```sh
  kitchen api GET /addons | jq '.items[] | {id, permitted, serving, managed}'
  kitchen api PATCH /addons/cloudnative-pg --data '{"install":true}'
  ```

  The refusal an unpermitted entry answers with names the chart value that
  would permit it, and setting that value is a `helm upgrade` — which is not
  something a CLI command could have finished either.
- **`kitchen update`.** The platform's own upgrade — `GET /updates`,
  `POST /updates`, `GET /updates/{name}` and now
  `GET /updates/{name}/logs` — has no command family here, deliberately.
  Upgrading the platform is the operator's, it is done once per release from
  the settings page, and a CLI that could do it would be a fourth place to
  keep the version arithmetic (what is published, what this installation would
  accept, whether a minor crossing is allowed) in step. `kitchen api GET
  /updates/update-0-2-1-h4k9c/logs --stream` follows helm's output the same way
  `--stream` follows a build's, which is the whole of what a command would have
  added.
