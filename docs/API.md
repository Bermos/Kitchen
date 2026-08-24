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

**What that token may do is a project role, and nothing else.** A key belongs
to a machine account created for it, and the project it was made for holds a
grant for that account in `spec.access` — `developer` by default, which is the
day job: builds, promotions, rollbacks, environment variables, logs. So a key
is a member of exactly one project and has no platform surface at all: it can
trigger a build on `shop` and it cannot change the base domain, read another
project, or see that another project exists. Nothing about the role is stored
on the key, and there is no fourth role for machines — it is an ordinary grant
on an ordinary project, which is why a key can never outrank the project it was
made for. See [Keys for CI](api/projects.md#keys-for-ci) for issuing one, and
[AUTH.md](AUTH.md#machine-accounts) for why it is built this way.

**Revocation is at the issuer.** Deleting a key stops it working immediately,
and the operator has nothing to invalidate because it never held anything;
`DELETE /projects/{name}/keys/{key}` deletes it there and takes the grant off
the project in the same request.

## Authorization

A token says **who** the caller is. What they may do is Kitchen's own answer,
resolved from the access recorded on its objects. The model — two platform
roles, three project roles, `operator` containing `developer` — is
[AUTH.md, "Who may do what"](AUTH.md#who-may-do-what); it is not restated
here, only applied.

Two axes, because one list cannot answer both "may Anna change the base
domain?" and "may Anna deploy `billing`?":

- the **platform role**, exactly one per account, from `spec.access.operators`
  on the `Kitchen` singleton;
- the **project role**, per account per project, from `spec.access` on each
  `Project`. An operator holds `admin` on every project, present and future.

The `Requires` column on every endpoint below says which of the two it wants.
An unqualified `viewer`, `developer` or `admin` is a **project** role, on the
project the request is about — the path's for `/projects/{name}/…`, and
otherwise the object's own (`spec.projectRef` on a build, a release, an
environment or a claim; the environment's project for a domain). Three values
are not roles:

| Value | Means |
|---|---|
| `any account` | a valid token, and nothing more |
| `any account — filtered` | a valid token; the answer is narrowed to the projects the caller can see |
| `any account — body varies` | a valid token; the shape of the body depends on the caller's platform role, and any list inside it is narrowed to the projects they can see. Two routes: `GET /status` and `GET /connections` |

The whole table lives in one place in the operator — `internal/api/policy.go`,
which every route is registered from — so a route cannot exist without a
requirement, and the dashboard's copy of it is generated rather than written
twice.

### Being refused

**`403` names the role it wanted**, and what would have satisfied it:

```json
{"error": "you have viewer on shop; redeploying needs developer"}
```

```json
{"error": "changing the platform's settings needs the operator role; you are a member"}
```

**`404` is what an object you hold no role on looks like.** Not `403`: on a
platform where developers are not meant to see each other's projects, "you may
not know whether `billing` exists" is the right answer, and a refusal that
differed from a missing object would be the answer to the question it was
withholding. So a build of somebody else's project, a `?project=` naming one,
and a saved query that mentions one are all answered exactly as if they were
not there. A `403` therefore always means *you can see this, and you may not do
that* — which is what makes it worth naming a role in.

## Endpoints

All paths are relative to `/api/v1`. Collections answer `{"items": [...]}`;
errors answer `{"error": "..."}` with a message meant to be read by whoever
sent the request. The `Requires` column is explained under
[Authorization](#authorization) above.

**Every route here is also reachable from a terminal.** The `kitchen` CLI
([CLI.md](CLI.md)) wraps the common ones as commands and reaches the rest with
`kitchen api METHOD PATH`, so a route added below is usable from the command
line the day it lands. A route added, renamed, or given a different requirement
is therefore a decision about the CLI as well — add a command or leave it to
`kitchen api`, but decide. The CLI's own tests check every endpoint its commands
name against `internal/api/policy.go`, so a route that moves fails them too.

| Method | Path | Does | Requires |
|---|---|---|---|
| GET | `/projects` | List projects | any account — filtered |
| POST | `/projects` | Create a project | any account |
| GET | `/projects/{name}` | One project — its env vars by name, never their values | `viewer` |
| PATCH | `/projects/{name}` | Change its settings — branch, previews, build, runtime. Not its env vars | `admin` |
| PATCH | `/projects/{name}/env` | Change its environment variables — the whole list | `developer` |
| DELETE | `/projects/{name}` | Delete it, and everything derived from it | `admin` |
| GET | `/projects/{name}/builds` | That project's builds, newest first | `viewer` |
| POST | `/projects/{name}/builds` | Build a commit — a rebuild | `developer` |
| GET | `/projects/{name}/releases` | That project's releases, newest first | `viewer` |
| GET | `/projects/{name}/environments` | That project's environments | `viewer` |
| GET | `/projects/{name}/members` | Who holds a role on it — the readable form of `spec.access` | `viewer` |
| POST | `/projects/{name}/members` | Give somebody a role. The address is resolved to a `sub` before it is written | `admin` |
| PATCH | `/projects/{name}/members` | Move a member to another role | `admin` |
| DELETE | `/projects/{name}/members` | Take a member off the project | `admin` |
| GET | `/projects/{name}/keys` | That project's CI keys — never their values | `viewer` |
| POST | `/projects/{name}/keys` | Issue one. The key is answered once, and the grant is written with it | `admin` |
| DELETE | `/projects/{name}/keys/{key}` | Revoke one, and take its grant off the project | `admin` |
| GET | `/builds` | Every build. `?project=` filters | any account — filtered |
| GET | `/builds/{name}` | One build | `viewer` |
| POST | `/builds/{name}/cancel` | Stop it — the Build stays, phase `Cancelled` | `developer` |
| GET | `/builds/{name}/logs` | That build's output | `viewer` |
| GET | `/builds/{name}/attestations` | The signed evidence attached to that build's artifact | `viewer` |
| POST | `/builds/{name}/gates` | Submit a quality gate result produced elsewhere | `developer` |
| GET | `/builds/{name}/vex` | The artifact's OpenVEX statements, joined to the findings they modify | `viewer` |
| POST | `/builds/{name}/vex` | Attach an OpenVEX document to that build's artifact | `admin` |
| GET | `/releases` | Every release. `?project=` filters | any account — filtered |
| GET | `/releases/{name}` | One release | `viewer` |
| GET | `/projects/{name}/promotions` | That project's promotions, newest first. `?environment=`, `?release=`, `?phase=` | `viewer` |
| POST | `/projects/{name}/promotions` | Ask for a release to land on an environment; the policy decides | `developer` |
| GET | `/promotions/{name}` | One promotion: the phase, the verdict, and the unmet rules by id | `viewer` |
| POST | `/projects/{name}/exceptions` | Request a break-glass exception; the escalation ladder decides who must approve | `developer` |
| GET | `/exceptions` | The exception register, soonest to expire first. `?project=`, `?environment=`, `?historical=true` | any account — filtered |
| GET | `/exceptions/{name}` | One exception whole: the grant, and every promotion that relied on it | `viewer` |
| PATCH | `/exceptions/{name}` | Resolve it, with a reason on the record | `admin` |
| GET | `/environments` | Every environment. `?project=` filters | any account — filtered |
| GET | `/environments/{name}` | One environment | `viewer` |
| PATCH | `/environments/{name}` | Move it to another release — promotion and rollback. An environment with requirements answers `202` with the Promotion the move became | `developer` |
| DELETE | `/environments/{name}` | Tear down a stuck preview. Previews only | `developer` |
| GET | `/environments/{name}/logs` | That environment's runtime logs | `viewer` |
| GET | `/environments/{name}/workload` | What it is running: replicas, restarts, uptime, resources, pods | `viewer` |
| GET | `/environments/{name}/metrics` | What it *has been* running: CPU, memory, replicas and restarts over a window | `viewer` |
| GET | `/environments/{name}/requests/summary` | The golden-signal header: traffic, error rate and latency over a window | `viewer` |
| GET | `/environments/{name}/requests/series` | The same signals over time — the charts | `viewer` |
| GET | `/environments/{name}/requests/routes` | One row per route template, sortable — the per-path breakdown | `viewer` |
| GET | `/environments/{name}/requests` | The requests themselves, newest first. Filterable, and live-tails like logs | `viewer` |
| GET | `/environments/{name}/diagnostics` | The crash report: everything about the last abnormal termination, assembled | `viewer` |
| GET | `/environments/{name}/signals` | What is wrong with it right now — the diagnostics strip | `viewer` |
| PATCH | `/environments/{name}/requirements` | Change the bar it sets — the policy bundle, its parameters, its owners | `viewer` to reach; the handler admits only `spec.owners` and operators |
| GET | `/environments/{name}/eligibility` | How a release measures up against that bar, from stored evidence alone | `viewer` |
| GET | `/environments/{name}/objects` | The Kubernetes objects the operator materialized for it | `operator` |
| GET | `/logs` | The whole logs table, filtered by a query. `?q=`, `?where=` | any account — filtered |
| GET | `/logs/histogram` | The same selection counted over time — the shape of the window | any account — filtered |
| GET | `/logs/facets` | The same selection's distinct values per field, with counts | any account — filtered |
| GET | `/logs/patterns` | The same selection's messages collapsed into templates | any account — filtered |
| GET | `/logs/saved` | Saved queries — selections someone kept under a name | any account — filtered |
| POST | `/logs/saved` | Keep the current selection under a name | any account |
| DELETE | `/logs/saved/{name}` | Forget one | any account — filtered |
| GET | `/events` | The platform's recent activity, newest first. `?project=` and `?limit=` filter | any account — filtered |
| GET | `/audit` | The tamper-evident log of state transitions. `?kind=`, `?name=`, `?project=`, `?actor=`, `?privileged=true`, `?privilegeClass=`, `?since=`, `?until=`, `?limit=` | any account — filtered |
| GET | `/audit/verify` | Re-derive the chain's hashes over a run and report every break. `?from=`, `?limit=` | `operator` |
| GET | `/compliance` | What the platform is producing: whether the audit log is recording, decisions are stored, and the key artifacts are signed under | `operator` |
| GET | `/compliance/inventory` | Every environment and claim with its data class, provenance and residency — the classification inventory, exportable in one request | any account — filtered |
| GET | `/compliance/drift` | Deployed releases measured against their environment's bar today: what is running that no longer meets it, and whether each rule started failing after promotion or was waived there. `?project=`, `?environment=`, `?all=true` | any account — filtered |
| GET | `/compliance/criticality` | The function-to-resource mapping: every designated function with the environments, releases, claims, connections, domains and third parties behind it. `?criticality=` narrows to a designation and worse, `?project=` to one | any account — filtered |
| GET | `/compliance/dependents` | The reverse query: which environments break if one connection, or one third party, is unavailable — with their designations and the tightest RTO among them. `?connection=` or `?provider=`, exactly one | any account — filtered |
| GET | `/decisions` | Stored policy decisions, newest first. `?project=`, `?environment=`, `?release=`, `?verdict=`, `?kind=`, `?since=`, `?until=`, `?limit=` | any account — filtered |
| GET | `/decisions/{id}` | One decision whole, with the full input it can be replayed from | any account — filtered |
| POST | `/decisions/{id}/replay` | Re-evaluate it from its stored inputs and compare the verdicts | `developer` on the decision's project, enforced by the handler |
| GET | `/policy/bundles` | The policy bundles available to require: digest, source, rule ids | `operator` |
| GET | `/metrics/overview` | The dashboard's numbers, pre-aggregated. `?project=` narrows | any account — filtered |
| GET | `/traffic` | The service map: aggregated flow edges. `?project=`, `?since=`, `?until=` | any account — filtered |
| GET | `/traces` | Traces in a window. `?project=`, `?environment=`, `?service=`, `?errors=1`, `?minDuration=` | any account — filtered |
| GET | `/traces/{traceId}` | One trace's spans, oldest first — the waterfall | any account — filtered |
| GET | `/me` | Who the caller is: subject, address, name and platform role | any account |
| GET | `/status` | The platform as it is running: cluster, tunnel, build queue, components | any account — body varies |
| GET | `/platform/signals` | Every finding firing anywhere on the platform, worst first — the problems list | `operator` |
| GET | `/platform/nodes` | Per node: conditions, pods, and when its collector last shipped anything | `operator` |
| GET | `/platform/workloads` | Every workload and pod on the platform — and the workloads with no pods at all | `operator` |
| GET | `/platform/edge` | Cross-project traffic, the Gateway, the tunnel and the certificates | `operator` |
| GET | `/platform/storage` | Volumes and what mounts them, plus the telemetry store's own health | `operator` |
| GET | `/platform/events` | The cluster's Warning history, faceted. `?reason=`, `?kind=`, `?node=`, `?search=` | `operator` |
| GET | `/platform/ingest` | Collector presence and freshness, and what the flow follower lost | `operator` |
| GET | `/platform/backup` | What an export would carry, what it would not, and whether this cluster can snapshot volumes | `operator` |
| POST | `/platform/backup` | Export the platform's state as one gzipped tar | `operator` |
| GET | `/settings` | The platform's settings — the `Kitchen` singleton, operator list included | `operator` |
| PATCH | `/settings` | Change the build and telemetry defaults, or who the operators are | `operator` |
| GET | `/updates` | The platform's own version, what it can upgrade to, and every upgrade it has attempted. `?refresh=true` asks the registry again | `operator` |
| POST | `/updates` | Upgrade the platform | `operator` |
| GET | `/updates/{name}` | One upgrade | `operator` |
| GET | `/updates/{name}/logs` | One upgrade's helm output. Streams with `Accept: text/event-stream` | `operator` |
| GET | `/connections` | An operator: every connection (never their credentials). Anybody else: the picker — name, capabilities, readiness | any account — body varies |
| GET | `/connections/{name}/repositories` | What this connection's credential can see, for the repository field of the create-a-project form | any account |
| POST | `/connections/{name}/detect` | What the platform makes of a repository, read the way a build would, before the project exists | any account |
| POST | `/connections` | Create one — the credential goes in, and never comes back out | `operator` |
| POST | `/connections/test` | Try a credential against its provider, storing nothing | `operator` |
| GET | `/connections/{name}` | One connection | `operator` |
| PATCH | `/connections/{name}` | Rotate the credential, change the config, or both | `operator` |
| DELETE | `/connections/{name}` | Delete it, unless something still uses it | `operator` |
| GET | `/domains` | Every custom domain. `?environment=` filters | any account — filtered |
| POST | `/domains` | Attach one — the response carries the DNS record to create | `developer` |
| GET | `/domains/{name}` | One domain, verification instructions included | `viewer` |
| DELETE | `/domains/{name}` | Detach it; the operator removes its certificate | `developer` |
| GET | `/claims` | Every resource claim. `?project=` filters | any account — filtered |
| POST | `/claims` | Ask for a provisioned resource: a database from a connection, or an OAuth client from the platform's identity provider | `developer` |
| GET | `/claims/{name}` | One claim | `viewer` |
| DELETE | `/claims/{name}` | Delete it — what happens to the data is its `deletionPolicy`'s call; an OAuth client is always deregistered | `developer` |

## Endpoint reference

What to send each route, what comes back, and why it answers the way it
does — one file per resource.

They are separate files because a single page was the most conflicted file
in this repository: every change that added an endpoint appended a section
to the same 2000-line document, so two unrelated features could not be
written at the same time without colliding. One file per resource makes two
such changes two changes to two different files.

- [Accounts](api/accounts.md) — who the caller is, and what they may do
- [Projects](api/projects.md) — settings, environment variables, membership, CI keys, and deletion
- [Builds](api/builds.md) — starting and cancelling one, what it reused, and the evidence it left
- [Environments and releases](api/environments.md) — rolling back, what is running, what is wrong with it, and the bar an environment sets
- [Connections and claims](api/connections.md) — the credentials the platform holds, and asking one for a resource
- [Custom domains](api/domains.md) — putting an environment on an address of its own
- [Logs and queries](api/logs.md) — reading them, following them live, querying them, and saving a query
- [Metrics, traffic and traces](api/telemetry.md) — the golden signals, the request rows behind them, and the spans
- [The activity feed and the audit log](api/audit.md) — what the platform did, best-effort and tamper-evident
- [Policy decisions](api/decisions.md) — every verdict the policy engine reached, the bundles it evaluates, replaying a decision, and the drift view over them
- [Promotions](api/promotions.md) — the staged pipeline: asking for a release to land, and what the policy decided
- [Exceptions](api/exceptions.md) — break-glass: bounded, two-person, per-rule waivers, and the register of every one
- [Criticality and disruption tolerance](api/criticality.md) — designating a function, the map of everything supporting it, and what breaks when a third party is unavailable
- [Platform status and the operator's screens](api/platform.md) — whether the platform is healthy, and everything behind /platform
- [Settings and updates](api/settings.md) — the installation's own configuration, and moving it to a new version

## Status codes

| Code | When |
|---|---|
| `200` / `201` | Fine |
| `202` | Accepted — the operator's finalizers finish the work after the response |
| `204` | Deleted, nothing left to describe |
| `400` | The request cannot be carried out as written |
| `401` | No valid token — including when the platform has no identity provider |
| `403` | Your account may not do this — the message names the role it wanted. Also the operator's own service account being refused by the cluster |
| `404` | No such object, or no such endpoint |
| `409` | Someone else changed the object first, it already exists, it already finished, or something still uses it |
| `503` | A capability this endpoint needs is not installed |

## Decisions

| Decision | Choice | Why |
|---|---|---|
| Token validation | Stateless, against the issuer's JWKS | No session state in the operator; the identity provider stays swappable |
| Token audience | The API's own URL (`resource=`), or the issuer | A resource server should be able to tell a token meant for it from a token meant for everything |
| CI tokens | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's own credential; keeping key lookup at the issuer keeps the operator's request path stateless |
| What a CI key may do | A project role on a machine account created for the key, in the same `spec.access` as everybody else's | A key that carried permissions of its own would be a second permission system; a grant on the project means a key cannot outrank the project it was made for, and revocation stays at the issuer |
| Response shapes | The API's own vocabulary, not raw custom resources | A stable contract for the UI, and freedom to change how state is stored |
| Write surface | The full project, connection and claim lifecycle, rebuild and cancel, promote/rollback, preview teardown, the settings' runtime defaults, and the platform's operator list | Nothing a user does in the platform's normal running should need `kubectl`; domain writes wait for their reconciler, because a write over objects nothing reconciles only looks like it works |
| Environment variables | A route of their own, `PATCH /projects/{name}/env`, rather than a field the settings route lets a developer through for | A whole route is the unit of authorization; a handler that decided by which key the body carried would apply the response-body exception to a write, and a dropped `env` would be a lost write that read as a successful one |
| The operator list | On `GET`/`PATCH /settings`, which are already operator-only, rather than a surface of its own | It is the platform's own access list, like the base domain and the issuer beside it; a list that is enforced against and seeded on upgrade but served by nothing is one somebody opens `kubectl` to read |
| Credentials | Write-only: the operator stores them in Secrets and never echoes them | "Credentials never leave the operator" survives the API growing a write surface |
| Introspection shapes | Kubernetes' own vocabulary — replicas, restarts, manifests | The exception that proves the rule above: these endpoints exist to explain the platform's machinery, and a reader comparing them against `kubectl` should not have to translate |
| Empty request surfaces | `edge.routed` beside the numbers, on every request answer | "Nothing reaches this environment through the edge" and "nothing was asked of it in this window" are both zeroes and different sentences; four empty charts would describe the platform rather than the application |
| The crash report | One endpoint that joins five sources, all-or-nothing | Troubleshooting should be reading rather than hunting, and a section that failed silently would be read as evidence that nothing happened |
| Operator screens | Their own `/platform/` prefix, never a parameter on a project-scoped endpoint | An operator-only surface separable by path is one row of the enforcement table rather than an audit of every handler |
| Authorization | One table every route is registered from, in `internal/api/policy.go` | A route cannot be added without a requirement, and the dashboard's copy of the rules is generated from it rather than written twice |
| Roles on the wire | The role itself on each project payload, not capability booleans | What a client may offer is derived from the same table the API enforces; a second vocabulary would be a second opinion, and they would drift |
| Signals | Evaluated when a screen asks, and answered with what could *not* be read | Findings are ephemeral until a background loop records them, and an empty problems list is the strongest claim the platform makes — it may only be made about inputs that were actually read |
| Pods and nodes | Read uncached, straight from the API server | Serving them from the manager's cache would mean an informer over every pod in the cluster, kept warm for a question only an open dashboard asks |
| The dashboard | Served by the same process, outside `/api/` | The SPA is public, stateless files plus `/config.json` (issuer, client id, audience — the same values every login redirect shows); everything with state stays behind the token check |
| OTLP ingest | The node collector's own unauthenticated in-cluster port, never on the Gateway | Spans come from workloads already inside the cluster; an OTLP endpoint on the public Gateway would be an unauthenticated write surface on the telemetry store |
| Saved queries | A `SavedQuery` object with no reconciler | The rule that a write waits for its reconciler is about objects that do nothing until something acts on them; a saved query has its whole effect by existing |
| Webhook receiver | Stays signature-authenticated, not OIDC | A provider proving a payload is genuine is a different question from a caller proving who they are |
| The CLI | A client of this API in the same repository, with no surface of its own | Everything `kitchen` does is a route the dashboard uses too; it ships here so one tag versions it with the chart and both images, and so its tests can check the endpoints it names against the enforcement table |

## Open

- **Teams.** Access is per account, on the object it is about: a role for every
  member of a team is that many entries. Per-organisation roles would land with
  the identity provider's organizations plugin — and would widen the contract
  this API rests on, so they wait until somebody asks. Nothing about the token
  changes either way: it says who the caller is, and Kitchen says what they may
  do (see [Authorization](#authorization)).
- **Paging.** Collections answer in full. `{"items": …}` is an object rather
  than a bare array so a cursor can be added without breaking clients.
