# Kitchen — Connections

A Connection is a credential the platform holds on an installation's behalf.
The API never reads one back — writing a credential means the operator creates
the Secret from the request body, and no response echoes it.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Claims

Asking a connection for a resource is its own page: [Claims](claims.md) —
what a claim is, what each type takes, and the matrix of what every provider
declares about previews, idling and deploys.

## Connections

A connection is a plugin instance — a git provider, a registry, a database
provisioner — and its credential is the reason these endpoints are shaped the
way they are: **the API never reads credentials back.** Writing one means the
operator stores it in a Secret it manages, and every response is the same
credential-free view `GET` answers.

The providers are `github`, `gitlab`, `gitea`, `dockerRegistry`, `neon`,
`cnpg`, `s3`, `inngest`, `valkey` and `redis`. **`cnpg` and `valkey` are the two with no
credential at all** — they provision into this cluster with the operator's own
service account, so there is nothing to store, nothing to rotate, and a
`credential` sent for either is refused rather than kept and never read:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "postgres", "provider": "cnpg"}' \
  https://kitchen.apps.example.com/api/v1/connections
```

Its `config` is optional and is the operator's defaults for every claim through
it: `namespace`, `storageSize`, `storageClass`, `instances`, and `images` — the
catalogue of what this installation will run a database from, and what each one
promises a claim's extensions can come from. Testing it asks whether
CloudNativePG is serving here, which is the only question there is.

Every one of these is the operator's except the list, which answers **two
shapes**. A project cannot exist without a `gitSource` and a `registry`
connection to name, so a member who could not see that any connection exists
could not create a project — self-service would stop at the first form field
and hand them back to an operator. So `GET /connections` is filtered by role
rather than refused. An operator gets the connections:

```json
{"items": [{"name": "harbor", "provider": "dockerRegistry", "capabilities": ["imageStore"],
            "createdAt": "2026-03-01T09:00:00Z", "conditions": [{"…": "…"}]}]}
```

and everybody else gets the picker — the three things a dropdown needs, in a
shape of its own rather than the one above with fields blanked out:

```json
{"items": [{"name": "harbor", "capabilities": ["imageStore"], "ready": true}]}
```

`ready` is whether the platform has reached the provider and the provider
accepted the stored credential; one nothing has assessed yet reads `false`.
Between it and `capabilities`, a form can offer what can be chosen and say why
the rest cannot. Nothing else crosses: no provider, no `config`, and no
condition messages — those are the provider's own words about the operator's
credential, and belong on the operator's screen, which is where fixing them
lives too. `GET /connections/{name}` and every write below stay `operator`.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "gh", "provider": "github", "credential": {"token": "ghp_…"}}' \
  https://kitchen.apps.example.com/api/v1/connections
```

`github`, `gitlab`, `gitea`, `neon` and `inngest` authenticate with
`credential.token`.
A `dockerRegistry` takes `credential.username` and `credential.password`, plus
the registry in `config.url` — the prefix images are pushed under, whose host
is what builds authenticate against:

```json
{"name": "harbor", "provider": "dockerRegistry",
 "config": {"url": "harbor.example.com/kitchen"},
 "credential": {"username": "robot$kitchen", "password": "…"}}
```

`config` is the provider's own configuration and passes through as given — a
self-hosted forge names its API endpoint as `apiUrl`:
`{"apiUrl": "https://github.internal/api/v3"}` for GitHub Enterprise,
`{"apiUrl": "https://gitlab.internal/api/v4"}` for GitLab,
`{"apiUrl": "https://git.internal/api/v1"}` for Gitea. Left out, each falls
back to its hosted service — which is almost never what a Gitea connection
means.

A `github` token registers the repository's webhook, reads the repository, and
posts the commit status, the deployment and the pull-request comment. As a
fine-grained token that is **Contents: read-only**, **Webhooks: read and
write**, and **Commit statuses**, **Deployments** and **Pull requests: read and
write**; as a classic one the `repo` scope covers all of it, or `public_repo`
where every repository is public. A token short of the reporting permissions
still builds and deploys — the connection carries a warning saying what it
cannot post, and nothing goes red.

A `gitlab` or `gitea` token registers the repository's webhook and clones the
repository. GitLab takes a personal, project or group access token with the
`api` scope, held by someone with **Maintainer** on the project — registering
a hook is a maintainer's right. Gitea takes an access token with
`write:repository`, held by an owner or administrator of the repository.

Both report `gitSource` and `statusChecks`, so the build's check and the
preview's comment are posted back as GitHub's are. Two things they do not do:
Gitea keeps no deployment record — it has no such API, and GitLab's has no way
to retire one, so a removed preview is announced in the comment alone — and
neither enumerates repositories, so the repository is typed as `owner/name`
rather than picked from a list.

A `neon` credential is an API key that can create projects.

An `s3` connection is any S3-compatible object store — the MinIO the chart
runs when `objectStore.enabled` is set (the operator seeds this one, as
`kitchen-objectstore`), a MinIO a team already runs, AWS S3, Cloudflare R2.
It takes `credential.accessKeyId` and `credential.secretAccessKey`, and a
`config` naming where the store is and how it is talked to:

```json
{"name": "r2", "provider": "s3",
 "config": {"endpoint": "https://<account>.r2.cloudflarestorage.com", "region": "auto",
            "forcePathStyle": false, "scopedCredentials": false},
 "credential": {"accessKeyId": "…", "secretAccessKey": "…"}}
```

| Field | Default | What it does |
|---|---|---|
| `endpoint` | required | The store's URL, with its scheme |
| `region` | `us-east-1` | What buckets are created in and what every binding carries — a formality S3 clients insist on |
| `forcePathStyle` | `false` | Address a bucket as a path rather than a host name. MinIO needs it; AWS does not. It travels in every binding, so the application never guesses |
| `scopedCredentials` | `true` | Mint a user and a policy per bucket through the MinIO admin API, so no application is handed this key pair. Set it `false` for a store without that API — S3, R2 — and every claim is handed the connection's own credential; a `size` on a claim is then refused, since a quota is set through the same API |

A store that is kept at `scopedCredentials: true` and does not answer the
admin API is not a failed connection — the credential works — but every claim
through it will fail until the flag is set or the credential is given admin
rights, and the test below says so as a warning rather than a verdict.
Testing an `s3` connection lists the buckets the credential can see.
An `inngest` credential is an [Inngest Cloud API key](https://www.inngest.com/docs/platform/api-keys)
(`sk-inn-api-…`), which only an organization admin can create. The platform
reads each environment's signing key and event key into a claim's binding
through it, creates a branch environment per preview and archives it when the
pull request closes; it creates no keys, because the Inngest API cannot. Leave
the key unscoped, or scoped to the environment claims bind — a key scoped to
one environment reads nothing in the others, and previews need the branch
environments. It is validated with `GET /account`, and the connection reports
the `backgroundJobs` capability.

A `valkey` connection is the in-cluster cache: the operator runs Valkey under
its own service account, so it takes no credential. By default a claim through
it gets a **tenancy** in one of two servers the platform keeps — one evicting
for caches, one write-refusing for queues — and only a claim that asks for
something a shared server cannot give is run a server of its own. `docs/api/claims.md`
is where that resolution is written down.

Its `config` is optional and is the operator's defaults for every claim through
it: `namespace` (where the servers run, `kitchen-caches` by default),
`maxMemory` (the ceiling on a server one claim has to itself),
`sharedMaxMemory` (the ceiling on each shared server as a whole, `1Gi` by
default), `storageSize` and `storageClass` (the volume a `queue` gets, and the
one every shared server keeps its ACL users on), and `images`, the catalogue of
what this installation will run a cache from. Testing it asks nothing of a
provider, because there is none to ask: it is accepted, and the claims through
it report what they found.

A `redis` connection is a server somebody else runs — Upstash, ElastiCache,
Aiven, or a Valkey a team already has. Its whole credential is the URL, because
a Redis address carries its own password:

```json
{"name": "upstash", "provider": "redis",
 "credential": {"url": "rediss://:password@eu2-x.upstash.io:6379"}}
```

The scheme is `redis://` or `rediss://` and nothing else — `rediss` is the
encrypted one, and the binding tells the application which it got rather than
letting it guess. Its `config` says what the operator knows about the server
and the provisioner will not guess:

| Field | Default | What it does |
|---|---|---|
| `usage` | unset | What the server's `maxmemory-policy` is configured for: `cache` for an evicting server, `queue` for one that refuses writes when it is full. Left unset, a claim naming a `usage` is **refused** — a queue bound to an evicting server loses jobs silently, and that is the incident this contract exists to prevent |
| `databases` | `16` | How many logical databases the server offers. A claim gets one and every preview of it gets another, so a claim whose preview would run past the last is refused rather than bound to a keyspace the server rejects |

Testing it dials the server and authenticates — `PING`, and the server's own
`+PONG`. Both providers report the `cache` capability.

`POST /connections/test` runs that credential past the provider **without
storing anything**: no Secret is written and no connection is created, so a
token that turns out to be wrong leaves nothing to clean up. It takes the same
`provider`, `config` and `credential` a create does — or just the `name` of a
connection that exists, to re-check the credential already stored for it:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"provider": "github", "credential": {"token": "github_pat_…"}}' \
  https://kitchen.apps.example.com/api/v1/connections/test
{"reachable": true, "credentialChecked": true, "credentialValid": true,
 "message": "authenticated as octocat (token scopes: admin:repo_hook, repo)"}
```

The verdict comes in the same three parts the `Connected` and
`CredentialsValid` conditions are written from, because it is the same probe
the `ConnectionReconciler` runs: a provider that is down
(`reachable: false`), one that answered without ruling
(`credentialChecked: false` — including a provider the platform does not
implement yet), and one that refused the credential are three different
answers. `message` is the provider's own words and never contains the
credential. A malformed request — a provider nothing knows, a token provider
given a username — is a `400`.

A credential the provider accepted can still be short of a permission, which
comes back as `warnings` rather than as a failure, and rides along in the
`CredentialsValid` condition's message so an existing connection reports it
too:

```json
{"reachable": true, "credentialChecked": true, "credentialValid": true,
 "message": "authenticated as octocat (token scopes: admin:repo_hook)",
 "warnings": ["this token cannot post commit statuses on builds: add the repo:status scope"]}
```

That is read from GitHub's `X-OAuth-Scopes` header, which only a classic token
sends: a fine-grained token reports no permissions and GitHub offers no way to
ask, so it is never warned about — the form's guidance is what gets those
right.

`PATCH /connections/{name}` rotates the credential (`credential`), replaces
the config (`config`), or both; fields left out keep what is stored. The
provider is not editable — a connection to a different kind of system is a
different connection.

`DELETE /connections/{name}` refuses with `409` while any project or claim
still references the connection, naming what does. The stored credential is
deleted with it — but only when the platform wrote the Secret; a credential
something else manages (an Infisical sync, a hand-written manifest) is left
in place. Answers `204`.

### What a connection can see

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/connections/gh/repositories
{"provider": "github", "supported": true,
 "items": [{"fullName": "acme/shop", "defaultBranch": "main", "private": true,
            "description": "the shop"}]}
```

The repository field of the create-a-project form, answered from what the
connection's stored credential can already see — so a repository is chosen
from a list rather than spelled correctly from memory, and the project's
production branch starts as the one the provider calls default.

It is the **second route under `/connections` that is not the operator's**,
and for the same reason the list is not: creating a project is self-service,
and this is the field after the connection. It reads no credential back — the
token is used to ask the provider a question and never leaves the operator —
and it writes nothing.

Three things can happen instead of a listing, and all three answer `200`,
because none of them is a failure of the platform and each ends with somebody
who still has a repository to name:

| Answer | What it means |
|---|---|
| `"supported": false` | The provider has no listing behind it — today, that means `dockerRegistry`, `neon`, `inngest`, `valkey`, `redis`, `gitlab`, or `gitea`. `message` says which |
| `"truncated": true` | The credential can see more than the listing carries. It stops at 500, most recently pushed first |
| `502` | The provider refused or could not be reached; the body carries its own words — a token that has expired says so here |

A missing repository must never be indistinguishable from one that does not
exist, which is what `truncated` is for and why the dashboard's field still
takes a typed `owner/name` in every one of these cases.

The listing is what the token can reach — owned, shared, and through an
organisation it belongs to — which for a fine-grained token is exactly the
repositories it was granted. There is no CLI command for it: `kitchen api GET
/connections/gh/repositories` reaches it authenticated, and `kitchen projects
create` needs no picker — it takes the repository from the checkout it is run
in, or from `--repo`.

### What the platform makes of a repository

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"repo": "acme/shop", "ref": "main", "rootDirectory": "apps/shop"}' \
  https://kitchen.apps.example.com/api/v1/connections/gh/detect
{"detected": true, "framework": "vite", "strategy": "buildpacks", "port": 8080,
 "ref": "main", "rootDirectory": "apps/shop", "dockerfile": false,
 "files": ["package.json", "vite.config.ts"]}
```

The field after the repository, and the one worth being wrong about early: it
reads the repository exactly as a build with `strategy: auto` would — the same
`internal/detect` code the `BuildReconciler` calls, so the preflight cannot
disagree with the build — and says what the platform makes of it while the
build context is still a form field. Without it, a root directory one level
off is a build that fails several minutes after the project was created and
reads like the platform is broken.

Every field of the request is the value the form currently holds, and asking
again with a corrected `rootDirectory` or `dockerfilePath` is the whole of
fixing it. `ref` may be left out, in which case the repository is read at the
branch the provider calls its default and that branch is answered back in
`ref` — one extra request, and the reason `kitchen projects create` needs no
`--production-branch`. A provider the platform cannot ask for a default branch
is the one case a missing `ref` is refused, and the refusal says so.

Everything the caller can act on answers `200`, including the answers nobody
wants:

| Answer | What it means |
|---|---|
| `"detected": false` with `message` | The directory was read and not recognised, the root directory is not there, or the connection is not a source of repositories. `files` says what the verdict was reached from |
| `"unreadable": true` with `message` | The repository itself could not be read: it is not there, or this connection's credential cannot see it. The one verdict that is not about the build context |
| `400` | No `repo`, or no `ref` for a repository the provider names no default branch of |
| `502` | The provider refused or could not be reached |

`unreadable` is a separate answer because it used to be the same one. Every
provider answers `404` both for a path that is not in a repository and for a
repository a credential may not know about — GitHub deliberately, so that a
token cannot enumerate private repositories by reading status codes — so a
repository nobody could read was reported as `has no directory "." at main`,
which sends somebody to correct a root directory that is already correct. The
preflight asks the repository itself before it says anything about a
directory, and the message that comes back names the connection, because an
installation may have several and the fix is usually the credential of the one
that was asked.

`detected: false` is not an error and does not stop a project being created:
the build strategy can be set afterwards, and the build is still what decides.
`kitchen projects create` runs it before every create for that reason, and a
bare verdict is a question it asks rather than a refusal — `--yes` answers it,
and `kitchen api POST /connections/gh/detect` asks it on its own.

Both fields it exists to correct — `rootDirectory` and `dockerfilePath` — are
accepted by `POST /projects`, so a form that showed a wrong verdict can create
the project with the right build context rather than fixing it afterwards.
