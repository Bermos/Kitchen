# Kitchen — Volumes the platform did not create

A [`volume` claim](claims.md#binding-a-volume-the-platform-did-not-create)
can mount storage that existed before the cluster did — twelve terabytes on a
NAS, a share a household has had for years, a volume a storage appliance's
own driver hands out. What it mounts is a **PersistentVolume**, and until
these routes existed nothing on the platform wrote one: the whole feature
rested on a step that was `kubectl apply -f media-pv.yaml` and on none of
Kitchen's three surfaces.

These are that step. They are the operator's, they write the object directly,
and **nothing here can destroy data**.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## What this is, and what it is not

The platform is pointed at storage; it never makes any. Nothing is formatted,
nothing is exported, no share is created on the server, and no capacity is
allocated anywhere. What is written is a *record*: the address of storage
that is already there, in the shape the thing that mounts it understands.

Two shapes, which are the two a real installation has:

- **`nfs`** — a server and an exported path. The overwhelming case.
- **`csi`** — a driver and the handle it knows a volume by. What a storage
  appliance's own driver hands out.

There is deliberately no third for a directory on a machine.
[`hostPath` is refused](#what-is-refused-and-why), with the reason.

## Writing one

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "nas-media", "capacity": "12Ti",
       "accessModes": ["ReadWriteMany", "ReadOnlyMany"],
       "nfs": {"server": "nas.lan", "path": "/export/media"}}' \
  https://kitchen.apps.example.com/api/v1/persistent-volumes
```

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "appliance-photos", "capacity": "2Ti", "accessModes": ["ReadWriteOnce"],
       "csi": {"driver": "csi.truenas.net", "volumeHandle": "tank/photos",
               "fsType": "ext4", "volumeAttributes": {"share": "photos"}}}' \
  https://kitchen.apps.example.com/api/v1/persistent-volumes
```

| Field | Default | What it does |
|---|---|---|
| `name` | required | What a claim names to mount it — a DNS name, so lowercase letters, digits, `-` and `.`. A name already in the cluster is a `409` rather than an overwrite: repointing a volume something is mounting is how a project silently starts reading somebody else's data |
| `capacity` | required | How much the storage offers, as a Kubernetes quantity (`12Ti`). It is what a claim mounting it asks for; nothing enforces it against the server, which is what "the platform allocates nothing" means in practice |
| `accessModes` | required | What the *storage* can do: one or more of `ReadOnlyMany`, `ReadWriteOnce`, `ReadWriteMany`. It is not what any one project may do with it — a claim declares [its own mode](claims.md#binding-a-volume-the-platform-did-not-create), and that is what decides who may write |
| `nfs.server` / `nfs.path` | one source, required together | The host serving the export, and the absolute path as the server exports it |
| `nfs.readOnly` | `false` | Refuses every write at the mount, whatever a project asks for |
| `csi.driver` / `csi.volumeHandle` | the other source | The name the driver registers under, and the id it knows this volume by |
| `csi.fsType` | the driver's own | What the driver should mount it as |
| `csi.readOnly` | `false` | As above |
| `csi.volumeAttributes` | none | Whatever else the driver asks for. Stored in the clear, so a key that reads like a credential is [refused](#what-is-refused-and-why) |

Answers `201` with the volume as `GET` reports it: what it points at, its
`identity`, its capacity, its access modes, and its `reclaimPolicy` — which
is always `Retain`.

## The platform owns what it wrote, and only that

Every volume written here carries `app.kubernetes.io/managed-by: kitchen`,
and that label is the whole of the ownership model:

- **`GET /persistent-volumes` answers those alone.** A volume somebody
  applied by hand is not the platform's to report on. It is still perfectly
  bindable, and it still appears on [`GET
  /claim-volumes`](claims.md#what-a-volume-claim-could-bind) with everything
  else a claim could mount; it is simply not something the platform is
  accountable for, and a list that mixed the two would be claiming otherwise.
- **`DELETE /persistent-volumes/{name}` refuses anything else**, as a `404`:
  the platform holds only what it created, exactly the line the [connection
  credentials](connections.md) draw.

The claim form marks the platform's own and lists them **first**, because
they are the ones it can vouch for — it knows what they point at and it knows
their reclaim policy. That ordering is the API's rather than the form's, so
`kitchen api GET /claim-volumes` gets it too.

## Deleting one deletes a record, never data

`persistentVolumeReclaimPolicy` is **`Retain`, always**. It is not a default:
asking for anything else is a `400` naming the reason, because a caller who
wrote `Delete` believed it and would go on believing it. `Retain` is what
makes the rest of this page safe to say — deleting removes the platform's
record of the storage and nothing on the server is deleted, formatted or
unexported.

```sh
curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/persistent-volumes/nas-media
```

`204`, and two refusals stand in front of it:

- **A volume a claim is mounting is a `409` naming the claim**, as
  `project/claim` — delete the claim first. Removing it under a running
  project takes the mount away from pods that are already reading it.
- **A volume held by something that is not one of this platform's claims** is
  a `409` too, naming what holds it, so there is somewhere to go and look.

## What is refused, and why

**`hostPath`.** A volume that is a directory on one machine ties whatever
mounts it to that machine: it cannot move, cannot be previewed and cannot
scale, and the premise the platform is built on is that the cluster is
abstracted away. It is a boundary rather than a missing field — see
[Helm charts](../HELM-CHARTS.md#what-the-platform-will-not-take) — so the
refusal says what to do instead: export the directory over NFS, or attach it
through a CSI driver.

**Anything credential-shaped.** A PersistentVolume is cluster-scoped and
holds no secrets by design, so:

- the five `*SecretRef` fields a CSI volume can carry
  (`nodePublishSecretRef`, `nodeStageSecretRef`, `controllerPublishSecretRef`,
  `controllerExpandSecretRef`, `nodeExpandSecretRef`) are refused. A
  reference is not itself credential material; what it points at is a Secret
  the platform never wrote and cannot account for, and a volume the platform
  owns must not depend on one. **A driver that cannot mount without a secret
  is not expressible here yet**, and saying so is better than half-writing
  it.
- a `csi.volumeAttributes` key that reads as a credential — `secret`,
  `password`, `token`, `credential`, `passphrase`, `apiKey` — is refused.
  Attributes are stored verbatim and read back by every listing, so a
  password pasted into one is a password in cleartext, which is the one thing
  this API is built never to hold.

**A mode no claim can ask for.** `ReadWriteOncePod` is refused: nothing in
the [claim vocabulary](claims.md#binding-a-volume-the-platform-did-not-create)
asks for it, and a volume offering only that could never be bound.

## Why there is no CRD behind this

The house rule is that a write surface waits for its reconciler, because an
API over objects nothing reconciles only looks like it works. Here there is
nothing left to reconcile: a PersistentVolume *is* the desired state, the API
server admits it, and the platform has no second opinion to keep applying to
it. A Kitchen kind in front of it would be a second copy of a Kubernetes
object kept in step by a controller that exists only to copy — two sources of
truth, bought for nothing. So the API writes the object directly, exactly the
way it writes a [connection's credential](connections.md), and the
`managed-by` label is what makes the write accountable afterwards.

## The CLI

`kitchen api` carries all three, deliberately:

```sh
kitchen api POST /persistent-volumes --data @media-volume.json
kitchen api GET /persistent-volumes
kitchen api DELETE /persistent-volumes/nas-media
```

Writing a volume is a one-time act by whoever administers the installation's
storage, in the same class as creating a connection — which has no command
either. A `kitchen volumes` command would be a first for the operator's
surface rather than an addition to it, and that is a larger decision than
this feature.

## Where to go next

| | |
|---|---|
| [Claims](claims.md#binding-a-volume-the-platform-did-not-create) | Mounting one of these into a project, and the one-writer rule |
| [Connections](connections.md) | The other thing an operator writes once and projects point at |
| [Helm charts, translated](../HELM-CHARTS.md) | Where these volumes come up: Sonarr writing the media Plex reads |
