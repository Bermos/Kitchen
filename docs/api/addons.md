# Kitchen — Addons

An Addon is a platform dependency the operator can install into the cluster it
owns, asked for by name.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## What an addon is, and what it deliberately is not

Kitchen owns the cluster it is installed into, and two of the things it needs
there cannot arrive in its own Helm release. KEDA's HTTP add-on ships a
`ScaledObject` of KEDA's CRD; CloudNativePG ships CRDs and an admission webhook
and is a popular thing for a cluster to already have. Helm builds and validates
a release's whole manifest before it applies any of it, and it will not adopt
objects another release owns — so the chart cannot install either. **An operator
is under neither constraint**: it applies in whatever order it likes, waits in
between, and refuses where somebody else's release is already serving.

So the platform installs them itself, one Addon at a time. What it will *not*
do is become a general chart installer:

> The catalogue is compiled into the operator. An Addon names an entry and a
> namespace, and nothing else — no repository URL, no chart name, no version,
> no values file.

That is not a limitation to be lifted later. The install job is bound to an
account that can apply CRDs and ClusterRoles, and a request that could name
what to install would make that grant unbounded and reduce its audit record to
"the platform installed something".

## The request and the grant are two different things

Anyone with RBAC in `kitchen-system` can create an Addon, so the gate cannot be
whether the object exists:

| | What it is | Where it lives |
|---|---|---|
| **The request** | *I want this installed* | `spec.install` on the Addon |
| **The grant** | *the operator may hold an account that can install it* | a chart value, which creates the install job's ServiceAccount |

Both are required. An Addon naming an entry this installation did not grant an
account for is `Refused`, and the message names the one value that would permit
it. An entry the operator has no catalogue entry for is `Refused` too, with the
catalogue listed.

The operator **seeds** an Addon for every entry the chart permitted — created
once, the fact recorded on the platform singleton, and never recreated. Granting
the account is an explicit act nobody performs without wanting the dependency,
so the seeded Addon asks for the install. Turning it off afterwards is one
field, and **an Addon somebody deletes stays deleted**: an installation that
would rather run its own KEDA has to be able to end up with no object at all.

The chart does not render these objects, and must not. `templates/kitchen.yaml`
is a `post-install` hook precisely because a chart cannot apply a custom
resource whose CRD arrives in the same release, and a chart-rendered Addon
would inherit that trap whole.

## The catalogue

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/addons
```

The list is the **catalogue**, not the objects — an entry nobody has asked for
has no Addon, and that is exactly the row somebody came to the page to click:

```json
{"items": [
  {"id": "cloudnative-pg", "title": "CloudNativePG",
   "summary": "Provisions Postgres into this cluster. Without it a postgres claim needs a connection to a database somebody else runs.",
   "charts": [{"name": "cloudnative-pg", "version": "0.29.0"}],
   "defaultNamespace": "cnpg-system",
   "permitted": true, "chartValue": "databases.install.enabled",
   "clusterAdmin": true,
   "grantBecause": "installing CloudNativePG applies CRDs, ClusterRoles and a webhook configuration, which is not an enumerable list",
   "blastRadius": "No project will be able to claim a Postgres from this cluster. …",
   "requested": true, "serving": true, "managed": true,
   "namespace": "cnpg-system",
   "installed": [{"name": "cloudnative-pg", "version": "0.29.0"}],
   "conditions": [{"type": "Ready", "status": "True", "reason": "Installed", "…": "…"}]}
]}
```

| Field | What it answers |
|---|---|
| `permitted` | Whether this installation granted an account to install it with. An entry that is not permitted is still listed, with `chartValue` — knowing what the platform *could* run, and the one line that would let it, is the question the page is open for |
| `clusterAdmin`, `grantBecause` | What that account can do and why, so the grant can be read before it is made |
| `requested` | Whether an Addon exists and asks for the install. Absent where there is no Addon at all |
| `serving` | Whether the cluster answers the entry's API, **whoever** installed it |
| `managed` | Whether that was the platform. It is what the operator reads as permission to upgrade the release, and nothing it does ever writes to a release it did not create |
| `installed` | The versions actually installed, re-derived from the install job on every pass rather than remembered |
| `blastRadius` | What stops working if it is removed |

`GET /addons/{name}` is the same shape for one entry, and answers for a
catalogue entry with no Addon as well as one with.

## Asking for one, and changing your mind

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"id": "cloudnative-pg"}' \
  https://kitchen.apps.example.com/api/v1/addons
```

`id` is the catalogue entry. `install` defaults to `true` — asking for an addon
is asking for it — and `namespace` is optional, defaulting to the entry's own,
which is upstream's, so that an installation taking the release over by hand
later finds it where its documentation says.

`PATCH /addons/{name}` takes `install` and `namespace`. Turning `install` off
does not uninstall anything: it stops the platform installing or upgrading it,
and what is running keeps running. Moving an entry the platform has already
installed is **refused** rather than silently ignored — helm would put a second
copy beside the first rather than migrate it, so the answer is to uninstall it
first.

## Removing one

```sh
curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/addons/cloudnative-pg
```

`202`, because the operator finishes it, and it is three different things
depending on what is there:

- **An entry something depends on does not go at all.** A Connection
  provisioning through it, or a claim bound through one, and the delete is
  refused: the Addon stays, with `UninstallRefused` and the dependents named,
  so the answer is a list of things to remove and not a shrug.
- **An entry the platform did not install loses its record and nothing else.**
  The release is somebody else's; the platform never wrote to it and does not
  get to remove it either.
- **An entry the platform installed is uninstalled**, by a job under the same
  account that installed it, in the reverse of the order it went in.

The response carries `blastRadius` — what stops working — and the dashboard
states it before it sends the delete. What is *refused* is data somebody cannot
get back; what is *stated* is a regression somebody chose.

## Conditions

One condition, `Ready`, whose reason is the whole vocabulary:

| Reason | What happened |
|---|---|
| `AlreadyServing` | It is serving and the platform installed nothing. It is used just the same; what the record decides is who may upgrade it |
| `Installed` | The platform installed it, at the versions in `installed` |
| `Installing` | Its job is running. Its helm output is in the platform's own logs, under the entry's component |
| `InstallFailed` | The job failed, with what helm said. Retried once the finished job is reaped |
| `NotInstalled` | It is not serving and this Addon does not ask for it |
| `Refused` | Not permitted by this installation, or not a catalogue entry. The message names the chart value, or the catalogue |
| `NamespaceInvalid` | `spec.namespace` is not a namespace name, refused before a cluster-admin job was created |
| `DependencyNotReady` | An entry it depends on is not serving yet; that one goes in first |
| `ServiceAccountMissing` | The chart said the account exists and it does not |
| `KedaNotOurs` | KEDA is here but its HTTP add-on is not, and the platform will not install over a release it does not own |
| `UninstallRefused` | Something still provisions through it |
| `Uninstalling` / `UninstallFailed` | Its removal is running, or said why it could not finish |

The platform singleton keeps two roll-ups of these — `ScaleToZeroReady` and
`DatabasesReady` — because "can this cluster idle an environment" and "can it
provision a database" are facts about the cluster that everything downstream
already asks. They are the Addon's own words, copied rather than restated.

## Reaching it from a terminal

There is no `kitchen` command for addons, deliberately: this is the operator's
cluster-bootstrap surface, next to installing the chart, and not something a
developer does in the normal running of a project. `kitchen api` reaches every
route above, authenticated:

```sh
kitchen api GET /addons
kitchen api PATCH /addons/cloudnative-pg -d '{"install": true}'
```
