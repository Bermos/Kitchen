# Kitchen — Operator REST API

The surface the Kitchen UI, the CLI and CI talk to, served by the operator
alongside the git webhook receiver and published on the shared Gateway at
`https://kitchen.<baseDomain>/api/v1/`.

It is a view onto the same custom resources the controllers reconcile — there
is no second copy of the platform's state. Listing projects reads `Project`
objects; rolling an environment back writes one field of an `Environment`.

## Authentication

Every endpoint is behind the platform's identity provider ([AUTH.md](AUTH.md)).
There is no unauthenticated mode, no local-admin escape hatch, and no
read-only exception: a request without a valid bearer token gets `401` with a
`WWW-Authenticate: Bearer` challenge. An installation running with
`auth.enabled=false` has no issuer, so every endpoint answers 401 — the API
does not fall open when the thing that guards it is missing.

Validation is stateless. The operator fetches the issuer's JWKS once, verifies
the signature, the issuer, the expiry and the audience, and keeps no session.
Key rotation needs no restart: an unknown key id refetches the JWKS.

Both the issuer and the accepted audiences come from the `Kitchen` singleton,
so nothing has to be configured twice:

| | |
|---|---|
| Issuer | `spec.auth.host`, defaulting to `auth.<baseDomain>` |
| Accepted audiences | the issuer, and `spec.api.externalURL` (defaulting to `https://kitchen.<baseDomain>`) |

`--api-audiences` adds more, for installations that mint tokens under another
name.

### Getting a token

**A person, through the UI.** The UI is an OAuth client (Authorization Code +
PKCE, client id `kitchen-ui`), registered by the chart on the first start of
the auth service. It asks the token endpoint for a token for the API by name:

```
POST https://auth.<baseDomain>/oauth2/token
grant_type=authorization_code&client_id=kitchen-ui&code=…&code_verifier=…
&resource=https://kitchen.<baseDomain>
```

The `resource` parameter is what makes the access token a JWT with the API as
its audience. Without it the provider issues an opaque token, which the
operator cannot validate and will refuse.

**CI, with an API key.** API keys are the identity provider's (better-auth's
api-key plugin) — see the decision below. The key is exchanged for a
short-lived JWT at the issuer, and the API sees only the JWT:

```sh
TOKEN=$(curl -sS -H "x-api-key: $KITCHEN_API_KEY" \
  https://auth.apps.example.com/token | jq -r .token)

curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects
```

That token's audience is the issuer, which the API accepts. The key itself
never reaches the operator, so a leaked API key is revoked in one place and
the operator has nothing to invalidate.

## Endpoints

All paths are relative to `/api/v1`. Collections answer `{"items": [...]}`;
errors answer `{"error": "..."}` with a message meant to be read by whoever
sent the request.

| Method | Path | Does |
|---|---|---|
| GET | `/projects` | List projects |
| POST | `/projects` | Create a project |
| GET | `/projects/{name}` | One project |
| GET | `/projects/{name}/builds` | That project's builds, newest first |
| POST | `/projects/{name}/builds` | Build a commit — a rebuild |
| GET | `/projects/{name}/releases` | That project's releases, newest first |
| GET | `/projects/{name}/environments` | That project's environments |
| GET | `/builds` | Every build. `?project=` filters |
| GET | `/builds/{name}` | One build |
| GET | `/builds/{name}/logs` | That build's output |
| GET | `/releases` | Every release. `?project=` filters |
| GET | `/releases/{name}` | One release |
| GET | `/environments` | Every environment. `?project=` filters |
| GET | `/environments/{name}` | One environment |
| PATCH | `/environments/{name}` | Move it to another release — promotion and rollback |
| GET | `/environments/{name}/logs` | That environment's runtime logs |
| GET | `/logs` | The whole logs table, filtered by a ClickHouse expression |
| GET | `/settings` | The platform's settings — the `Kitchen` singleton |
| PATCH | `/settings` | Change the build and telemetry defaults |
| GET | `/connections` | Every connection (never their credentials) |
| GET | `/connections/{name}` | One connection |
| GET | `/domains` | Every custom domain. `?environment=` filters |
| GET | `/domains/{name}` | One domain |
| GET | `/claims` | Every resource claim. `?project=` filters |
| GET | `/claims/{name}` | One claim |

Creating connections, domains and claims is not here yet: those are the flows
the UI will drive, and they are worth designing against a UI rather than ahead
of one. Until then they are `kubectl apply`, the same as they were.

### Creating a project

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop", "repo": "acme/shop", "connection": "gh", "registry": "harbor"}' \
  https://kitchen.apps.example.com/api/v1/projects
```

A project is a name, a repository in the provider's `owner/name` form, and the
two Connections it builds and stores images with — `connection` needs the
`gitSource` capability, `registry` needs `imageStore`. Optional fields with
their defaults:

```json
{"productionBranch": "main", "previews": true}
```

The name has to work as a DNS label of at most 46 characters, because
everything the platform derives from it — the application namespace, release
names, generated hostnames — has to fit Kubernetes' 63-character limit.
Naming a Connection that does not exist, or one without the needed
capability, is a `400`; a Connection the operator has not assessed yet is
accepted, and the project's own conditions report whether it fits. A name
already in use is a `409`.

Answers `201` with the new project. The operator takes it from there:
namespace, webhook, and — once the first build of the production branch
lands — the production environment.

### Triggering a build

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/builds
```

An empty body rebuilds the commit the project built last — a rerun after a
flaky build or a changed secret. To build a particular commit:

```json
{"sha": "abc123def456789", "branch": "main"}
```

The branch may be left out for a commit that has been built before; for one
that has not, it falls back to the project's production branch. Builds are
immutable, so a rebuild is always a new `Build` with a generated name
(`shop-bld-abc123def456-xk2p9`) rather than a mutation of the old one.

Answers `201` with the new build.

### Rolling back

Rollback is not a special operation. A `Release` is an immutable snapshot of an
image digest and the configuration it runs with, so pointing an `Environment`
at an older one puts back exactly what was running:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"release": "shop-rel-41"}' \
  https://kitchen.apps.example.com/api/v1/environments/shop-production
```

The release has to belong to the same project as the environment; anything else
is a `400`. Promotion is the same call with a newer release.

Each move is remembered. The environment's `history` lists the releases that
stopped being current, newest first: which release, when it was current
(`from`/`to`), how it stopped (`reason`) and who moved the environment off it
(`by` — the authenticated caller for API moves, the promoting build for
automatic ones):

```json
{"history": [{"release": "shop-rel-42", "from": "2026-08-13T09:12:00Z",
  "to": "2026-08-14T10:30:00Z", "reason": "rolledBack", "by": "ada@example.com"}]}
```

`reason` is `promoted` when a fresh build's release was auto-promoted over it,
`rolledBack` when the environment was moved back to an older release, and
`superseded` when another release replaced it any other way.

### Settings

`GET /settings` is the `Kitchen` singleton as a view: the base domain, the
derived API and issuer URLs, the gateway's address and conditions, and the
defaults the platform builds and retains telemetry with.

`PATCH /settings` changes the fields that are safe to change at runtime:

```json
{"buildStrategy": "auto", "buildConcurrency": 2, "logRetentionDays": 30}
```

Fields left out stay as they are. Everything else on the singleton — the base
domain, the issuer, the ingress — shapes URLs and credentials the platform has
already handed out, so changing those stays a deliberate kubectl operation.

### Logs

Build and runtime logs come from ClickHouse, where the collector has been
shipping them since the log pipeline landed — so a build's output survives the
build pod, and a preview's logs outlive the preview.

```
GET /builds/{name}/logs?limit=200&search=error&since=2026-08-13T10:00:00Z
GET /environments/{name}/logs?limit=200&container=app
```

| Parameter | Meaning |
|---|---|
| `limit` | Lines to return, default 200, capped at 5000. The *newest* lines are kept |
| `since` / `until` | RFC 3339 bounds |
| `search` | Case-insensitive substring of the message |
| `container` | One container of the pod |

Lines come back oldest first — a log reads forwards — as
`{timestamp, source, project, environment, build, pod, container, stream, message}`.

An installation without a telemetry store answers `503`: there are no logs to
read, which is a missing capability rather than a bad request.

### Querying logs with ClickHouse syntax

The logs live in ClickHouse, and the API does not pretend otherwise. `/logs`
takes a real ClickHouse boolean expression over the table's columns and
evaluates it as written — the observability view's query bar is this endpoint:

```
GET /logs?where=project = 'shop' AND stream = 'stderr' AND message ILIKE '%timeout%'
GET /logs?where=match(message, 'GET /works\?page=\d+') AND environment = 'shop-production'
```

`limit`, `since` and `until` work as above and are applied on top of the
expression; `where=1 = 1` selects everything in the window. A refused
expression — a typo, an unknown column — answers `400` carrying ClickHouse's
own diagnostic, which is the message that says how to fix it.

The expression reaches ClickHouse as query text, which is the point — and why
it runs pinned read-only (`readonly=2`: no writes, no DDL) under an execution
cap, as the operator's own database user. What that user can read is the
telemetry database; per-caller scoping arrives with scopes and RBAC
([open item](#open)).

## Status codes

| Code | When |
|---|---|
| `200` / `201` | Fine |
| `400` | The request cannot be carried out as written |
| `401` | No valid token — including when the platform has no identity provider |
| `403` | The operator's own service account may not do this |
| `404` | No such object, or no such endpoint |
| `409` | Someone else changed the object first |
| `503` | A capability this endpoint needs is not installed |

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Token validation | Stateless, against the issuer's JWKS | No session state in the operator; the identity provider stays swappable |
| Token audience | The API's own URL (`resource=`), or the issuer | A resource server should be able to tell a token meant for it from a token meant for everything |
| CI tokens | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's own credential; keeping key lookup at the issuer keeps the operator's request path stateless |
| Response shapes | The API's own vocabulary, not raw custom resources | A stable contract for the UI, and freedom to change how state is stored |
| Write surface | Create project, rebuild, promote/rollback, and the settings' runtime defaults | The writes the UI drives today; creating connections, domains and claims waits for the flows they belong to |
| The dashboard | Served by the same process, outside `/api/` | The SPA is public, stateless files plus `/config.json` (issuer, client id, audience — the same values every login redirect shows); everything with state stays behind the token check |
| Webhook receiver | Stays signature-authenticated, not OIDC | A provider proving a payload is genuine is a different question from a caller proving who they are |

## Open

- **Scopes and RBAC.** Tokens carry their scopes and the API records who asked
  for what, but nothing is enforced beyond "the issuer vouches for you". Teams
  and per-organisation roles land with the organizations plugin, and the token
  shape follows them.
- **Streaming logs.** Log reads are bounded queries. Following a build live
  wants Server-Sent Events on the same endpoints.
- **Paging.** Collections answer in full. `{"items": …}` is an object rather
  than a bare array so a cursor can be added without breaking clients.
