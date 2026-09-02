# Kitchen — Claims

A claim asks the platform for something the project needs and does not want
to install itself — a database, a cache or a queue, a bucket, a disk, single
sign-on, durable background work — and binds it into the project's
environments. The credentials it produces stay in the cluster: the API hands
out the claim's status, never the secret's contents. A volume produces no
credential at all, only a mount.

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

Every type takes an optional `dataClass` — `public`, `internal`,
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

**`objectStore`** asks a Connection with the `objectStore` capability for a
bucket: somewhere an application can put a file it did not build into its
image — user uploads, generated exports, anything it writes and expects to
read back — instead of the container filesystem, which loses it on the next
deploy. The one provider is `s3`, and it is the most substitutable contract
the platform has: the bundled MinIO, a MinIO a team already runs, AWS S3 and
Cloudflare R2 all speak the same API, so an application written against the
binding moves between them without changing.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-uploads", "project": "shop", "connection": "kitchen-objectstore", "type": "objectStore",
       "objectStore": {"versioning": true, "size": "50Gi"}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

The binding secret carries `endpoint`, `bucket`, `region`, `accessKeyId`,
`secretAccessKey` and `forcePathStyle`. The last is not decoration: MinIO
addresses a bucket in the path and AWS in the host name, and an application
that guesses wrong fails on every request — so the Connection says which,
once, and every binding carries the answer.

**A bucket per claim, with a credential scoped to it.** Never a prefix in a
shared bucket, because a prefix is not an isolation boundary. At a MinIO —
the bundled store, or one a team runs — the platform mints a user and a
policy per bucket through the admin API, and the application is never handed
the connection's own key pair. At a store without that API (AWS S3, R2) the
connection says so with `scopedCredentials: false`, every claim is handed the
connection's own credential, and the bucket is the isolation.

| Field | Default | What it does |
|---|---|---|
| `objectStore.versioning` | off | Keep every version of an object, so an overwrite or a delete can be undone at the store |
| `objectStore.publicRead` | off | Anyone may read the bucket's objects without a credential. Only a store on the internet can honour it — the bundled store is reached at a Service address inside the cluster alone, and refuses it saying so |
| `objectStore.size` | no limit | A Kubernetes quantity the bucket may not grow past — a hard quota at a MinIO, refused at a store without the admin API to set one |

As with `postgres`, this endpoint checks the shape and the provisioner
answers whether it can be supplied: a claim asking the bundled store for a
publicly readable bucket, or an R2 connection for a size, is `Failed` with
the reason on its `Ready` condition, and nothing was created. Everything in
the block is applied when the bucket is created.

**What `deletionPolicy` means for a bucket.** `Retain`, the default, leaves
the bucket, its objects and — at a MinIO — its user at the store; a claim of
the same name created later against the same connection finds the bucket by
name and re-issues its credential. `Delete` removes the credential, every
version of every object, and the bucket. **A preview's bucket goes with the
preview under either policy**, like a database branch: it is the platform's
bookkeeping, not the data the policy exists to protect.

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

**`volume`** asks the platform for a persistent volume mounted into **one** of
the project's processes — for the workload that must write to a filesystem:
a legacy application, SQLite, anything that writes where it was told to
write rather than where a cloud-native rewrite would put it. It is the odd
claim: every other one produces credentials, and this one produces a mount.

**Two consequences, stated here because they are not fine if discovered
later.** A volume is `ReadWriteOnce` unless the cluster genuinely has a class
for more, and `ReadWriteOnce` caps the process that mounts it at **one
replica** and deploys it by **recreation — a gap in serving on every
deploy**. A rolling update would create the new pod before the old one stops,
both would want the same volume, and the new one would wait in `Multi-Attach`
for a volume the old one never releases: a deadlock, not a delay. The claim's
answer carries `forcesRecreate: true`, and the environment reconciler sets
`strategy: Recreate` on that process's workload, writes `replicas: 1`, and
caps the autoscaler's ceiling at one where the environment idles. Where the
StorageClass is detected to support `ReadWriteMany`, both are lifted:
`volume.accessMode` in the answer says which, and `volume.accessModeReason`
why. The dashboard says all of this where the claim is made.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-data", "project": "shop", "type": "volume",
       "volume": {"process": "web", "size": "10Gi", "mountPath": "/data"}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

It takes **no `connection`** — the provider is the cluster's StorageClass,
which Kitchen requires of every cluster anyway — and a required `volume`
block:

| Field | Default | What it does |
|---|---|---|
| `volume.process` | required | The one process that mounts it: `web` for the web process, or the name of one of the project's processes. A claim naming none, or a process the project does not have, is refused here with the list |
| `volume.size` | required | A Kubernetes quantity — `"10Gi"`. Set when the volume is created; it is not shrunk |
| `volume.mountPath` | required | The absolute path inside that process's container the volume appears at |
| `volume.storageClass` | the cluster's default | The class the volume is cut from; one the cluster does not have fails the claim, naming the ones it has |

**Detecting `ReadWriteMany`.** Kubernetes records nothing about access modes
on a StorageClass, so the platform reads the evidence it has and never
assumes: a class whose provisioner is a shared filesystem driver
(`nfs.csi.k8s.io`, `smb.csi.k8s.io`, `efs.csi.aws.com`, `file.csi.azure.com`,
`filestore.csi.storage.gke.io`, CephFS) is `ReadWriteMany`; anything else is
`ReadWriteOnce`. An operator overrides either answer by annotating the class
`kitchen.bermos.dev/read-write-many: "true"` or `"false"` — the way to declare
a block driver that serves NFS-backed volumes from one of its classes.

**Which process, and why only one.** Every pod an environment runs carries the
environment label — the web process, its workers, its scheduled runs — and
two of them mounting one `ReadWriteOnce` volume is the same `Multi-Attach`
failure, so the claim names one and only that one gets the mount. A worker
gets it the way the web process does, replica cap and recreate included. A
scheduled process may mount it too; with `ReadWriteOnce` a run that overlaps
the last one on another node waits for it to finish — bounded by the run's
timeout, and avoided by the default `concurrencyPolicy: Forbid`.

**Previews get a fresh, empty volume** of the same size and class, created with
the preview and deleted with it — `synthetic`, like a CloudNativePG preview
database — and `previewMode: shared` is refused: a preview mounting
production's volume would take it from production. `none` is the other
choice.

**`deletionPolicy`** is `Retain` by default, and it means what it means for a
database: deleting the claim keeps the volume. The volume lives in the
project's application namespace, where its pods are, and that namespace goes
with the project — so the moment the production claim binds, the platform
sets its PersistentVolume's reclaim policy to `Retain` and labels the volume
with the project and the claim. The namespace can go; the volume stays, and a
claim of the same name in the same project finds it and binds to it again
(ask for the size it has, or less). `Delete` deletes the volume and its data
with the claim. Preview volumes are always removed with their preview. The
answer's `volume.claimName` is the PersistentVolumeClaim and
`volume.persistentVolume` the volume behind it, once bound.

A volume claim binds to a mount rather than to a secret, so `secret` is empty
in the answer and nothing in `Project.spec.env` refers to it: the environment
reconciler finds the project's volume claims itself, and waits — `Ready=False`
with reason `VolumeNotBound` — while one has no volume for that environment
yet. Deleting the claim answers `202` like every other, and the process that
mounted it is redeployed without the mount.

Two things this does not do. It is not backed up: a volume's data belongs to
whoever backs up the cluster's storage, and [BACKUP.md](../BACKUP.md) says so
beside the databases the platform runs. And a process pinned to one replica
under `Recreate` cannot be restarted for a secret rotation without a brief
outage — the [rotation restart](secrets.md#what-rolls-and-what-does-not) meets
the same gap every deploy does, and says so in the entry it leaves.
**`inngest`** asks a Connection with the `backgroundJobs` capability — an
Inngest Cloud API key — for the keys a worker connects to Inngest with, so
that the application gets retries, sleeps, fan-out, concurrency limits and
cron without supervising any of it:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-jobs", "project": "shop", "connection": "inngest", "type": "inngest",
       "inngest": {"app": "shop-worker", "environment": "production"}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

| Field | Default | What it does |
|---|---|---|
| `inngest.app` | the claim's name | The app ID the application's Inngest client is created with (`new Inngest({ id })`). The claim reports on it; it cannot set it |
| `inngest.environment` | `production` | The Inngest environment production binds to — `production`, or a custom environment created in the Inngest dashboard. Previews never bind to it |
| `inngest.mode` | `connect` | How the worker reaches Inngest. `connect` is the only value; `serve` is refused, saying why |

The binding is four keys, spelled as the variables the Inngest SDKs read so
that `fromResourceClaim` names the key and the application reads a variable
of the same name: `INNGEST_EVENT_KEY`, `INNGEST_SIGNING_KEY`, `INNGEST_ENV`
and `INNGEST_BASE_URL`. `INNGEST_ENV` is empty for production and names the
preview's branch environment in a preview; `INNGEST_BASE_URL` is empty on
Inngest Cloud on purpose — the SDKs use it for the event API and the REST API
alike, and Cloud serves those from two hosts (`inn.gs` and
`api.inngest.com`), so setting it to either would misroute the other. A
self-hosted Inngest, which is not this provider, is what would set it. Bind
all four through `Project.spec.env`, and today that reaches every process of
the project — there is no per-process environment yet (#271), so the web
process carries the keys as well as the worker that uses them. It takes no
`deletionPolicy`: an Inngest app holds no data the platform could destroy
(see below).

*What the provider does, and what it cannot.* This type is shaped by five
facts about Inngest Cloud, established against its documentation and its
[v2 OpenAPI specification](https://api-docs.inngest.com/api-specs/v2.json)
before it was built:

1. **There is a management REST API**, v2 at `https://api.inngest.com/v2`,
   authenticated with a bearer [API key](https://www.inngest.com/docs/platform/api-keys)
   (`sk-inn-api-…`, created by an organization admin, optionally scoped to
   one environment) — [authentication](https://api-docs.inngest.com/authentication).
   That key is the `inngest` Connection's credential, and the platform
   validates it with `GET /account`.
2. **Apps are not created; they register on connect.** The API has
   `GET /apps` and `GET /apps/{appId}` and no create. A connect worker
   "automatically sync[s] your functions with Inngest when a worker
   connects" ([connect](https://www.inngest.com/docs/setup/connect)), so the
   app exists once the process holding the worker has started with the
   binding. The claim binds before that and reports it on an `AppConnected`
   condition: `NotConnected` until a worker has, `Connected` with the
   function count and SDK afterwards, `SyncFailed` with Inngest's words if
   the last sync failed.
3. **Keys are read, never minted.** `GET /keys/signing` and
   `GET /keys/events` answer the full key values for the environment named
   in the `X-Inngest-Env` header, as often as asked; there is no endpoint
   that creates or revokes either. So the platform reads the environment's
   signing key and its first event key (one named after the app is preferred)
   into the binding on every reconcile — a key rotated in the dashboard
   reaches the binding by itself — and a claim against an environment with no
   event key is `Failed` with `RequirementsUnsatisfiable`, saying to
   [create one in the dashboard](https://www.inngest.com/docs/events/creating-an-event-key);
   it binds on the next reconcile after that.
4. **Branch environments are the preview story.** They are "created
   on-demand", "share Event Keys and Signing Keys", and are selected by
   `INNGEST_ENV` ([environments](https://www.inngest.com/docs/platform/environments),
   [signing keys](https://www.inngest.com/docs/platform/signing-keys#signing-keys-and-branch-environments));
   the API lists them (`GET /envs`), creates them (`POST /envs`, `409` when
   the name exists) and archives or unarchives them (`PATCH /envs/{id}` with
   `isArchived`) — there is no delete, and "archiving a branch environment
   doesn't delete anything; it only prevents the environment's functions
   from triggering". Inngest archives a branch environment itself three days
   after its last deploy; a preview that is still open gets it unarchived on
   the claim's next reconcile. So a preview gets an environment of its own,
   found or created by name, its binding is the shared branch keys plus
   `INNGEST_ENV`, and closing the pull request archives it. The branch
   declares provenance `synthetic`: an empty event stream of the preview's
   own, never a copy of production's.
5. **A connect worker dials out**, first to the Inngest API for connection
   information and then to the WebSocket gateway the answer names
   ([connect](https://www.inngest.com/docs/setup/connect#connection-lifecycle));
   nothing dials in. That is why connect works behind a protected preview's
   gate and why serve mode is refused — in serve mode Inngest calls the
   application over HTTP and meets a login page — and it is why the
   declaration below says what it says about scale to zero.

**Scale to zero, and the connection cap.** The worker's WebSocket never
crosses the interceptor, so nothing can tell when the environment is idle;
the provider declares `keepsPodsRunning`, every environment reading the
claim reports `ScaleToZero: False` with reason `ClaimKeepsPodsRunning`
naming the claim, and — because idling is a project-level policy — the
project's settings say the project is not offered scale to zero and why.
Inngest caps concurrent worker connections per **account**, by plan: 3 on
the free plan, 20 on paid plans, at most 10 apps per connection
([connect](https://www.inngest.com/docs/setup/connect), and the
[pricing page](https://www.inngest.com/pricing) lists "Workers: 3 / 20 then
$10 per 10 workers"). Every running pod of the process holding the worker,
in every environment of the project, is one of them — a project with two
replicas and three open pull requests is eight. The API exposes no plan or
cap (`GET /account` answers an id, a name and an email), so the platform
counts rather than checks: the claim's `ConnectWorkers` condition carries
the number of environments reading the binding and the numbers above, and
the account's billing page is where the cap is.

**What deleting the claim does.** The binding secrets go and the preview
branch environments are archived. The app record stays at Inngest until
somebody archives it in the dashboard, the keys are the account's, and
event and run history live at Inngest under the account's own retention —
nothing the platform could destroy, which is why the type refuses a
`deletionPolicy`.

**`redis`** asks a Connection with the `cache` capability for somewhere to
put what an application can afford to recompute, or work it cannot afford to
lose:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop-jobs", "project": "shop", "connection": "valkey", "type": "redis",
       "redis": {"usage": "queue", "maxMemory": "512Mi"}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

The binding carries `url`, `host`, `port`, `password` and `tls`. `url` is the
single-string form every client library takes; `tls` says whether the
connection is encrypted, which an application should not have to guess.

**`usage` is the field this type exists for**, and it is the one that is
expensive to get wrong. A cache and a queue are opposite configurations of
the same server:

| `usage` | eviction | on disk | what running out of memory looks like |
|---|---|---|---|
| `cache` (default) | `allkeys-lru` | nothing | a miss, and nobody notices |
| `queue` | `noeviction` | every write, appended | the enqueue fails, loudly, where the application can retry |

A Sidekiq or BullMQ queue served by an evicting instance drops jobs under
memory pressure and reports nothing: the queue is empty, the work is gone,
and the application is none the wiser. So the usage is applied when the
instance is created, and a provider that cannot honour what the claim asked
**refuses the claim** rather than binding something that will lose work.
`cache` is the default because it is the safe one — a cache that turns out to
be a queue loses work, where a queue that turns out to be a cache only costs
a volume.

| Field | Default | What it does |
|---|---|---|
| `redis.usage` | `cache` | `cache` or `queue`, as above |
| `redis.maxMemory` | the platform's own | A Kubernetes quantity the instance may not grow past — `"512Mi"` |
| `redis.version` | the platform's own | The Valkey major, as a number: `"8"` |

**Two providers, and they differ in what they can promise.**

`valkey` runs **one instance per claim** in this cluster — a StatefulSet, a
Service and a Secret the platform writes with its own account, no operator
and no install job. It is the one that can honour everything above, because
it is the one that configures the server. A logical database number inside a
shared server was considered and rejected: it is not an isolation boundary —
one tenant's `FLUSHALL` empties another's, there is no per-tenant memory
limit, and keyspaces collide — and the requirement that matters here cannot
be shared at all, because `maxmemory-policy` is server-wide. One instance
cannot offer `noeviction` to a queue and `allkeys-lru` to a cache.

`redis` reaches a server somebody else runs — Upstash, ElastiCache, Aiven, or
the Valkey a team already has — over the URL its Connection holds. It
provisions nothing, so it can promise nothing about the server's
configuration, and it says so rather than guessing: an operator states on the
Connection what the server is configured for (`usage` in its config), and a
claim asking for something else is refused. A connection that does not say
refuses any claim that names a usage at all. `maxMemory` and `version` are
the server's own and are refused outright.

A preview gets a fresh, empty instance from `valkey` and a fresh, empty
**logical database at the same server** from `redis`. Both declare
`synthetic`, because what the preview gets is empty either way — but they are
not equally isolated, and the difference is worth knowing: a database number
keeps the preview from reading production's keys and does not keep a
`FLUSHALL` on either side from emptying both.

`deletionPolicy: Retain` (the default) keeps the instance and everything in
it; `Delete` destroys both, and for a queue that is somebody's unfinished
work, which is exactly why Retain is the default. Preview instances are torn
down with their previews under either policy. At an external server the
platform destroys nothing on the way out: the keyspace is the server's, and
emptying somebody else's database is not the platform's to do.

The CLI reaches all of this through `kitchen api`, as for every claim type;
no command creates a claim.

## What a preview gets, and what a claim costs the workload

A preview environment gets a copy of production's database from Neon, an
empty one from CloudNativePG, and an empty bucket of its own from an object
store. Both are correct, and the difference between
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
them so the screen can say so before the claim is made. The `inngest` type
is the first to declare `keepsPodsRunning`, and since scale to zero is a
project-level policy the cost is the whole project's: the project settings
say so, next to the switch that would otherwise decide it.

### What can be claimed

`GET /claim-types` answers this table for the dashboard, with the same rows.
The CLI reaches it with `kitchen api GET /claim-types`; nothing else in the
CLI needs it, because no command creates a claim — an `objectStore` or
`volume` claim is made the way every other is, `kitchen api POST /claims`
with the body above.

<!-- generated by hack/gen-claim-matrix from internal/provider/declarations; do not edit -->
| Type | Provider | Previews get | Scale to zero | Deploys |
|---|---|---|---|---|
| `postgres` | `neon` | `branch` — a copy-on-write branch of production's data under its own address — cheap, and production-derived: the branch declares provenance production | unaffected | unaffected |
| `postgres` | `cnpg` | `fresh` — a new, empty database with the same version, extensions and storage, never a copy of production: the branch declares provenance synthetic | unaffected | unaffected |
| `oidcClient` | `kitchen` | `shared` — every environment signs in through the project's one client; the operator keeps its redirect list in step as previews come and go, and a client holds no data | unaffected | unaffected |
| `objectStore` | `s3` | `fresh` — a new, empty bucket of the preview's own with its own credential, versioned when production's is and torn down with the preview: the branch declares provenance synthetic | unaffected | unaffected |
| `volume` | `storageClass` | `fresh` — a new, empty volume of the same size and class, never a copy of production's: the preview declares provenance synthetic | unaffected | **recreate, with downtime** — a ReadWriteOnce volume attaches to one pod at a time, so the process mounting it runs one replica and is deployed by stopping the old pod before starting the new one — a rolling update would leave the new pod waiting in Multi-Attach for a volume the old pod never releases. Every deploy of that process has a gap in serving; a StorageClass detected to support ReadWriteMany lifts both |
| `inngest` | `inngest` | `branch` — an Inngest branch environment of the preview's own — its own event stream, function set and run history, empty rather than a copy of production's, selected by INNGEST_ENV on the account's shared branch keys; archived, not deleted, when the preview goes | **blocked** — a connect worker holds an outbound WebSocket to Inngest's gateway that never crosses the interceptor, so nothing can tell when it is idle — and scale to zero is a project-level policy, so every environment of the project keeps its pods, previews included | unaffected |
| `redis` | `valkey` | `fresh` — a new, empty instance of the preview's own, configured like production's and torn down with the preview: the branch declares provenance synthetic | unaffected | unaffected |
| `redis` | `redis` | `fresh` — a new, empty keyspace of the preview's own at the same server, torn down with the preview: the branch declares provenance synthetic | unaffected | unaffected |
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

A preview of an `inngest` claim binds the account's shared branch keys with
`INNGEST_ENV` naming its own branch environment, which is what routes its
events and functions apart from every other preview's.

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
preview buckets, binding secrets, the registered client, the branch
environments to archive and — under `Delete` — the database or the bucket
itself to remove.

