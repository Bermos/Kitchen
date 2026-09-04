# Spike — what a real Helm chart contains, and what Kitchen cannot say

*September 2026. Evidence for [#312](https://github.com/Bermos/Kitchen/issues/312),
produced by [#313](https://github.com/Bermos/Kitchen/issues/313). No platform
code changed.*

#312 could not be decided from first principles, because the argument for
translating a chart (shape A) and the argument for running one (shape B) both
turn on one unknown: **how much of a real chart lands inside Kitchen's
vocabulary, and what is left over.** This page renders the charts a home lab
actually installs, classifies every object against what the platform can
express since #295, and counts.

The headline, before the working:

- **46% of the rendered objects arrive intact**, 21% arrive as a workload the
  platform can run but carrying fields it cannot hold, and **33% is residue**.
- **No chart is fully expressible. Not one of five.**
- Of the residue, **seven objects in eleven are inert** — service accounts,
  `helm test` pods and two shell scripts that #311 makes unnecessary — and the
  four that are load-bearing are two needs: a control plane that creates
  Kubernetes objects at runtime, and a published port that is not HTTP.
- **One missing field accounts for more broken applications than every other
  gap combined**: a volume the platform did not provision — the NAS export that
  already holds the media.
- **Zero charts need a CRD.** The forecloser everybody expected is not there.
- **Every chart renders an Ingress, and every chart can render an HTTPRoute
  instead by changing a value.** The rewrite to the shared Gateway is a values
  change, not a patch.

## What was rendered, and how to repeat it

Helm **4.2.2** (`helm-v4.2.2-linux-amd64.tar.gz`, sha256
`9adafecab4d406853bba163a70e9f104f47dbbf65ce24b7653bae7e36150bcb6`), in a
container with no kubeconfig. Each chart is rendered twice: once with the
chart's own default values, and once with a values file written for this spike
— the file a person would actually install it with. The values files are
[Appendix A](#appendix-a--the-values-files); the classification is what the
render produced from them.

| Software | Chart | Version | App version | Repository |
|---|---|---|---|---|
| Home Assistant | `home-assistant` | **0.3.79** | 2026.9.0 | `http://pajikos.github.io/home-assistant-helm-chart/` |
| Plex | `plex-media-server` | **1.7.1** | 1.43.0 | `https://raw.githubusercontent.com/plexinc/pms-docker/gh-pages` |
| Sonarr | `app-template` (bjw-s) | **5.1.0** | — | `https://bjw-s-labs.github.io/helm-charts` |
| Coder | `coder` | **2.37.0** | 2.37.0 | `https://helm.coder.com/v2` |
| Gitea *(extra)* | `gitea` | **12.7.0** | 1.27.0 | `https://dl.gitea.com/charts/` |

The four required by [#313's decided list](https://github.com/Bermos/Kitchen/issues/313#issuecomment-5531994511)
are the first four. Sonarr has no canonical chart of its own — TrueCharts, the
usual answer, returned `502 Bad Gateway` for `index.yaml` throughout this
spike, so Sonarr is rendered the other way the home lab deploys it, as a
values file over bjw-s `app-template`. That substitution is itself a finding
and is treated as one below.

**Gitea is the optional extra, and it earns its place.** The four required
charts between them render no bundled Postgres and no bundled Redis, which
leaves the `ResourceClaim` row of #313's table with no evidence at all. Gitea
ships `postgresql-ha` and `valkey-cluster` as subcharts, enabled by default,
and so is the only chart here that answers what a claim would be worth.

```sh
helm template rel <chart>                       # the default render
helm template rel <chart> -f <appendix-a file>  # the realistic render
```

## The unit of counting

An object is one top-level document in the rendered manifest, plus each
`volumeClaimTemplates` entry inside a StatefulSet — those are PersistentVolume
declarations that never appear as documents of their own, and leaving them out
would undercount exactly the thing the platform *can* express.

Every object falls in one of three classes:

- **Maps** — the vocabulary holds it whole. Nothing is lost.
- **Partial** — the platform can run the thing, but the object carries fields
  it has no way to say. The workload deploys; something about it is different.
- **Residue** — nothing in the vocabulary corresponds to it. Sub-classified by
  what dropping it costs.

## Chart by chart

### Home Assistant — `home-assistant` 0.3.79

Default values render four objects. Realistic values render seven, plus one
`volumeClaimTemplates` entry.

| Object | Class | Reading |
|---|---|---|
| `StatefulSet rel-home-assistant` | **partial** | The web runtime — image, port, probes, resources, `TZ`. Four fields have no home: `hostNetwork: true`, `dnsPolicy: ClusterFirstWithHostNet`, a `hostPath` volume of type `CharDevice` for the Zigbee coordinator, and a `nodeSelector` pinning the pod to the machine the dongle is plugged into. It also carries an init container. |
| `volumeClaimTemplates: rel-home-assistant` (10Gi, RWO) | maps | A `volume` claim on the web process at `/config`. |
| `Service rel-home-assistant` | maps | `spec.runtime.port`. |
| `Ingress rel-home-assistant` | maps | The environment's route, rewritten to the shared Gateway. |
| `ConfigMap hass-configuration` | maps *(given #311)* | `configuration.yaml`. Today there is nowhere to put it; #311 is exactly this object. |
| `ConfigMap init-script` | residue — mechanism | A shell script an init container runs to merge the template into `/config`. It exists **because Helm cannot write a file into a PVC**. Under #311 the platform places the file and this object has no reason to exist. |
| `ServiceAccount rel-home-assistant` | residue — inert | Home Assistant never speaks to the Kubernetes API. |
| `Pod …-test-connection` (`helm.sh/hook: test`) | residue — inert | Never applied by an install; `helm test` only. Kitchen has no such notion and loses nothing. |

**The default render is not a deployable Home Assistant.** `persistence.enabled`
is `false`, so the config volume is an `emptyDir`: every restart is a factory
reset. Nothing is published. This is true of every chart here and is discussed
under [What the default render is worth](#what-the-default-render-is-worth).

**What Home Assistant needs and Kitchen has no word for is one need, not
four**: the application's value is talking to hardware on the local link — a
USB radio on a particular machine, and mDNS/SSDP discovery that a pod network
does not carry. `hostNetwork`, the char device, the DNS policy and the node
pin are four spellings of it.

### Plex — `plex-media-server` 1.7.1

Default values render three objects plus one `volumeClaimTemplates` entry;
realistic values render four plus one.

| Object | Class | Reading |
|---|---|---|
| `StatefulSet rel-plex-media-server` | **partial** | The web runtime. `PLEX_CLAIM` arrives from a `secretKeyRef` to a secret the chart does not create — a plain Kitchen secret. `ADVERTISE_IP` is derived from the ingress host, which is what `KITCHEN_URL` already is. Two volumes have no home: an inline `nfs:` volume for `/data/media`, and an `emptyDir` for `/transcode`. `securityContext.fsGroup: 1000` has no home either. |
| `volumeClaimTemplates: pms-config` (50Gi, `fast-ssd`) | maps | A `volume` claim, size and storage class and all. |
| `Service rel-plex-media-server` | maps | Port 32400. |
| `Ingress rel-plex-media-server-ingress` | maps | Rewritten to the shared Gateway. |
| `ServiceAccount rel-plex-media-server` | residue — inert | The chart already sets `automountServiceAccountToken: false`. |

Plex is the case the chart list was chosen for, and it splits cleanly. The
**library metadata** is a `volume` claim and nothing is lost: 50Gi,
`ReadWriteOnce`, `fast-ssd`, `Recreate` on every deploy — which is what
[claims.md](../api/claims.md) already promises and what Plex wants anyway. The
**media** is not a claim at all. It is twelve terabytes on a NAS that existed
before the cluster did, mounted read-only, and shared with Sonarr. Kitchen's
`volume` claim provisions a *new* PersistentVolumeClaim from a StorageClass; it
has no way to name a volume it did not create, and no way for two projects to
mount one.

Losing the `emptyDir` transcode scratch is a degradation, not a break — Plex
falls back to `/config`, which is the PVC. Losing `fsGroup` is a break: the
config volume comes up owned by root and Plex, running as 1000, cannot write
it.

### Sonarr — bjw-s `app-template` 5.1.0

Default values render **one** object: a ServiceAccount. Realistic values render
five.

| Object | Class | Reading |
|---|---|---|
| `Deployment rel` | **partial** | The web runtime, `strategy: Recreate` — which a Kitchen volume claim sets anyway. The `nfs:` media volume has no home. `runAsUser`/`runAsGroup` map onto `runtime.security`; `fsGroup` and `fsGroupChangePolicy` do not. |
| `PersistentVolumeClaim rel` (5Gi, RWO) | maps | A `volume` claim at `/config`. |
| `Service rel` | maps | Port 8989. |
| `Ingress rel` | maps | Rewritten to the shared Gateway. |
| `ServiceAccount rel` | residue — inert | The pod already sets `automountServiceAccountToken: false`. |

Sonarr is the closest of the five to being expressible, and it is still not
expressible, for exactly one reason: the same NFS export Plex needs. Everything
else — one container, one config volume, one HTTP port, `Recreate`, a TZ — is
already what a Kitchen project is.

**That app-template is the answer at all is a finding.** There is no Sonarr
chart; there is a general-purpose template that a person fills in with an
image, a port, two volumes and an ingress. Shape A's premise is that a chart
carries knowledge worth importing. For a large part of the home lab estate the
chart carries almost none — the values file is the knowledge, and it is
already, structurally, a project declaration.

### Coder — `coder` 2.37.0

Default values render five objects; realistic values render six.

| Object | Class | Reading |
|---|---|---|
| `Deployment coder` | **partial** | The web runtime, and a good one: a tight security context Kitchen's default already matches, `CODER_PG_CONNECTION_URL` from a `secretKeyRef`, `CODER_PROVISIONER_DAEMON_PSK` from another. Two env values come from the **downward API** (`fieldRef: status.podIP`), and a third interpolates one of them (`http://$(KUBE_POD_IP):8080`). Kitchen's `env` takes a literal, a `secretRef` or a `fromResourceClaim`, and nothing else. |
| `Service coder` | maps | Port 80 → 8080. (`LoadBalancer` at defaults; ClusterIP is the realistic setting.) |
| `Ingress coder` | **partial** | Two hosts: `coder.example.com` **and `*.coder.example.com`**, each with its own TLS secret. The wildcard is where workspace applications are published. An environment has one hostname, and [a custom domain](../api/domains.md) is one hostname verified by one TXT or CNAME record. There is no wildcard anywhere in the model. |
| `ServiceAccount coder` | residue — **load-bearing** | The identity the Role below is bound to. |
| `Role coder-workspace-perms` | residue — **load-bearing** | `pods`, `persistentvolumeclaims` and `apps/deployments`, eight verbs each, namespace-scoped. |
| `RoleBinding coder` | residue — **load-bearing** | Binds the two. |

Coder was picked as the one that would not map, and it does not, for a reason
that goes past its manifest. **Coder is a control plane that creates workloads
at runtime.** The pods, PVCs and Deployments it makes are not in the render at
all — they appear when a user opens a workspace. Nothing Kitchen owns would
see them: no environment label, no `part-of: kitchen`, no component survey row,
no log pipeline, no metrics. The render undercounts what the software does, and
it is the only chart here where that is true.

The RBAC is therefore not "an RBAC rule the chart does not need outside its own
operator" — the distinction #313 asked for. It is the application. Refusing it
refuses Coder.

Two smaller things worth recording. Coder's **Postgres is not in the chart**:
its documentation tells you to `helm install` a Postgres chart separately and
hand the URL over in `CODER_PG_CONNECTION_URL`. Under Kitchen that is a
`postgres` ResourceClaim, and the platform's answer is strictly better than the
chart's — one claim, credentials the API never reads back, a preview that gets
its own database. And the two `fieldRef`s are Coder telling itself where it is;
at one replica they are ceremony, and at more than one they are the DERP relay
mesh, which is a thing Kitchen has no opinion about.

### Gitea — `gitea` 12.7.0 *(extra)*

This is the ResourceClaim evidence. **Default values render 30 objects plus two
`volumeClaimTemplates` entries. The same chart with `postgresql-ha` and
`valkey-cluster` disabled and the connection details supplied renders nine.**

| | Default | With the databases claimed |
|---|---|---|
| Objects | 30 (+2 volumeClaimTemplates) | 9 |
| Of which the bundled Postgres and Valkey | 22 (+2) | 0 |

The 22 that vanish are `NetworkPolicy` ×3, `PodDisruptionBudget` ×4,
`ServiceAccount` ×2, `Secret` ×2, `ConfigMap` ×3, `Service` ×5, `Deployment` ×1
(pgpool), `StatefulSet` ×2 and the two volume claim templates. **Seventy per
cent of Gitea's default manifest is two databases and the bookkeeping around
them**, and every object of it is replaced by two lines in a Kitchen project.

The nine that remain:

| Object | Class | Reading |
|---|---|---|
| `Deployment rel-gitea` | **partial** | The web runtime, plus **three init containers** (`init-directories`, `init-app-ini`, `configure-gitea`) that prepare the volume and merge the config before Gitea starts, plus `fsGroup: 1000`, plus a **second container port**: SSH on 2222. |
| `Service rel-gitea-http` | maps | The web runtime's. |
| `Service rel-gitea-ssh` | residue — **load-bearing** | Port 22 → 2222, TCP. `git clone git@…` is half of what Gitea is for, and Kitchen publishes HTTP and nothing else. |
| `PersistentVolumeClaim gitea-shared-storage` (20Gi) | maps | A `volume` claim. |
| `Secret rel-gitea-inline-config` | maps *(given #311)* | `app.ini`. |
| `Secret rel-gitea` | maps | Admin credentials — an ordinary Kitchen secret. |
| `Secret rel-gitea-init` | residue — mechanism | The init containers' shell scripts. Dissolves the way Home Assistant's does. |
| `Ingress rel-gitea` | maps | Rewritten to the shared Gateway. |
| `Pod …-test-connection` | residue — inert | `helm test`. |

Gitea also has, in its templates and not rendered here, an `HTTPRoute` **and a
`TCPRoute`** — which is the chart itself saying that HTTP alone does not
publish it.

## The numbers

### Objects

Across the four required charts, realistic values, 24 declarations:

| Class | Count | Share |
|---|---|---|
| Maps — arrives intact | 11 | **46%** |
| Partial — runs, but something is lost | 5 | 21% |
| Residue | 8 | **33%** |

Two thirds of the objects (16 of 24) have somewhere to go. Fewer than half get
there whole.

With Gitea's realistic render added — 33 declarations across five charts:

| Class | Count | Share |
|---|---|---|
| Maps | 16 | 48% |
| Partial | 6 | 18% |
| Residue | 11 | 33% |

The shares barely move, which is worth as much as the shares themselves: five
charts from four unrelated projects land in the same place.

### Charts

**Fully expressible: zero of five.** Every chart has at least one thing the
platform cannot say, and in four of the five that thing stops the application
doing its job rather than merely looking different.

Ordered by what it would take:

| Chart | What stands between it and a Kitchen project |
|---|---|
| **Sonarr** | One thing: mount an existing external volume. |
| **Plex** | The same one thing, plus `fsGroup`. Loses an `emptyDir` scratch (degradation, not a break). |
| **Gitea** | #311, plus `fsGroup`, plus a way to publish SSH, plus a substitute for three init containers. |
| **Home Assistant** | The host's network namespace, a host device, and a node pin. |
| **Coder** | Namespace RBAC for a control plane, a wildcard hostname, and downward-API env. |

**With #311 landed and one new field — a volume the platform did not provision
— two of five (Sonarr and Plex) become expressible.** That is the single
highest-value line in this report.

### Residue by kind and by purpose

Eleven residue objects across five realistic renders:

| Kind | Count | Purpose | Cost of dropping it |
|---|---|---|---|
| `ServiceAccount` | 3 | Pod identity | **None.** Home Assistant, Plex and Sonarr never call the Kubernetes API, and two of the three already disable the token mount. |
| `Pod` (`helm.sh/hook: test`) | 2 | `helm test` | **None.** Never applied by an install. |
| `ConfigMap` / `Secret` holding a shell script | 2 | Seed a volume before the app starts, because Helm cannot write into a PVC | **None once #311 exists** — the object is a workaround for the gap #311 closes. |
| `ServiceAccount` + `Role` + `RoleBinding` (Coder) | 3 | Create workspace pods, PVCs and Deployments at runtime | **The application.** Not a rule "the chart does not need outside its own operator" — it is what Coder is. |
| `Service` (Gitea SSH) | 1 | `git clone git@…` on TCP/22 | Half the product. |

The split #313 asked for lands cleanly, and it is lopsided: **7 of 11 residue
objects cost nothing to drop** — or cost nothing once #311 lands. Four do, and
they are two needs: a control plane's RBAC (three objects, one grant) and a
published port that is not HTTP.

The residue that does not appear as an object at all is larger and matters
more. It is the **fields inside the five "partial" workloads**:

| Field | Charts | Purpose | Cost |
|---|---|---|---|
| A volume the platform did not provision (`nfs:`) | Plex, Sonarr | The data that already exists | **The application.** Sonarr with no media library manages nothing; Plex serves nothing. |
| `securityContext.fsGroup` | Plex, Sonarr, Gitea | Let a non-root process write its own volume | **The application.** A volume it cannot write. |
| Init containers | Home Assistant, Gitea | Prepare a volume before the main container starts | Mostly #311. What is left — `init-directories`, chown, a first-run merge — has no substitute: a Kitchen `task` is a separate pod, and [a volume claim binds to exactly one process](../api/claims.md), so a task cannot mount the volume the web process holds. |
| `hostNetwork`, `hostPath` device, `nodeSelector` | Home Assistant | Local hardware and link-local discovery | The application. |
| A second published port, TCP | Gitea | `git` over SSH | Half the application. |
| A wildcard hostname | Coder | Workspace applications | A feature of the application. |
| Downward-API env (`fieldRef`) | Coder | Tell itself its own pod IP | Nothing at one replica. |
| `emptyDir` scratch | Plex, Gitea | Transcoding, temporary files | A degradation. |

### CRDs

**Zero of five charts render a custom resource, at default values or at
realistic ones.** The thing #312 named as the outright forecloser did not
appear once.

It is *available* in all five, from optional values, and the inventory is worth
recording because it says what a chart reaches for when it goes past core
Kubernetes:

| Chart | Custom resources it can render | Whose CRD |
|---|---|---|
| `home-assistant` | `HTTPRoute`, `ServiceMonitor` | Gateway API, Prometheus Operator |
| `plex-media-server` | `HTTPRoute` | Gateway API |
| `app-template` | `HTTPRoute` (via `route`), `ServiceMonitor`, `PodMonitor`, `ReferenceGrant` | Gateway API, Prometheus Operator |
| `coder` | `HTTPRoute`, `ListenerSet` | Gateway API |
| `gitea` | `HTTPRoute`, `TCPRoute`, `BackendTLSPolicy`, `ClientSettingsPolicy`, `ServiceMonitor`, `PodMonitor`, `PrometheusRule`, `VerticalPodAutoscaler`, `Route` (OpenShift) | Gateway API, Prometheus Operator, VPA, OpenShift |

Two observations follow, and they point opposite ways.

The optional CRs are overwhelmingly **Gateway API's**, whose CRDs Kitchen
already requires — Cilium is a prerequisite and it pins the version. So the
one custom resource these charts most want to render is the one the platform
can already accept, which is convenient for the Ingress rewrite below.

But the rule in CLAUDE.md — *a chart cannot bundle a dependency that ships a
custom resource of another chart's CRD* — is unmoved, because it was never
about these charts. It is about what a chart brings *with* it. Gitea's
subcharts are ordinary workloads; had one of them been CloudNativePG rather
than `postgresql-ha`, the release would not have installed at all. **Zero of
five need a CRD** is a fact about the estate this spike sampled, not a
loosening of the rule.

### Ingresses

**Five of five realistic renders produce an `Ingress`. Zero of five default
renders do** — every one of these charts ships with ingress disabled.

Every Ingress here would have to be rewritten to an `HTTPRoute` on the shared
Gateway to ride the platform's wildcard certificate, and the rewrite is
uniformly shallow: one host, one path, one backend Service, one TLS secret the
platform would stop needing. Two of the five (Coder, Gitea) render more than
one rule, and Coder's second rule is the wildcard the model cannot hold.

**All five charts can render a Gateway API `HTTPRoute` instead, from values.**
That is the useful finding, and it cuts both ways:

- For **shape B**, it means a chart runtime could very nearly get the
  platform's certificate by supplying `parentRefs` — except that the
  `HTTPRoute` it renders is the *chart's*, carrying the chart's hostnames and
  none of the labels the platform selects on, so the URL would be right and
  every downstream screen still blank.
- For **shape A**, it means the Ingress is the least of the problems. A
  translator that could only rewrite Ingresses would be solving the part that
  was already easy.

### What the default render is worth

Every default render in this spike is undeployable, and the ways differ:

| Chart | The default render |
|---|---|
| `home-assistant` | 4 objects. `persistence.enabled: false` — the config volume is an `emptyDir`, so every restart is a factory reset. Nothing published. |
| `plex-media-server` | 3 objects. A 2Gi config PVC and no media at all. Nothing published. |
| `app-template` | **1 object — a ServiceAccount.** No container, no application. |
| `coder` | 5 objects. No `CODER_ACCESS_URL`, no database; Coder refuses to start. A `LoadBalancer` Service. |
| `gitea` | 30 objects, and the only one of the five that would actually run — because it brings its own databases. Nothing published. |

This bears directly on shape A. A translator's input is not "a chart"; it is a
chart **and a values file the user wrote**. The values file is where the
application is, and for `app-template` it is the *whole* application. Whatever
#312 chooses, the platform cannot promise anything from a chart reference
alone.

## Which residue deserves a field, and which is the boundary

The test used here: a gap deserves a field when it is about **the application's
own shape** and the platform can hold it without acquiring an opinion it does
not want. It is a boundary when honouring it would mean giving up something the
platform's premise rests on — that Kitchen owns the objects it deploys, that a
project is portable across its own environments, that a preview is safe.

### Deserves a field

**1. A volume the platform did not provision.** *Two of five charts; the single
largest cause of a broken application in this sample.* A `volume` claim today
means "cut me a new PVC from a StorageClass". The home lab's other half is "the
data is already there, on the NAS, and Sonarr writes what Plex reads." That is
a third mode of the claim the platform already has — a name, an access mode, a
mount path, and no provisioning — and everything downstream (one process
mounts it, `Recreate`, the replica cap) already applies. It is also the one
gap here that is *not* about escaping the cluster: an existing PVC or a CSI
volume is as much a Kubernetes object as a new one.

It raises one question this spike cannot answer, and #312 should: **two
projects mounting one volume is two projects sharing state**, which nothing in
the model contemplates. The read-only case (Plex) is easy. The read-write case
(Sonarr writing where Plex reads) is a decision.

**2. `fsGroup`.** *Three of five charts.* `runtime.security` already carries
`runAsUser`, `runAsGroup`, `runAsNonRoot`, `readOnlyRootFilesystem`,
`allowPrivilegeEscalation` and `dropCapabilities`. It does not carry `fsGroup`,
and the moment a non-root process is handed a volume, it needs one. This is the
cheapest fix in the report: one field, in a block that already exists, on the
same argument every field in it was added for.

**3. A published port that is not HTTP.** *One of five charts, and half of what
that one is for.* Kitchen publishes one HTTP hostname per environment. Gitea
needs TCP/22 as well, and the chart's own templates offer a `TCPRoute` for it.
This is a real feature with a real cost — a second listener on the shared
Gateway, no hostname-based routing to multiplex on, and therefore a port
allocation the platform would have to own — and it should be filed and argued
on its own rather than smuggled in as chart support.

**4. Something for a first-run volume seed.** *Two of five charts, and the
residue that survives #311.* #311 places a file; it does not `mkdir`, `chown`,
or merge a template on first boot. A `task` cannot stand in, because a volume
claim binds to one process. Whether the answer is an init container in the
model, a task that may mount another process's volume, or a decision that
images which need this are out of scope — that is a question, and this spike's
contribution is to show it survives #311 rather than being solved by it.

> **Settled.** [#348](https://github.com/Bermos/Kitchen/issues/348) took the
> first of the three: an init step in the model, and **declarative rather than
> a command**. A workload declares `init` for a volume it mounts, made of two
> typed steps — `directories` created if absent, and configuration files
> `seed`ed in only where the destination does not exist — which the platform
> executes itself in an init container in the workload's own pod, from the
> operator's own image and under the project's own `runtime.security`. No
> user-supplied argv and no shell, so the rule the KEDA install job follows
> holds here; idempotent by construction, so running it on every start is the
> same as running it once; and #267's one-process rule is untouched, since the
> steps run in the pod that already mounts the volume. The second option was
> rejected because it changes that rule; the third because `fsGroup` (#347)
> plus a config file (#311) still leaves Home Assistant and Gitea unable to
> start on an empty volume, which is exactly the state the issue calls
> unacceptable. The reasoning lives with the field: [CRDS.md](../CRDS.md)
> (`runtime.init`), [CONFIG.md](../CONFIG.md) (`kitchen.json`) and
> [api/projects.md](../api/projects.md#a-volume-the-process-cannot-start-on).

### The boundary

**Host network, host devices, and node pinning.** Home Assistant's whole case.
Kitchen's premise is that the cluster is abstracted away — "nothing needs
kubectl", no namespace to name, no node to think about. A project that pins
itself to a machine because a radio is plugged into it has left that premise:
it cannot move, it cannot preview, it cannot scale, and the platform would be
lying about all three. **Home Assistant is not a Kitchen project, under any of
the three shapes**, and saying so is more useful than a partial answer that
publishes a URL and never discovers a device.

**Namespace RBAC for a control plane.** Coder's case, and the one #312's
question 4 is about. Granting a project's workload the right to create pods and
Deployments in its own namespace is *narrower* than the Addon engine's
cluster-admin, and #285's rule does not textually forbid it. But the objects it
would then create carry none of Kitchen's labels, so the platform's ownership
of what it deploys ends the moment the grant is used — the component survey,
the log pipeline, the metrics, the workload screen and the finalizer all stop
being complete. That is not a security refusal, it is a **model** refusal, and
it is firmer: an application that creates Kubernetes objects is a second
platform running inside this one.

**A wildcard hostname per environment.** Coder again. A generated URL is one
name; a custom domain is one name, verified by one record, with HTTP-01
issuance through the shared Gateway. A wildcard cannot be issued over HTTP-01
at all — [CLAUDE.md](../../CLAUDE.md) already states it — so this is not a
schema change but a DNS-01 credential for somebody else's zone. Out of scope,
and it should be said rather than discovered.

**Downward-API environment variables.** Deliberately not a field. A project
that needs to know its own pod IP is reasoning about the cluster, which is the
thing being abstracted away. Coder at one replica does not need it.

**Templating a config file from environment variables.** #311 already leans
against it. Nothing in these five renders changes that: every config file here
is either static or assembled by the chart's own init script, and the platform
becoming a template engine is the first step towards the thing #285's compiled
catalogue exists to refuse.

## Recommendation

> **Decided, 4 September 2026 — and partly built since.** #312 took this
> recommendation: [the decision](https://github.com/Bermos/Kitchen/issues/312#issuecomment-5536311908)
> is shape C, and shape C's own deliverable — a page that translates a chart by
> hand — is [docs/HELM-CHARTS.md](../HELM-CHARTS.md), which works Sonarr through
> to a project and states the boundaries below in the platform's own voice. The
> two fields this section lifts out have both shipped: a `volume` claim that
> [binds a volume the platform did not create](../api/claims.md#binding-a-volume-the-platform-did-not-create)
> ([#346](https://github.com/Bermos/Kitchen/issues/346)) and `fsGroup` in
> `runtime.security` ([#347](https://github.com/Bermos/Kitchen/issues/347)). So
> the highest-value line in this report has come true: **Sonarr and Plex are now
> expressible whole.** What survives of the residue is the first-run volume seed
> ([#348](https://github.com/Bermos/Kitchen/issues/348)), which is open, and a
> published port that is not HTTP, which is not filed.

### Shape C, with two fields lifted out of it and built anyway

**C — a chart is a source of values that a person translates by hand, and the
platform documents how.** The numbers say it, and they say it more clearly than
this spike expected to.

The case, in the order the evidence arrived:

**No chart is fully expressible, so no translator can be trusted.** Shape A's
product is "a proposed project *and* a list of what it cannot express". At 46%
clean, 21% lossy and 33% residue, that list is not a footnote — it is most of
the output, and reading it *is* the translation. The import would produce a
project that comes up looking correct and does the wrong thing: Sonarr with no
media, Plex that cannot write its own config volume, Gitea that cannot serve
`git@`. Every one of those is a green deploy.

**The residue that matters is not translatable in principle.** Eight of eleven
residue objects cost nothing to drop, which sounds like good news for A until
you look at the three that remain and the eight fields inside the "partial"
workloads. They are host devices, namespace RBAC, a TCP port, a wildcard, an
init container, an external volume. A translator cannot map them because there
is nothing to map them *to* — which is why #313 asked which deserve a field.
Four do. Building those four is not building a translator.

**The knowledge a chart carries is thinner than shape A assumes.** Sonarr's
chart is a general-purpose template with a values file poured into it, and that
values file is already, structurally, a project declaration: an image, a port,
two volumes, an ingress. Coder's chart does not even carry its own database.
And every default render here is undeployable, so the input to any importer is
a values file a person wrote — at which point the person has already done the
translation, and shape A automates transcription rather than understanding.

**Shape B fails on ownership, not on security.** The security question (#312's
question 4) has a better answer than expected: none of these charts needs a
cluster-scoped object, none needs a CRD, and only Coder needs RBAC at all. A
namespace-scoped grant would install four of the five. But the objects it
installed would carry none of the labels Kitchen selects on, so the component
survey, the log pipeline, the metrics, the workload screen, the process runs
and the finalizer would all be blank or incomplete for exactly those projects
— and the platform would have two classes of project with different
guarantees, which #312 already names as the large cost. Every chart can render
an `HTTPRoute` from values, so B could get the certificate right; it would then
publish a working URL over a project whose every screen is empty.

**And the translation is done once.** #312's own reading of C — "not obviously
the wrong one for a home lab where the translation is done once per application
and never again" — is what the renders show. Sonarr is nine lines of values;
as a Kitchen project it is a page in the dashboard. Doing that by hand, once,
is cheaper than any of the machinery above, and the result is a first-class
project with previews, rollback, logs and evidence, rather than a chart-shaped
exception to all of them.

### What to build anyway

C is not "do nothing". Two of these belong in the platform whether or not a
chart is ever mentioned again, and they are what turn C from a refusal into an
answer:

1. **`volume` claims that bind an existing external volume**, read-only or
   read-write, with the two-projects-one-volume question settled explicitly.
   *Unlocks Sonarr and Plex — two of the five charts — on its own.*
   **Shipped: [#346](https://github.com/Bermos/Kitchen/issues/346). Two projects
   may read one volume; one may write it.**
2. **`fsGroup` in `runtime.security`.** One field. Three of five charts need
   it, and so does every non-root image handed a volume.
   **Shipped: [#347](https://github.com/Bermos/Kitchen/issues/347).**
3. #311, already filed, which this spike confirms is correctly scoped —
   and which does **not** close the first-run volume seed, so that should be
   filed separately rather than assumed. **Filed:
   [#348](https://github.com/Bermos/Kitchen/issues/348), as the three-way
   decision it is rather than as a presumed init container.**
4. **A page in `docs/` that translates a chart by hand**, worked through one
   of these five, naming what does not come across. That page is shape C's
   entire deliverable and the reason it is an answer rather than a shrug.
   **Written: [docs/HELM-CHARTS.md](../HELM-CHARTS.md).**

### What would change this recommendation

Stated so that the next person can check rather than re-argue:

- **A different estate.** Five charts from four projects is a sample, and it
  was chosen for a home lab. If the software people actually ask for turns out
  to be twenty applications shaped like Sonarr — one container, one config
  volume, one HTTP port — then shape A's 46% becomes 90% for that set, and a
  translator that refuses anything it cannot express whole becomes worth
  building. **The check is cheap: render ten more charts and re-run this
  count.** If "maps" clears 80% and no chart in the set has load-bearing
  residue, revisit A.
- **A chart repository with values files that are themselves the product** —
  the app-template pattern generalised. If the input is already a declaration,
  translating it is a schema mapping rather than an interpretation, and A gets
  much cheaper. This spike saw one instance of it and it was the easiest chart
  in the set.
- **Someone deciding Kitchen should host control planes.** If a project may
  create Kubernetes objects, the ownership argument against B weakens
  considerably and Coder becomes possible. That is a change to the platform's
  premise, and it should be made as one, in the open, rather than arriving as a
  consequence of chart support.
- **A `helm template` in a Job proving cheaper than expected as a *reader*.**
  Nothing here needs the platform to install a chart in order to *look* at one.
  A read-only importer that renders a chart and prints the project declaration
  it would correspond to — for a person to review, edit and create by hand — is
  shape C with a good tool, and it carries none of A's promises. If that is
  built, it should be described as a translation aid and never as an import.

What would **not** change it: closing any single gap in the field list above.
Sonarr and Plex become expressible with the external volume; the other three
each need something the platform has decided against for reasons older than
this issue.

## Answers to #312's questions, so far as five renders can give them

Three of #312's eight questions are answerable from this evidence. The rest are
about what the platform is willing to promise, and this spike has nothing to
add to them.

| # | The question | What the renders say |
|---|---|---|
| **2** | Who owns the rendered objects | Nothing is stopping the *routing* from being adopted — every chart can render an `HTTPRoute` at a `parentRef` from values. Everything else is: the objects carry the chart's labels, and under shape B every downstream screen is blank. This is B's decisive cost, and it is a modelling cost rather than a security one. |
| **4** | Does #285's rule extend to application charts | The security ceiling is lower than feared. Zero of five charts render a cluster-scoped object; zero render a CRD; one of five renders any RBAC at all, and it is namespace-scoped. A narrower grant would install four of the five — so if B is ever revisited, it fails on question 2, not on this one. |
| **5** | What is a preview of a chart | Of the five, only Coder and Gitea have a preview that means anything, and both need a database branch — which the platform already gives a `postgres` claim and a chart cannot. Home Assistant, Plex and Sonarr are single-instance stateful appliances whose preview is a second copy with an empty volume. This is evidence that charts and previews are largely orthogonal, which strengthens C. |

## Appendix A — the values files

Each is the second render for its chart. They are written to be realistic
rather than minimal: what somebody would actually install, including the parts
that turn out not to be expressible.

### Home Assistant

```yaml
hostNetwork: true
dnsPolicy: ClusterFirstWithHostNet

env:
  - name: TZ
    value: Europe/Zurich

persistence:
  enabled: true
  size: 10Gi
  accessMode: ReadWriteOnce

configuration:
  enabled: true
  forceInit: false
  trusted_proxies:
    - 10.42.0.0/16

ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
  hosts:
    - host: home.example.com
      paths:
        - path: /
          pathType: ImplementationSpecific
  tls:
    - secretName: home-example-com-tls
      hosts:
        - home.example.com

# The Zigbee coordinator, passed through from the node it is plugged into.
additionalVolumes:
  - name: usb
    hostPath:
      path: /dev/serial/by-id/usb-Silicon_Labs_Sonoff_Zigbee_3.0_USB_Dongle_Plus_0001-if00-port0
      type: CharDevice
additionalMounts:
  - name: usb
    mountPath: /dev/ttyACM0

nodeSelector:
  kubernetes.io/hostname: node-with-the-dongle

resources:
  requests:
    cpu: 200m
    memory: 512Mi
  limits:
    memory: 2Gi
```

### Plex

```yaml
ingress:
  enabled: true
  ingressClassName: nginx
  url: plex.example.com
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
  tls:
    - hosts:
        - plex.example.com
      secretName: plex-example-com-tls

pms:
  configStorage: 50Gi
  storageClassName: fast-ssd
  claimSecret:
    name: plex-claim
    key: claim
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 8Gi
  podSecurityContext:
    fsGroup: 1000
  livenessProbe:
    httpGet:
      path: /identity
      port: 32400
    initialDelaySeconds: 60
    periodSeconds: 60

extraEnv:
  TZ: Europe/Zurich
  PLEX_UID: "1000"
  PLEX_GID: "1000"
  ALLOWED_NETWORKS: "10.0.0.0/8"

# The media already exists, on the NAS, and is shared with Sonarr.
extraVolumes:
  - name: media
    nfs:
      server: 10.0.0.20
      path: /export/media
extraVolumeMounts:
  - name: media
    mountPath: /data/media
    readOnly: true
```

### Sonarr, over bjw-s `app-template`

```yaml
controllers:
  sonarr:
    type: deployment
    strategy: Recreate
    containers:
      app:
        image:
          repository: ghcr.io/home-operations/sonarr
          tag: 4.0.15
        env:
          TZ: Europe/Zurich
          SONARR__SERVER__PORT: "8989"
          SONARR__AUTH__METHOD: External
        probes:
          liveness:
            enabled: true
          readiness:
            enabled: true
        resources:
          requests:
            cpu: 50m
            memory: 256Mi
          limits:
            memory: 1Gi

defaultPodOptions:
  securityContext:
    runAsUser: 1000
    runAsGroup: 1000
    fsGroup: 1000
    fsGroupChangePolicy: OnRootMismatch

service:
  app:
    controller: sonarr
    ports:
      http:
        port: 8989

ingress:
  app:
    className: nginx
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    hosts:
      - host: sonarr.example.com
        paths:
          - path: /
            pathType: Prefix
            service:
              identifier: app
              port: http
    tls:
      - secretName: sonarr-example-com-tls
        hosts:
          - sonarr.example.com

persistence:
  config:
    type: persistentVolumeClaim
    accessMode: ReadWriteOnce
    size: 5Gi
    globalMounts:
      - path: /config
  media:
    type: nfs
    server: 10.0.0.20
    path: /export/media
    globalMounts:
      - path: /data
```

### Coder

```yaml
coder:
  env:
    - name: CODER_ACCESS_URL
      value: "https://coder.example.com"
    - name: CODER_WILDCARD_ACCESS_URL
      value: "*.coder.example.com"
    - name: CODER_PG_CONNECTION_URL
      valueFrom:
        secretKeyRef:
          name: coder-db-url
          key: url
    - name: CODER_TELEMETRY_ENABLE
      value: "false"

  replicaCount: 1

  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      memory: 4Gi

  service:
    type: ClusterIP

  ingress:
    enable: true
    className: nginx
    host: coder.example.com
    wildcardHost: "*.coder.example.com"
    annotations:
      cert-manager.io/cluster-issuer: letsencrypt
    tls:
      enable: true
      secretName: coder-example-com-tls
      wildcardSecretName: coder-example-com-wildcard-tls

provisionerDaemon:
  pskSecretName: coder-provisioner-psk
```

### Gitea *(the extra: the bundled databases turned off, which is what a claim would do)*

```yaml
ingress:
  enabled: true
  className: nginx
  annotations:
    cert-manager.io/cluster-issuer: letsencrypt
  hosts:
    - host: git.example.com
      paths:
        - path: /
  tls:
    - secretName: git-example-com-tls
      hosts:
        - git.example.com

postgresql-ha:
  enabled: false
postgresql:
  enabled: false
valkey-cluster:
  enabled: false
valkey:
  enabled: false

persistence:
  enabled: true
  size: 20Gi

gitea:
  admin:
    existingSecret: gitea-admin
  config:
    database:
      DB_TYPE: postgres
      HOST: postgres.example.internal:5432
      NAME: gitea
      USER: gitea
    cache:
      ADAPTER: redis
      HOST: redis://valkey.example.internal:6379/0
    queue:
      TYPE: redis
      CONN_STR: redis://valkey.example.internal:6379/0
    session:
      PROVIDER: redis
      PROVIDER_CONFIG: redis://valkey.example.internal:6379/0
```

## Appendix B — object counts, as rendered

| Chart | Default objects | Realistic objects | Of which `volumeClaimTemplates` |
|---|---|---|---|
| `home-assistant` 0.3.79 | 4 (+0) | 7 (+1) | config, 10Gi RWO |
| `plex-media-server` 1.7.1 | 3 (+1) | 4 (+1) | `pms-config`, 50Gi RWO `fast-ssd` |
| `app-template` 5.1.0 (Sonarr) | 1 (+0) | 5 (+0) | — (top-level PVC) |
| `coder` 2.37.0 | 5 (+0) | 6 (+0) | — |
| `gitea` 12.7.0 | 30 (+2) | 9 (+0) | Postgres and Valkey, at defaults only |
