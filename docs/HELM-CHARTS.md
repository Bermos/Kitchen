# Kitchen — software that ships as a Helm chart

A great deal of self-hostable software is published as a Helm chart and
nothing else. **Kitchen does not install charts, and it is not going to.** The
decision on [#312](https://github.com/Bermos/Kitchen/issues/312#issuecomment-5536311908)
is shape C: a chart is a source of values that a person translates once, by
hand, into an ordinary project. This page is that translation, worked through.

It is not a workaround, and the difference is the whole reason the decision
went that way. A translated project is a first-class one — the platform's URL
on its wildcard certificate, its logs and its metrics, a workload screen,
releases and a rollback, evidence attached to the image it runs. A chart
installed as a chart renders objects carrying the chart's own labels and none
of the ones Kitchen selects on, so every one of those screens would be blank
for exactly the projects that arrived that way. The evidence is
[the September 2026 spike](spikes/helm-charts-2026-09.md): five charts a home
lab actually installs, every rendered object classified, and no chart of the
five expressible whole at the time it was written.

**Two of the four gaps it found have since closed.** A volume claim can
[bind a volume the platform did not create](api/claims.md#binding-a-volume-the-platform-did-not-create)
([#346](https://github.com/Bermos/Kitchen/issues/346)), and `runtime.security`
carries [`fsGroup`](CONFIG.md#runtime)
([#347](https://github.com/Bermos/Kitchen/issues/347)). Sonarr and Plex — the
two the spike put closest — now translate whole, which is why Sonarr is the
worked example below. What is left is [What is still open](#what-is-still-open).

## What a chart is, here

A chart is a template over N images at whatever tags its values name. Kitchen
has no template. A project is **a workload unit**: the images it runs, the
files they read, the storage they mount, the variables they take, and one HTTP
address the platform publishes. Translating is reading a rendered manifest
object by object and saying which of those each one is.

| What the chart renders | What it is here |
|---|---|
| A Deployment or StatefulSet with a Service and an Ingress | the project's **web process** — its `image` and its `runtime` |
| A Deployment with a Service and no route | a `service` [workload](api/processes.md) |
| A Deployment with neither | a `worker` |
| A CronJob | a `cron` |
| An install or upgrade hook Job | a `task`, which runs before the release takes traffic |
| A PersistentVolumeClaim, or a `volumeClaimTemplates` entry | a [`volume` claim](api/claims.md#creating-a-claim), `source: provision` |
| An inline `nfs:` volume, or a PersistentVolume somebody wrote | a [`volume` claim](api/claims.md#binding-a-volume-the-platform-did-not-create), `source: bind`, over a [volume the operator wrote](api/volumes.md) |
| A ConfigMap or Secret mounted as a configuration file | a [`files`](api/files.md) entry, `secret` where it holds a credential |
| A Secret holding credentials | [the project's own secrets](api/secrets.md), and a variable that references one |
| A bundled Postgres, Redis or object-store subchart | a [`ResourceClaim`](api/claims.md) |
| The image repository and tag | the project's `image` — [a project whose software this platform did not build](api/projects.md#a-project-whose-software-this-platform-did-not-build) |
| The Ingress, its class, its host, its TLS secret, its issuer annotation | nothing to declare. The environment has a URL already; a name of your own is [a domain](api/domains.md) |
| A ServiceAccount for a pod that never calls the API, a `helm test` pod, the init script that seeds a volume | dropped |
| RBAC, NetworkPolicy, PodDisruptionBudget, an admission webhook, a CRD | [not translated](#what-the-platform-will-not-take) |

Three of those rows are worth more than the line they get.

**The Ingress is the row everyone expects to be hard, and it is the easy one.**
Five of the spike's five realistic renders produce one; none of them survives
translation, because the environment's address is the platform's to give and it
already rides the wildcard certificate. The chart's TLS secret, its
cert-manager annotation and its ingress class all go with it.

**The bundled database is the row that pays.** Gitea's default render is 30
objects, and 22 of them are a Postgres, a Valkey and the bookkeeping around
them. Two claims replace all of it — with credentials the API never reads back,
and a preview that gets a database of its own, which a chart cannot do at any
price.

**The dropped row is not a shortcut.** A ServiceAccount exists because a pod
needs an identity, and Kitchen gives every pod one; two of the three in the
sample already disable the token mount. A `helm test` pod is never applied by
an install. And the shell scripts two of the five charts carry exist *only*
because Helm cannot write a file into a PersistentVolumeClaim — which is what
[`files`](api/files.md) does. What survives of them — the `mkdir` and the
first-boot merge that a placed file is not — is
[#348](https://github.com/Bermos/Kitchen/issues/348).

## There is no chart, so what is the artifact

#312 opened with four collisions between a chart and this platform. The second
of them — who owns the rendered objects — is the one that decided the shape,
and for a translated project it does not arise: the platform owns them because
it created them. The other three are questions a translated project still has
to answer, and the ordinary machinery answers all three.

- **What the artifact is.** The digest of the image the vendor published.
  Creating the project produces a Build that resolves the reference to a
  digest, harvests what the vendor attested about it, signs what it observed
  itself, and freezes it onto a Release without running a builder —
  [an artifact the platform did not build](api/builds.md#an-artifact-the-platform-did-not-build).
  There is no template to attest, because nothing templates anything.
- **What a version bump is.** A new digest under the tag the project follows,
  taken on the platform's poll or asked for with
  [`POST /projects/{name}/acquisitions`](api/projects.md#acquiring-a-new-digest).
  It is a Build, and it produces a Release like every other. A chart version
  does not appear anywhere in it: what moved is a tag.
- **Which rollback wins.** Kitchen's, because there is no second one.
  `kitchen rollback` puts the environment back on an earlier Release with the
  configuration that release froze. `helm rollback` never enters, and the two
  mechanisms #312 was worried about cannot both exist for a project that was
  translated rather than installed.

**Previews are refused in words**, and that is the one thing such a project
gives up. It has no repository to open a pull request against, so
`{"previews": true}` is a `400` saying so. The spike found this costs less than
it sounds: of its five charts, only two have a preview that would mean
anything, and both need a database branch — which a Kitchen claim gives and a
chart never could.

## Reading the chart

Render it with the values you would actually install it with, and read the
objects rather than the templates.

```sh
helm template rel <chart> -f values.yaml
```

**The values file is the thing you are translating, not the chart.** Every
default render in the spike was undeployable — persistence off, nothing
published, and in one case a single ServiceAccount and no application at all —
and one of the five is a general-purpose template whose values file *is* the
application. There is far less to carry across than a chart's size suggests: it
is a page of values, and somebody has already written it.

Then, in this order:

1. **The web process** — the workload behind the Service the Ingress points at.
   Its image and tag, its port, its probes, its resources, its security
   context.
2. **The storage** — every claim and every inline volume, with its mount path.
   Which of them the platform provisions and which already exists is the
   question that decides whether the application works at all.
3. **The configuration** — every file mounted from a ConfigMap or Secret, and
   every environment variable. A `secretKeyRef` to a secret the chart does not
   create is a project secret you set.
4. **The rest.** If it is in the dropped row above, drop it. If it is in
   [What the platform will not take](#what-the-platform-will-not-take), stop
   here — a project that deploys green and does the wrong thing is worse than
   one that was never created.

## A chart translated: Sonarr

The spike renders Sonarr the way a home lab actually deploys it, as a values
file over bjw-s `app-template` 5.1.0 — the values are
[Appendix A](spikes/helm-charts-2026-09.md#appendix-a--the-values-files).
Rendered, it is five objects: a Deployment, a PersistentVolumeClaim, a Service,
an Ingress and a ServiceAccount. All five are below.

### The storage

Sonarr mounts two filesystems and they are not the same kind of thing. `/config`
is its own database — 5Gi, and the platform cuts it. `/data` is the media on the
NAS: it existed before the cluster did, Sonarr writes it, and Plex reads it.

```sh
kitchen api GET /claim-volumes   # what the cluster has, what each offers, who holds it
```

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "config", "project": "sonarr", "type": "volume",
       "volume": {"source": "provision", "process": "web",
                  "size": "5Gi", "mountPath": "/config"}}' \
  https://kitchen.apps.example.com/api/v1/claims

curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "media", "project": "sonarr", "type": "volume",
       "volume": {"source": "bind", "process": "web", "mountPath": "/data",
                  "bind": {"persistentVolume": "nas-media",
                           "accessMode": "ReadWriteMany"}}}' \
  https://kitchen.apps.example.com/api/v1/claims
```

The dashboard's claim dialog makes both, and lists what is bindable rather than
asking anyone to remember a name. No CLI command creates a claim; from a
terminal it is `kitchen api POST /claims`, which is the deliberate answer for
every claim type.

Three things follow from those two lines without being asked for:

- **`Recreate` and one replica.** The config claim is `ReadWriteOnce`, so the
  process that mounts it is capped at one replica and deployed by recreation,
  with a gap in serving. That is exactly the `strategy: Recreate` the chart's
  values set by hand, and here it is a consequence rather than a setting.
- **One writer.** `accessMode` on a bound claim is how *this* project mounts
  the volume, and declaring `ReadWriteMany` makes Sonarr the export's writer.
  Any number of projects may read it; the second one asking to write it is
  refused, naming the claim that holds it.
- **`deletionPolicy: Delete` is refused outright** on a bound claim. The export
  existed before the claim and the platform does not own it, so teardown
  unmounts and never deletes.

The PersistentVolume behind the export — `nas-media` — is written on the
platform's own [Volumes screen](api/volumes.md), which is where an operator
points the platform at storage that was already there:

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "nas-media", "capacity": "12Ti",
       "accessModes": ["ReadWriteMany", "ReadOnlyMany"],
       "nfs": {"server": "nas.lan", "path": "/export/media"}}' \
  https://kitchen.apps.example.com/api/v1/persistent-volumes
```

Nothing is created on the NAS: the export is already there, and what this
writes is the record that lets a claim name it. It retains its data by
construction — the reclaim policy is `Retain` and cannot be set otherwise —
so removing it removes the record and leaves every byte where it is.

### The project

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "sonarr", "image": {"repository": "ghcr.io/home-operations/sonarr",
                                   "tag": "4.0.15"}}' \
  https://kitchen.apps.example.com/api/v1/projects
```

The New project dialog offers the same two sources — a repository this platform
builds, or an image somebody else published — and a public image is pulled
anonymously, so there is no `connection` to name. Creating the project resolves
the tag to a digest and produces the first Release; from then on the platform
watches the tag and acquires a new digest when it moves.

### The runtime and the variables

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"port": 8989, "cpu": "50m", "memory": "1Gi", "notRequestDriven": true,
       "security": {"runAsUser": 1000, "runAsGroup": 1000,
                    "fsGroup": 1000, "fsGroupChangePolicy": "OnRootMismatch"}}' \
  https://kitchen.apps.example.com/api/v1/projects/sonarr

kitchen env set --project sonarr \
  TZ=Europe/Zurich SONARR__SERVER__PORT=8989 SONARR__AUTH__METHOD=External
```

Four notes, because four things about that block are not a transcription of the
values file:

- **`notRequestDriven` has no counterpart in any chart, and it is the one to
  get right.** Sonarr's work — monitoring, searching, fetching — is not driven
  by anybody's HTTP request, so on an installation with scale to zero on, an
  idle Sonarr would be parked and would quietly stop doing its job. Helm idles
  nothing, so no chart has a field for this.
- **`fsGroup` is what makes the volume writable.** A provisioned volume comes up
  owned by `root:root`, and a process declaring `runAsUser: 1000` starts, reads
  as healthy and fails on its first write. `fsGroupChangePolicy` is the chart's
  own `OnRootMismatch`, kept for the reason the chart set it: it skips the
  ownership walk over a volume whose root already matches.
- **`cpu` and `memory` are request and limit alike**, so the chart's split — 50m
  and 256Mi requested, 1Gi allowed — becomes one number per resource, and the
  honest reading is the limit for memory: the number the application is allowed
  to reach is the number that must be there when it reaches for it.
- **The probes are a TCP connect on the port**, which is what Kitchen's default
  health check already is, so nothing is declared. Naming a `path` makes it an
  HTTP check and a stronger claim — Sonarr answers `/ping`.

### Where `kitchen.json` comes in

Sonarr runs a vendored image, so its project has no repository and therefore no
[`kitchen.json`](CONFIG.md): every field above goes on the routes instead, and
[the workloads page maps the file onto them field for field](api/processes.md#a-project-with-no-repository-declares-all-of-this-here).
The vocabulary is the same either way — and for a project that *is* built from a
repository, the same translation is a file committed beside the code:

```json
{
  "$schema": "https://raw.githubusercontent.com/Bermos/Kitchen/main/docs/schemas/kitchen.schema.json",
  "runtime": {
    "port": 8989,
    "notRequestDriven": true,
    "resources": {"cpu": "50m", "memory": "1Gi"},
    "security": {
      "runAsUser": 1000, "runAsGroup": 1000,
      "fsGroup": 1000, "fsGroupChangePolicy": "OnRootMismatch"
    }
  },
  "env": {
    "TZ": "Europe/Zurich",
    "SONARR__SERVER__PORT": "8989",
    "SONARR__AUTH__METHOD": "External"
  },
  "volumes": [
    {"name": "config", "process": "web", "mountPath": "/config"},
    {"name": "media", "process": "web", "mountPath": "/data",
     "source": "bind", "accessMode": "ReadWriteMany"}
  ]
}
```

**The file declares the volumes; it never makes them.** `size`, `storageClass`
and `bind` are refused in it by name — a file in a repository that could mount
somebody's NAS export would be a pull request mounting it into its own preview
— so the two claims stay the project's. What the declaration buys is the
failure nobody catches otherwise: the code writes to `/data`, the claim mounts
`/var/data`, and everything deploys green until the first restart takes the
data with it.

### What did not come across

| The chart | What became of it |
|---|---|
| `Ingress`, `className: nginx`, the TLS secret, the cert-manager annotation | the environment's own URL on the platform's wildcard certificate; a name of your own is a [domain](api/domains.md) |
| `Service` | `runtime.port`. Nothing else addresses Sonarr |
| `ServiceAccount` | dropped — Sonarr never calls the Kubernetes API, and the chart already turned its token mount off |
| `strategy: Recreate` | not declared: the `ReadWriteOnce` config claim sets it |
| the inline `nfs:` volume | the bound claim |
| `persistence.config` (5Gi, RWO) | the provisioned claim |

**Nothing is left over.** Sonarr — the chart the spike called the closest of
five and still not expressible — translates whole, and the one field it was
waiting for is #346.

### Plex, and the volume they share

Plex is the same shape and adds one thing worth seeing, which is what happens
when two projects want one filesystem.

| Plex's values | Here |
|---|---|
| `pms.configStorage: 50Gi`, `storageClassName: fast-ssd` | a provisioned volume claim at `/config`, size and class and all |
| the inline `nfs:` media volume, `readOnly: true` | a bound claim at `/data/media`, `accessMode: ReadOnlyMany` — the same `nas-media` Sonarr writes |
| `PLEX_CLAIM` from a `secretKeyRef` to a secret the chart does not create | a [project secret](api/secrets.md), and a variable that references it |
| `ADVERTISE_IP`, derived by the chart from the ingress host | `KITCHEN_URL`, which the platform injects into every environment |
| `podSecurityContext.fsGroup: 1000` | `runtime.security.fsGroup` |
| the `/transcode` `emptyDir` | nothing. Plex falls back to `/config`, which is the volume — a degradation rather than a break, and the only thing in either translation with no home |

**Two projects, one export, one writer.** Sonarr's claim writes the media;
Plex's reads it, and the platform mounts it read-only on the pod rather than
trusting the application to behave. The comparison is on what the volumes point
at — `nfs://nas.lan/export/media` — and not on their names, because two projects
reach one export through two PersistentVolumes and comparing names would call
them unrelated. A preview of Plex gets the same volume, read-only, because a
preview of a media server with an empty media directory is a preview of nothing.

## What the platform will not take

None of the following is a missing field somebody forgot. Each is a boundary,
and it is worth having them written down in one place, because a translation
that stops here is a decision and a translation that carries on is an
application that comes up green and does not work.

**Host paths and host devices.** Home Assistant's whole case: a `hostPath`
volume of type `CharDevice` for the Zigbee radio plugged into one machine. The
premise the platform is built on is that the cluster is abstracted away —
nothing needs `kubectl`, no namespace to name, no node to think about — and a
project reaching for a device on a node has left it.

**Node pinning.** The other half of the same case. A project pinned to a machine
cannot move, cannot preview and cannot scale, and the platform would be lying
about all three.

**Host network.** The third spelling of it. Link-local discovery — mDNS, SSDP —
does not cross a pod network, and giving a workload the node's network namespace
gives up the isolation everything else here assumes.

**A published port that is not HTTP.** Kitchen publishes one HTTP hostname per
environment. Gitea's `git@` on TCP/22 is half of what Gitea is for, and its own
chart offers a `TCPRoute` for it — so this one is a genuine gap rather than a
principle, and the spike says so. It is also a real feature with a real cost: a
second listener on the shared Gateway, no hostname to multiplex on, and
therefore a port allocation the platform would have to own. Until that is
argued on its own, software whose product is a non-HTTP port is not a Kitchen
project.

**Namespace RBAC for a control plane.** Coder's case, and the one #312 expected
to be a security question. It is not: a Role over pods, PVCs and Deployments in
the project's own namespace is far narrower than anything the platform already
grants itself. The refusal is a modelling one and it is firmer. The objects such
a workload creates would carry none of Kitchen's labels, so the component
survey, the log pipeline, the metrics, the workload screen and the project
finalizer all stop being complete the moment the grant is used. **An application
that creates Kubernetes objects is a second platform running inside this one.**

**A wildcard hostname per environment.** Coder again, publishing workspaces
under `*.coder.example.com`. A generated URL is one name and a custom domain is
one name verified by one record; a wildcard cannot be issued over HTTP-01 at
all, so this is not a schema change but a DNS-01 credential for somebody else's
zone.

**Downward-API environment variables.** A `fieldRef` to `status.podIP` is a
project reasoning about the cluster, which is the thing being abstracted away.
Coder needs it at more than one replica, for a relay mesh Kitchen has no opinion
about.

**A configuration file templated from the environment.** [`files`](api/files.md)
places a file exactly as written and substitutes nothing into it. The platform
becoming a template engine is the first step towards the thing the Addon
engine's compiled catalogue exists to refuse.

**Anything that arrives with a CRD.** None of the spike's five renders a custom
resource, and what they *can* render is overwhelmingly Gateway API's, whose CRDs
Kitchen already has through Cilium. That is a fact about the sample and not a
loosening: a chart that ships a CRD, or a custom resource of one, is out — Helm
validates a release's whole manifest before applying any of it, which is why
this rule holds for every shape #312 considered.

Grouped, they are one sentence. **A project that has to know which machine it is
on, create Kubernetes objects, or publish something that is not HTTP has left
the premise the platform is built on.** Home Assistant and Coder are not Kitchen
projects, under any of the three shapes #312 weighed, and saying so is more
useful than a translation that publishes a URL and never finds the radio.

## What is still open

- **[#348](https://github.com/Bermos/Kitchen/issues/348) — a volume that needs
  directories made and owned before the process starts has no first run.**
  `fsGroup` closed the ownership half of the common case; what is left is the
  `mkdir`, and a first-boot merge into a file that may already be there. A
  `task` cannot stand in, because it is a separate pod and a volume claim binds
  to one process. The issue is filed as a three-way decision — a step in the
  model, a task that may mount another process's volume, or a plain statement
  that images which cannot start on an empty volume are out of scope — and it
  is being settled rather than assumed, so this page will say which when it is.
  Two of the spike's five charts need it; neither of the two translated above
  does.
- **A published port that is not HTTP.** Named by the spike as deserving a
  field and deliberately not filed with the others: it is a real feature with a
  real cost, and it wants an argument of its own before it wants an issue. With
  #348 it is what stands between Gitea — the third of the five — and a Kitchen
  project.

**What would reopen the decision**, from the spike, stated so it can be checked
rather than re-argued: a different estate — render ten more charts and re-run
the count, and if what maps clears 80% with no load-bearing residue in the set,
shape A is worth another look; charts whose values files are themselves
declarations, which makes translating a schema mapping rather than an
interpretation; a decision that Kitchen should host control planes, which is a
change to the platform's premise and should be made as one; or a `helm template`
job used as a *reader* — a tool that renders a chart and prints the project
declaration it would correspond to, for a person to edit and create by hand,
which is this page with a good tool and carries none of shape A's promises.
Closing any single field gap above would not reopen it.

## Where to go next

| | |
|---|---|
| [The spike](spikes/helm-charts-2026-09.md) | Five charts, every object classified, and the numbers behind this page |
| [`kitchen.json`](CONFIG.md) | Every key a repository may declare, and the short list it may not |
| [Claims](api/claims.md) | Volumes provisioned and bound, databases, caches, object stores |
| [Volumes](api/volumes.md) | Pointing the platform at an export or a share that already holds data |
| [Projects](api/projects.md) | Creating one from an image, and acquiring a new digest |
| [Workloads](api/processes.md) | Workers, services, scheduled jobs and tasks, and the file-to-route map |
| [Configuration files](api/files.md) | What software configured by a file on disk needs |
| [Deploying an application](DEPLOYING.md) | The ordinary path, for software that has a repository |
