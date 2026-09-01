# Kitchen — Claims

A claim asks the platform for something the project needs and does not want
to install itself — a database, single sign-on — and binds it into the
project's environments. The credentials it produces stay in the cluster:
the API hands out the claim's status, never the secret's contents.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to. The
credentials a claim provisions through are [Connections](connections.md).

## Creating a claim

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-db", "project": "shop", "connection": "neon", "type": "postgres"}' \
  https://kitchen.apps.example.com/api/v1/claims
```

A claim asks for something the project needs; the reconciler writes the
credentials into a binding secret that `Project.spec.env`'s `fromClaim`
references, and the API never reads them back. The `type`s are the rows of
[what can be claimed](#what-can-be-claimed), and each is refused every other
type's fields rather than having them ignored:

**`postgres`** asks a Connection with the `database` capability to provision a
database. `deletionPolicy` (`Retain`, the default, or `Delete`) decides what
deleting the claim later does to the provisioned database — `Retain` is the
default because destroying data has to be asked for, never implied. What a
preview environment gets is the provider's declaration, below, unless the
claim says otherwise with `previewMode`.

`postgres` is the block that says *which* Postgres, because "Postgres" is not
one thing: an application that needs PostGIS, pgvector or a time-series
extension otherwise binds, gets a URL, and dies on a `CREATE EXTENSION` in its
first migration.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "maps-db", "project": "maps", "connection": "postgres", "type": "postgres",
       "postgres": {"version": "17", "extensions": ["postgis"],
                    "storage": {"size": "40Gi", "storageClass": "fast-ssd"}}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

| Field | Default | What it does |
|---|---|---|
| `postgres.version` | the platform's own | The major version, as a number: `"17"` |
| `postgres.extensions` | none | Created in the database when it is built, as superuser, so the application never needs the right to create them |
| `postgres.storage.size` | the platform's own | A Kubernetes quantity — `"40Gi"` |
| `postgres.storage.storageClass` | the cluster's default | The class the volume is cut from |

This endpoint checks the *shape* — a version that is a version, extension names
that are identifiers, a size that parses. Whether they can be **supplied** is
the provisioner's answer, and it lands on the claim: a claim asking for an
extension no image the connection can run ships is `Failed`, and its `Ready`
condition names what could not be supplied and what is available instead. That
is the whole point of asking here — the refusal is the feature, and it arrives
before there is a database rather than three minutes into a rollout. A
connection to a hosted Postgres cannot be asked for any of it and refuses the
claim saying so, rather than provisioning as though the block had not been
written down.

Everything in the block is applied when the database is **created**. A major
version is not something to change under a live Postgres and a volume is not
something to shrink, so editing it on a bound claim reshapes nothing: asking
for a different database means asking for a different database.

**What `deletionPolicy` means for a database with a volume behind it.** For the
self-hosted provider, `Delete` deletes the database and CloudNativePG collects
its volume with it — the data is gone. `Retain` leaves the database running in
the platform's own database namespace, still holding its volume, and a claim of
the same name created later against the same connection finds it and rebinds to
it. That namespace is deliberately not the project's own: deleting a project
deletes that one, and a retained database has to survive exactly that.

Either type takes an optional `dataClass` — `public`, `internal`,
`confidential` or `strictlyConfidential` — classifying the data the resource
will hold. It may not exceed the [project](projects.md)'s own class
(classification narrows going down, never widens), and a classified claim in
an unclassified project is refused the same way: classify the project first.
Absent means unclassified, shown as such in the
[inventory](audit.md#the-classification-inventory) rather than defaulted.

The answer carries two facts the *provider* supplies once the claim binds,
alongside the class the caller chose. `dataProvenance` is the provider's
declaration of what the provisioned data derives from — `production`,
`masked` or `synthetic`; absent means the provider declared nothing, which
policy treats as the worst case rather than as clean. Neon declares
`production` for both a fresh database and every preview branch, because a
branch of a production database is production-derived. The self-hosted
provider declares `production` for the claim's own database and **`synthetic`
for every preview**: it has no copy-on-write branch, and a preview gets a
fresh, empty database with the same version, extensions and storage rather
than a slow copy of production — which keeps production data out of previews
by construction rather than by policy. `residency` is where
the provider reported the resource actually is — a Neon region id for the
hosted provider, and for a self-hosted database the topology of the node its
primary actually landed on, empty where the nodes say nothing about
themselves. Reported, not declared. Both are visible on the environment screen, where a preview
running on production-derived data is marked rather than implied, and both
are enforced at promotion by the default bundle's `data-provenance-preview`
rule.

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

## What a preview gets, and what a claim costs the workload

A preview environment gets a copy of production's database from Neon and an
empty one from CloudNativePG. Both are correct, and the difference between
them is the difference between a preview that can read production data and
one that cannot — so it is not decided inside each provisioner. Every
provider **declares** it, in code next to the implementation, and the
platform records the declaration on the claim, shows it where a dependency
is chosen, and generates the matrix below from it. A declaration that moves
without this page moving with it fails `make test`.

**Preview mode** is what a claim binds to in a preview environment:

| mode | meaning |
|---|---|
| `branch` | a cheap copy of production's data under its own address — production-derived, and declared so |
| `fresh` | a new, empty resource of the same shape, created with the preview and torn down with it — `synthetic` |
| `shared` | the production resource itself: the preview reads and writes what production does |
| `none` | no binding in previews; the claim's status says why |

`shared` is the one that matters. It is what everyone does informally, and
it is how a preview writes to production, so a claim through a provider
that holds data has to ask for it by name: `"previewMode": "shared"`. It is
never a default and never inferred.

**And the two things a claim can do to the workload that reads it**, because
both are otherwise invisible until they bite: whether the environment can
still scale to zero (`keepsPodsRunning`), and whether it forces a recreate
on every deploy, giving up zero-downtime deploys (`forcesRecreate`). The
environment reconciler acts on both — the `ScaleToZero` condition names the
claim, and the Deployment's strategy is set — and the claim's answer carries
them so the screen can say so before the claim is made.

### What can be claimed

`GET /claim-types` answers this table for the dashboard, with the same rows.
The CLI reaches it with `kitchen api GET /claim-types`; nothing else in the
CLI needs it, because no command creates a claim.

<!-- generated by hack/gen-claim-matrix from internal/provider/declarations; do not edit -->
| Type | Provider | Previews get | Scale to zero | Deploys |
|---|---|---|---|---|
| `postgres` | `neon` | `branch` — a copy-on-write branch of production's data under its own address — cheap, and production-derived: the branch declares provenance production | unaffected | unaffected |
| `postgres` | `cnpg` | `fresh` — a new, empty database with the same version, extensions and storage, never a copy of production: the branch declares provenance synthetic | unaffected | unaffected |
| `oidcClient` | `kitchen` | `shared` — every environment signs in through the project's one client; the operator keeps its redirect list in step as previews come and go, and a client holds no data | unaffected | unaffected |
<!-- end generated -->

### Choosing on the claim

`previewMode` on a claim is one of the provider's own mode, `shared`, or
`none`; empty takes the provider's declaration. A mode the provider cannot
give — `fresh` from Neon, `branch` from CloudNativePG — is refused here with
what the provider does give. The one exception to "empty takes the
declaration" is a provider that declares `shared` for a type that holds
data: previews then get `none`, and the claim's `previewReason` names the
`previewMode: shared` that would accept it.

The answer carries what was resolved: `previewMode` and `previewReason` are
the reconciler's decision and the sentence behind it, `previewChoice` is
what the claim asked for, and `keepsPodsRunning` and `forcesRecreate` are the
provider's declarations about the workload. All of it is on the claim's
status too, which is what a preview's workload, the policy engine and the
screen act on.

A preview of a claim whose mode is `none` deploys **without the variables
read from it**, and its `ClaimsBound` condition says which claim and why,
rather than the environment failing. A preview of a `shared` claim reads the
production binding. A preview of a `branch` or `fresh` claim waits for its
own binding, exactly as an unbound claim is waited for.

Claims written before providers declared carried `previewBranching: true`
to ask for a resource of the preview's own; it still reads as that, which is
now the provider's default. A claim that carried nothing used to give its
previews production — after upgrading, its previews get the provider's
declared mode instead, and `previewMode: shared` restores what it had.

Deleting a claim answers `202`: the operator's finalizer still has branches,
binding secrets, the registered client and — under `Delete` — the database
itself to remove.

