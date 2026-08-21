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

```sh
make cli            # builds bin/kitchen
make cli-install    # installs it into GOBIN

go install github.com/Bermos/Kitchen/cmd/kitchen@latest
```

The version is stamped by the linker from the same `LDFLAGS` the images use, so
`kitchen version` reports the release the platform is on.

## Signing in

**The CLI authenticates with an API key**, exchanged at the platform's identity
provider for the short-lived token the API actually sees — the flow
[API.md](API.md#getting-a-token) documents for CI, used here for a laptop as
well. Issue one from a project's Keys tab in the dashboard, or with `POST
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
`shop` and cannot see that any other project exists. That is the right shape for
a CLI, which is exactly where a too-broad token ends up on a laptop.

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
  made deliberately.

So the API key is the whole of it, and that is a smaller credential than a
person's token would be. If the issuer grows a device endpoint, `kitchen login`
gains a browser flow and the key path stays for CI.

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
| `kitchen env list/set/rm` | The project's environment variables | `PATCH /projects/{name}/env` |
| `kitchen rollback` | Put an environment back on an earlier release | `PATCH /environments/{name}` |
| `kitchen projects` | The projects this account can see, with its role on each | `GET /projects` |
| `kitchen builds` | The project's builds, newest first | `GET /projects/{name}/builds` |
| `kitchen attestations` | The signed evidence attached to a build's artifact | `GET /builds/{name}/attestations` |
| `kitchen gates list/submit` | What ran over an artifact, and submitting a result from elsewhere | `GET /builds/{name}`, `POST /builds/{name}/gates` |
| `kitchen releases` | The project's releases — what there is to roll back to | `GET /projects/{name}/releases` |
| `kitchen environments` | The project's environments and where they answer | `GET /projects/{name}/environments` |
| `kitchen api` | Any endpoint of the API, authenticated | anything |
| `kitchen schema` | The whole CLI as JSON | — |
| `kitchen backup` | Take a backup of the platform and write it to a file | `POST /platform/backup` |

### Creating a project

```sh
kitchen projects create
```

is the whole of it in a checkout: the repository comes from `origin`, the name
from the repository, and the two connections from the platform when it offers
only one that can do each job. It ends by writing the same
`.kitchen/project.json` `kitchen link` writes, so the directory is linked to the
project it just made.

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
`--root-directory` and `--dockerfile` are sent with the project rather than
patched onto it: a monorepo whose application is in `apps/shop` would otherwise
fail one build before anybody realised what to correct.

A repository the platform recognised nothing in — and that has no Dockerfile
either — is a question rather than a refusal, since the detector reads one
commit and the person is looking at the whole repository. Like every other
question here it has a flag that answers it, so `--yes` creates the project and
nothing ever waits:

```sh
kitchen projects create shop --repo acme/mono --root-directory apps/shop \
  --connection github --registry kitchen --yes --json
```

The preflight is advice, so a platform that cannot reach the provider to give
any is reported on stderr and the project is still created. The flags are
`--repo`, `--connection`, `--registry`, `--production-branch`, `--previews`,
`--root-directory`, `--dockerfile`, `--link` (on by default) and `--yes`;
leaving `--previews` off leaves the platform's default alone rather than
turning previews off.

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

**The exit status is the build's**: `0` when it succeeded, `9` when it failed or
was cancelled. Whether the environment went live inside
`--environment-timeout` is in the result rather than in the status, because it
is a different question from whether the deploy worked — the result carries the
environment's phase and URL either way.

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
kitchen env set API_KEY --from-secret shop-api-key:key
kitchen env set DATABASE_URL --from-claim shop-db:url
kitchen env set --from-file .env
kitchen env rm LOG_LEVEL --yes
```

Variables land in the next release's snapshot: what is already running keeps the
configuration it was released with until the next deploy.

### Logs

```sh
kitchen logs                                  # the production environment's last 200 lines
kitchen logs --follow                         # and everything after them
kitchen logs --since 1h --search error --json
kitchen logs --build shop-bld-abc123def456-xk2p9
kitchen logs --environment shop-pr-42 --follow --timeout 10m
```

`--since` and `--until` take an RFC 3339 timestamp or a duration (`15m`, `2h`)
read as "that long ago". A bounded page and a followed tail answer the same
shape — one line per log line — so `--follow` changes how long the command runs
and nothing else.

### Rolling back

Rollback is not a special operation: a `Release` is an immutable snapshot of an
image digest and the configuration it runs with, so pointing an environment at
an older one puts back exactly what was running.

```sh
kitchen releases              # what there is to move to
kitchen rollback              # back one, from the environment's own history
kitchen rollback shop-rel-41  # or to a named release — a promotion is the same call
```

### Evidence

```sh
kitchen attestations shop-bld-7
kitchen attestations shop-bld-7 --json | jq '.attestations[].predicateType'
```

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
```

`Completed` means the gate ran, whatever it found; `Failed` means it did not run
and nothing is known either way. Neither says whether the findings were
acceptable — that is decided at promotion, against the environment being
deployed to.

`submit` is for a scanner the pipeline already ran. The findings are sent as the
exact bytes the tool wrote, and the result is recorded as reported by the
credential that sent it: the platform signs it but did not witness it, and a
policy that trusts only what the platform ran itself can tell the difference.
Do not pass the scanner a flag that makes it exit non-zero on findings — the
pipeline would fail on a fact rather than on a decision.

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

There is no `kitchen restore`, and there cannot be: a restore happens into a
cluster whose accounts database is gone, so the credential this CLI signs in
with is inside the archive and there is nobody left to run it. The chart renders
a Job for that instead — [docs/BACKUP.md](BACKUP.md) is the procedure, and CI
runs it on every change.

Reading what an archive *would* carry, without taking one, is
`kitchen api GET /platform/backup`.

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
| The exit status of a deploy | The build's | "Did my build pass" and "is it live yet" are different questions; the second is in the result, where a caller can read the phase and the URL |
| Credentials on the command line | `--api-key-file` and `--api-key-stdin` preferred, `--api-key` documented as visible in the process list | The convenient spelling should not be the one that leaks |

## Open

- **Browser sign-in**, once `auth/` decides between the device grant and a
  seeded loopback client — see [Signing in](#why-there-is-no-browser-sign-in).
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
  other way round.
