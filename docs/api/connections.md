# Kitchen — Connections and claims

A Connection is a credential the platform holds on an installation's behalf.
The API never reads one back — writing a credential means the operator creates
the Secret from the request body, and no response echoes it.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Creating a claim

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-db", "project": "shop", "connection": "neon", "type": "postgres", "previewBranching": true}' \
  https://kitchen.apps.example.com/api/v1/claims
```

A claim asks for something the project needs; the reconciler writes the
credentials into a binding secret that `Project.spec.env`'s `fromClaim`
references, and the API never reads them back. There are two `type`s, and each
is refused the other's fields rather than having them ignored:

**`postgres`** asks a Connection with the `database` capability to provision a
database. `previewBranching` gives every preview environment its own database
branch. `deletionPolicy` (`Retain`, the default, or `Delete`) decides what
deleting the claim later does to the provisioned database — `Retain` is the
default because destroying data has to be asked for, never implied.

**`oidcClient`** asks the platform's own identity provider for an OAuth
client, so that the application signs its users in with the same accounts as
the dashboard:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-auth", "project": "shop", "type": "oidcClient"}' \
  https://kitchen.apps.example.com/api/v1/claims
```

It takes **no `connection`** — the provider is the issuer the platform is
already configured with — and no `deletionPolicy`, because its client is
always deregistered with the claim. Three optional fields shape it, and the
answer carries all three with the platform's defaults filled in, so a claim
never reports "unset" for something it does have an answer to:

| Field | Default | What it does |
|---|---|---|
| `callbackPaths` | `["/auth/callback", "/api/auth/callback/kitchen"]` | Appended to every URL the project's environments are reachable at |
| `redirectURIs` | none | Registered verbatim, for addresses the platform does not own — `http://localhost:3000/auth/callback` |
| `scopes` | `["openid", "profile", "email", "offline_access"]` | What the client may ask the issuer for; `openid` is required |

`redirectURIs` in the *answer* is a different thing from the one in the
request: it is what the client currently accepts, which the operator keeps in
step with the project's environments as previews come and go. It is the one
part of that automation anybody can check.

Deleting a claim answers `202`: the operator's finalizer still has branches,
binding secrets, the registered client and — under `Delete` — the database
itself to remove.

## Connections

A connection is a plugin instance — a git provider, a registry, a database
provisioner — and its credential is the reason these endpoints are shaped the
way they are: **the API never reads credentials back.** Writing one means the
operator stores it in a Secret it manages, and every response is the same
credential-free view `GET` answers.

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

`github`, `gitlab`, `gitea` and `neon` authenticate with `credential.token`.
A `dockerRegistry` takes `credential.username` and `credential.password`, plus
the registry in `config.url` — the prefix images are pushed under, whose host
is what builds authenticate against:

```json
{"name": "harbor", "provider": "dockerRegistry",
 "config": {"url": "harbor.example.com/kitchen"},
 "credential": {"username": "robot$kitchen", "password": "…"}}
```

`config` is the provider's own configuration and passes through as given — a
self-hosted GitHub names its API endpoint as `{"apiUrl": "https://github.internal/api/v3"}`.

A `github` token registers the repository's webhook, reads the repository, and
posts the commit status, the deployment and the pull-request comment. As a
fine-grained token that is **Contents: read-only**, **Webhooks: read and
write**, and **Commit statuses**, **Deployments** and **Pull requests: read and
write**; as a classic one the `repo` scope covers all of it, or `public_repo`
where every repository is public. A token short of the reporting permissions
still builds and deploys — the connection carries a warning saying what it
cannot post, and nothing goes red. A `neon` credential is an API key that can
create projects.

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
