# Kitchen — A project's configuration files

Software written for this platform is configured by environment variables, a
secret a variable reads, and a volume it can write to. Twelve-factor
configuration is a decision the platform may assume of code that was written
to be deployed by it.

It cannot assume it of code somebody else wrote. Home Assistant is configured
by `configuration.yaml`, Gitea by `app.ini`, Plex by a preferences file, and a
large share of the vendored estate by a file at a fixed path. A configuration
file is where one goes.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## What a configuration file is, and what it is not

It is **configuration**: content, a path, and the workloads that read it. It
is small, it changes with a deploy, and it is frozen into every Release beside
the environment variables — so a rollback restores the file that release ran
with, which is the whole reason it is not a volume somebody wrote into by hand
once, out of band.

It is **not storage**. A file the application writes back wants a
[volume claim](claims.md); this one is mounted read-only. It is capped at
128 KiB, which is the size of configuration rather than of data.

It is **never templated**. The file is placed exactly as written — nothing is
substituted into it from the environment. That is a decision rather than an
omission, and the reasoning is at the bottom of this page.

## The shape

```json
{
  "name": "configuration",
  "path": "/config/configuration.yaml",
  "content": "logger: info\n",
  "workloads": ["web"],
  "secret": false
}
```

| | |
|---|---|
| `name` | A name for the file, and the key every surface refers to it by — letters, digits, `-`, `_` and `.`, at most 253 characters. It is **not** the path, so a file that moves keeps its name |
| `path` | Where the file appears inside the container: absolute, naming the file itself rather than the directory holding it. Only that one path is replaced — the rest of the directory stays as the image left it, which is what a config file dropped beside an application's own files needs. **Optional**: a file with no path is placed in no container, and exists to be seeded into a volume — see below |
| `content` | The file, verbatim, at most 128 KiB. Absent on a write **keeps what is stored**; absent on a read of a secret file means there is nothing to show |
| `workloads` | The workloads that mount it — `web` for the web process, and a workload's own name for anything in the project's list. Empty is **every** workload of the unit, which is what a vendored application's single config file wants |
| `secret` | The content is a credential: it is held where nothing reads it back, and written through a route of its own |

Two files at one path on one workload is refused rather than resolved: the
second mount would win and the first would silently never appear. A file with
no path is mounted nowhere and so collides with nothing.

### A file the application will own

A mounted config file is **read-only**, and that is right for a file the
platform places and the application reads. It is wrong for the other kind:
Home Assistant's `configuration.yaml` is a file the application rewrites, and
mounting one where it expects to write would shadow its own copy for ever.

That file is declared here with **no `path`** and copied into a volume by the
workload's
[`init`](projects.md#a-volume-the-process-cannot-start-on) instead — once,
where the destination does not exist, after which it is the application's. It
is still a configuration file in every other respect: it is held here, frozen
into the release, and restored by a rollback.

## Reading them

They are on the project, because they are part of what the project declares:

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop | jq .files
```

A **plain** file carries its content. A **secret** file carries `contentHash`
and `size` instead — a short digest of what the platform holds — and carries
them only once content has been written. A secret file with neither is
declared and empty, which is a state worth naming: the workloads that mount it
will not start until it has content.

## Writing a plain file

On the project's settings, with everything else it declares:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"files": [{"name": "configuration", "path": "/config/configuration.yaml",
        "content": "logger: info\n"}]}' \
  https://kitchen.apps.example.com/api/v1/projects/shop
```

**The list replaces the project's**, and a file whose `content` the request
leaves out keeps the content it has. That is what makes "read the list, change
one file, send the rest back" possible against an API that never reads a
credential back: the client has nothing to send for the files it is not
changing. `"files": []` removes them all.

## Writing a secret file's content

The declaration comes first, on the settings route, with `"secret": true` and
no content. Then:

```sh
curl -sS -X PUT -H "authorization: Bearer $TOKEN" \
  -d '{"content": "[server]\nSECRET_KEY = s3cr3t\n"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/files/app-ini
```

**Setting and replacing are the same request.** A `PUT` on a file whose
content is already there answers `200`; one on a file that has none answers
`201`. There is deliberately no way to ask which it will be first — that
question is only answerable by something that can read the content.

The answer is the declaration and a digest, and cannot be anything else:

```json
{"name": "app-ini", "path": "/data/gitea/conf/app.ini", "secret": true,
 "contentHash": "9f2c4a1b7e3d5086", "size": 214}
```

A file the project does not declare answers `404` naming the settings route; a
file that is not secret answers `400` saying its content travels with its
declaration.

**There is no route to read one and no route to delete one.** Reading is what
the whole feature refuses. Deleting is taking the file off `files` on the
settings route, which removes the content the platform held with the
declaration that named it — a credential outliving its declaration is residue
nobody can see.

## Why secrecy is a flag and not a second list

The issue this came from left it open, and it is a flag on one object.

A file is one thing: a name, a path, the workloads that read it, and content.
Every one of those is true of a plain file and of a secret one, and a file
that *becomes* secret keeps all of them. Two lists would mean moving a file
house to change one property of its content, and would leave the screen with
two tables answering "which files does this project place".

What is genuinely two things is the **writing**, and that is where the split
is: a plain file's content is not a credential and travels with its
declaration, and a secret file's has a route no response answers. One
declaration, two write surfaces — which is the same shape a project's
environment variables already have, where the variable is one entry and
`fromSecret` sends the value somewhere else.

## Why they are not templated from the environment

Substituting `${DATABASE_URL}` into a config file is the obvious next ask, and
the answer is no, stated here rather than left open.

It turns the platform into a template engine. That is a language, with a
syntax, an escaping rule, a failure mode for an undefined variable and a
question about what happens to a `$` the application meant literally — none of
which the platform is in a position to answer for somebody else's software,
and all of which it would then own for ever. It is the first step towards the
thing [#285](https://github.com/Bermos/Kitchen/issues/285)'s compiled
catalogue exists to refuse.

It also breaks the property that makes a file worth freezing. A file rendered
per environment is no longer the file the Release froze: two environments on
one release would run two different files, and a rollback would restore a
template rather than a file. The [Helm chart spike](../spikes/helm-charts-2026-09.md)
looked for a case that changes this and found none — every config file in the
five charts it rendered is either static or assembled by the chart's own init
script.

An application that needs one value to differ per environment reads it from an
environment variable, which is what those are for. One whose config file
format cannot reference the environment needs two files, one per environment —
and that is a project setting, not a template.

## What a change does to what is running

A plain file's content is in the Release's snapshot, so changing it lands in
the **next release**, exactly as an environment variable's value does. What is
running keeps the file its release was built with until the next deploy — and
a rollback brings the old file back with the old image.

That last half needs a mechanism, and it is
[#288](https://github.com/Bermos/Kitchen/issues/288)'s: every workload's pod
template carries a digest of the plain files it reads
(`kitchen.bermos.dev/config-files-revision`). Without it, two releases
differing only in a file's content would produce identical pod templates — the
platform would rewrite the file and nothing would restart, so the rollback
would reach the next pod to start and not the ones already running.

A **secret** file's content is not in any Release — a Release is readable by
everyone who may read the project, and a credential is not — so replacing it
reaches what is already running: it is mounted from a Secret, and the digest
of the Secrets a workload reads already covers a file mounted from one. That
is the same bargain a variable bound to a secret makes, and a rollback
restores the declaration and today's content.

## What this does not do

**It does not prepare a volume.** Creating a directory tree, chowning it, or
merging a template into a file on first boot is
[#348](https://github.com/Bermos/Kitchen/issues/348), which survives this
feature rather than being solved by it. A configuration file is placed; it is
not a first run.

## Requirements

| Route | Requires |
|---|---|
| `GET /projects/{name}` — the declarations, and a plain file's content | `viewer` |
| `PATCH /projects/{name}` — declare, change or remove them | `admin` |
| `PUT /projects/{name}/files/{file}` — a secret file's content | `admin` |

**Admin for the content too, which is one rule rather than two.** A project
secret's value is a developer's because it is the day job — the thing they
were writing in cleartext until secrets existed. A configuration file is not:
the declaration is a project setting alongside the port and the workload list,
and a plain file's content travels on that same admin route. A content route
below that bar would let a developer replace the *secret* file and not the
plain one, which is an inversion nobody would choose on purpose.

## kitchen.json

A repository declares its files too, so a repository-backed project is not
left behind by a feature added for vendored ones — see
[CONFIG.md](../CONFIG.md). Two differences, and both follow from the file
being committed:

- **`content` is required there.** A commit has nowhere else to have put it.
- **`secret` is refused there**, by name, with a sentence rather than as an
  unknown field. Whether the platform holds a credential for a project is not
  something a pull request gets to declare — the same rule that stops a
  committed file pointing a variable at a Secret.

The file's declarations **merge onto the project's by name**, unlike its
process list and like its variables, and for the variables' reason: a project
may hold a secret file the repository may not declare, and a list that
replaced would take the declaration away and leave the application starting
without the credential file it is configured by. A name the file declares that
the project holds as a secret is refused outright rather than resolved either
way.

## CLI

`kitchen files list`, `kitchen files set` and `kitchen files rm` — see
[CLI.md](../CLI.md). The content of a config file is a *file*, which is the
one kind of value a terminal is better at than a form:

```sh
kitchen files set configuration --path /config/configuration.yaml \
  --content-file ./configuration.yaml
kitchen files set app-ini --path /data/gitea/conf/app.ini --secret \
  --content-file ./app.ini
kitchen files set configuration --no-path --content-file ./configuration.yaml
```
