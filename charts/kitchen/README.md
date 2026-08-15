# Kitchen Helm chart

Deploys the Kitchen operator — CRDs, RBAC, the controller manager, the git
webhook receiver and its route on the shared Gateway — the platform's identity
provider with its Postgres, and the `Kitchen` singleton that holds platform
configuration.

## Prerequisites

Kitchen assumes these exist; the chart does **not** install them:

- **Cilium as the cluster CNI** with `gatewayAPI.enabled=true` and kube-proxy
  replacement. Cilium's Gateway API implementation is the ingress; there is no
  separate ingress controller.
- **Gateway API CRDs**, at the version your Cilium release requires — Cilium
  pins this tightly and it moves quickly, so read its docs rather than assuming
  the newest is right. The release states it on its Gateway API page, and CI
  resolves the same value from the Cilium version it targets.
- **A default StorageClass.** ClickHouse and the identity provider's Postgres
  are StatefulSets whose `volumeClaimTemplates` leave `storageClass` empty, so
  they take the cluster default. With no default they stay `Pending`
  indefinitely, which is the usual reason a first install looks like it hung.
- **A LoadBalancer address for the Gateway**: on bare metal, Cilium LB IPAM,
  plus L2 announcements or BGP to make that address answer.

  Note that cloudflared removes the need for the address to be *routable*, not
  the need for it to *exist*: Cilium reports `Programmed=False`
  (`AddressNotAssigned`) on a Gateway with no LoadBalancer IP, and the operator
  mirrors that into the Kitchen object, so the platform never becomes ready.
  With cloudflared you still want LB IPAM — you just do not need L2 or BGP,
  because the tunnel dials out from inside the cluster.
- **Wildcard DNS** for `*.<baseDomain>`, pointed at that address.
The platform namespace is **not** in that list — the chart creates and labels
`kitchen-system` itself; see [Install](#install).

cert-manager is **not** in that list either: the chart ships it as a sub-chart
(`cert-manager.enabled`, on by default), because Kitchen owns the cluster it is
installed into. Set `cert-manager.enabled=false` for a cluster that already
runs one.

In `acme` mode the **operator**, not the chart, creates the `ClusterIssuer` and
the wildcard `Certificate` from `kitchen.tls.acme`. That split is deliberate:
cert-manager's webhook admits both objects, so on a first install neither can
exist until it is serving, and a reconcile loop can wait where a Helm release
cannot. Progress shows up as the `CertificateReady` condition on the Kitchen
object.

The solver is DNS-01 with a Cloudflare API token. That is not a preference:
every generated URL is a subdomain of the base domain, so the platform needs a
wildcard certificate, and ACME issues wildcards over DNS-01 alone — no amount
of inbound reachability makes HTTP-01 able to.

In `acme` mode port 80 serves nothing but a permanent redirect to HTTPS: every
route the platform creates names the Gateway's `https` listener explicitly, and
the operator publishes a redirect route bound to `http`. In `none` and
`cloudflared` mode there is no HTTPS listener — port 80 is where the platform
answers, and no redirect is created. Routes pointed at a Gateway other than the
shared one therefore need listeners named `http` and `https`.

## Install

The operator resolves its platform namespace from a compiled-in constant, so
the chart must be installed into `kitchen-system`:

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

### The chart owns the namespace

`kitchen-system` is part of the release (`namespace.create`, on by default), so
its Pod Security level is set rather than inherited. That matters because the
log collector mounts the node's `/var/log`, and `hostPath` is admitted at the
`privileged` level alone — `baseline` forbids it outright, so there is no
narrower level that still collects logs. Clusters differ in what they default
to: kind is `privileged` and notices nothing, Talos is `baseline` and the
collector's pods are refused at admission with no pod ever created. The
namespace therefore carries `pod-security.kubernetes.io/{enforce,audit,warn}`
at `namespace.podSecurity`, and setting that stricter while `logs.enabled` is
true is refused at render time rather than discovered later.

Two consequences worth knowing:

- **Still install with `--create-namespace`, and do not create the namespace
  yourself.** Helm writes its release record into the target namespace *before*
  it applies any manifest, so a chart cannot bootstrap the namespace it installs
  into — `helm install` without the flag fails with `namespaces "kitchen-system"
  not found`. What the flag creates is a bare namespace, which the chart's
  template then adopts and labels. A namespace created any other way carries no
  Helm ownership metadata and **fails the install** instead of being adopted.
- **Uninstall keeps it.** The namespace is annotated
  `helm.sh/resource-policy: keep`, so `helm uninstall` leaves it and everything
  still inside — including the PVCs behind Postgres and ClickHouse. Without that
  annotation, deleting the release would delete the namespace and take the
  platform's data with it.

Set `namespace.create=false` to manage the namespace yourself, in which case
its Pod Security labels are yours to get right.

#### Adopting an existing namespace

An existing `kitchen-system` — one made with `kubectl create namespace`, or by
`--create-namespace` under a chart older than this one — has no Helm ownership
metadata, so install and upgrade both fail with:

```
Error: UPGRADE FAILED: unable to continue with update: Namespace "kitchen-system" in namespace ""
exists and cannot be imported into the current release: invalid ownership metadata; label
validation error: missing key "app.kubernetes.io/managed-by": must be set to "Helm"
```

Hand it over once, then upgrade normally:

```sh
kubectl label namespace kitchen-system app.kubernetes.io/managed-by=Helm --overwrite
kubectl annotate namespace kitchen-system \
  meta.helm.sh/release-name=kitchen \
  meta.helm.sh/release-namespace=kitchen-system --overwrite
```

The release name must match yours. Nothing is restarted or recreated — this only
writes metadata Helm reads to decide it may take ownership. The alternative, if
you would rather the chart kept its hands off, is `--set namespace.create=false`.

Releases are published to GHCR by `.github/workflows/publish.yml` when a `v*`
tag is pushed: the multi-arch operator image as `ghcr.io/bermos/kitchen`, the
auth image as `ghcr.io/bermos/kitchen-auth`, and the chart as
`oci://ghcr.io/bermos/charts/kitchen` with matching version and appVersion. Use
`./charts/kitchen` in place of the OCI reference to install from a checkout.

Then point `*.apps.example.com` at the Gateway:

```sh
kubectl get kitchen default -o jsonpath='{.status.gatewayAddress}'
```

Behind a cloudflared tunnel instead, no public address needed:

```sh
helm install kitchen ./charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.mode=cloudflared \
  --set kitchen.ingress.cloudflared.enabled=true \
  --set kitchen.ingress.cloudflared.tunnelSecretName=kitchen-tunnel

kubectl -n kitchen-system create secret generic kitchen-tunnel --from-literal=token=<tunnel-token>
```

The secret is created after the release because the chart is what creates the
namespace it goes in. The operator reports `TunnelConnected=False` until the
token is there and reconciles again once it is.

Or without TLS at all, which is how a cluster usually comes up first — before
DNS and certificates exist:

```sh
helm install kitchen ./charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.mode=none
```

`kitchen.tls.mode` decides the scheme of every URL the chart and the operator
publish, not only whether a certificate is managed. In `none` mode the shared
Gateway gets an HTTP listener and nothing else, so the OIDC issuer, the API's
external URL, the UI's redirect URIs and generated app URLs are all `http://`
— which is what makes login work there instead of failing discovery against a
scheme nothing serves. Everything is in the clear, so it is a way to bring a
cluster up, not to run one.

## Telemetry store

The chart runs a single-node ClickHouse — the store for logs, metrics, traces,
build logs and Hubble flow data. It is not the system of record; the CRDs are.

Connection details always land in the secret `<release>-clickhouse` (`host`,
`httpPort`, `nativePort`, `database`, `username`, `password`, `dsn`), whether
ClickHouse runs here or elsewhere, so the collectors have one place to look.

The password is generated on install and read back from the cluster on upgrade,
so it stays stable. Two consequences worth knowing:

- `helm template` can't read the existing secret, so rendering offline invents
  a fresh password every time. Set `clickhouse.auth.password` if you render
  manifests instead of installing.
- The bundled `default` user is dropped at startup, because
  `clickhouse.auth.username` is not `default`. Don't set it back to `default`
  unless you want a passwordless superuser on the network.

Point at an existing ClickHouse instead:

```sh
--set clickhouse.enabled=false \
--set clickhouse.external.host=clickhouse.telemetry.svc \
--set clickhouse.auth.password=<password>
```

Or install without a store at all — logs, metrics and traces then have nowhere
to land — with `--set clickhouse.enabled=false --set
clickhouse.acknowledgeNoStore=true`.

## Logs

A Vector DaemonSet tails every container log file the kubelet writes and ships
the lines into ClickHouse. Kitchen's labels become columns, so a line can be
traced back to what produced it:

| Column | From |
|---|---|
| `source` | `build`, `runtime` or `platform`, derived from the labels below |
| `project` | `kitchen.bermos.dev/project` |
| `environment` | `kitchen.bermos.dev/environment` (production or a preview) |
| `build` | `kitchen.bermos.dev/build` |
| `namespace`, `pod`, `container`, `node`, `stream` | the pod that wrote the line |
| `level` | best-effort severity (`trace`…`fatal`), parsed out of JSON logs or the common text spellings; `""` when the line says neither |
| `message` | the line |
| `labels` | every pod label, for anything the columns miss |

The tables (`logs`, plus `events` for the activity feed and `flows` for the
traffic view) are created by the operator, not the chart — they are applied
from `Kitchen.spec.observability.clickhouse`, and their TTL follows
`retentionDays`, so changing retention in the UI or with `kubectl` is enough:

```sh
kubectl patch kitchen default --type=merge \
  -p '{"spec":{"observability":{"clickhouse":{"retentionDays":7}}}}'
```

Watch it take: `kubectl get kitchen default -o jsonpath='{.status.conditions}'`
carries a `TelemetrySchemaReady` condition.

Ask the store what a build did:

```sql
SELECT timestamp, message FROM logs
WHERE build = 'shop-bld-8f3a2c1d0abc' ORDER BY timestamp
```

or what an app is doing right now:

```sql
SELECT timestamp, container, message FROM logs
WHERE project = 'shop' AND environment = 'shop-production'
  AND timestamp > now() - INTERVAL 15 MINUTE
ORDER BY timestamp DESC
```

Build logs outlive the pods that wrote them: the build Job keeps its finished
pod for an hour so the collector can catch up, and the lines are in ClickHouse
long before that.

By default every pod on every node is collected, including the platform's own
components — that is what makes the store useful when Kitchen itself
misbehaves. Narrow it with `logs.extraLabelSelector` (for example
`app.kubernetes.io/part-of=kitchen`) or `logs.extraFieldSelector`, or turn the
collector off entirely with `logs.enabled=false`. With no telemetry store
configured the collector is not rendered at all.

### Sizing the collector

The collector's memory limit is set for the worst case rather than the normal
one, and the gap between them is wide: it settles at roughly **35Mi** in steady
state, and was measured at **1.2Gi** catching up on sixteen hours of a single
quiet node's logs. Thirty-five times the steady figure, for a burst that ends.

The limit is what it is because being short is not a slowdown, it is a loop.
Vector commits its read offsets as it goes, so a process killed *before its
first commit* has nothing newer to resume from and starts again at the
beginning. On a backlog it cannot finish inside the limit, that means: read,
die, re-read the same lines, die again — shipping the whole backlog every cycle
while never getting to the end of it. Each restart also adds whatever
accumulated meanwhile, so it moves away from finishing rather than towards it.

Measured on a cluster where this ran at the old 512Mi default for ten hours:

```
total     distinct   factor
7751294   173323     44.8x
```

Every line written about forty-five times, and the earliest line — from the
cluster's first minutes — present in 1122 copies. The store was not empty, it
was full of the same thing.

Two things follow for anyone tuning this:

- **Do not size the limit from steady-state usage.** `kubectl top pod` on a
  healthy collector reports something in the tens of megabytes, and a limit
  chosen from that number fails on the first restart after any outage.
- **1Gi is not a safe halving.** It happens to sit just above steady state and
  far below catch-up, so it looks fine indefinitely and then fails exactly when
  the collector has work to do.

The request stays small on purpose: 128Mi covers steady state with room, and
reserving catch-up-sized memory on every node for a burst that is rare would
cost far more than it buys. The trade-off is that a collector spiking to over a
gigabyte is using much more than it requested, which makes it a likelier target
if the node comes under memory pressure. On a node where that is a real risk,
raise the request rather than the limit.

### If no logs arrive

Check the collector is actually running, which is not the same as checking that
it was installed:

```sh
kubectl -n kitchen-system get ds -l app.kubernetes.io/component=logs
```

`DESIRED 1, AVAILABLE 0` with no pod anywhere means its pods are being refused
before they are created — Pod Security is the usual reason, and the rejection is
recorded as a `FailedCreate` event on the DaemonSet rather than as a pod in a
bad state:

```sh
kubectl -n kitchen-system describe ds -l app.kubernetes.io/component=logs
```

```
Warning  FailedCreate  daemonset-controller  Error creating: pods "kitchen-logs-gc9m4" is
forbidden: violates PodSecurity "baseline:latest": hostPath volumes (volumes "data", "var-log")
```

Fix it by labelling the namespace as described under
[Prerequisites](#prerequisites), then `kubectl -n kitchen-system rollout restart
ds -l app.kubernetes.io/component=logs`. The same message is on the Kitchen
object's `logs` component, which is where to look first.

## Identity provider

The chart runs Kitchen's identity provider at `auth.<baseDomain>` — better-auth
with its OAuth/OIDC provider plugin — backed by a single-node Postgres
StatefulSet. It is the login for the Kitchen UI and the operator API, and the
issuer apps get OAuth clients from. The architecture is in
[docs/AUTH.md](../../docs/AUTH.md), the service in [auth/](../../auth).

Because the service is mounted at the root of that hostname, the issuer is the
origin itself:

```
https://auth.apps.example.com/.well-known/openid-configuration
```

**Create the first administrator.** `helm install` prints a one-time link; it
stops working as soon as the installation has an account:

```sh
echo "https://auth.apps.example.com/bootstrap?token=$(kubectl -n kitchen-system \
  get secret kitchen-auth -o jsonpath='{.data.bootstrapToken}' | base64 -d)"
```

Public sign-up is off. To let people in with the GitHub account they push
from, register an OAuth app with the callback URL
`https://auth.<baseDomain>/callback/github` and pass it in:

```sh
--set auth.github.clientId=Iv1.… \
--set auth.github.existingSecret=github-oauth \
--set auth.allowSocialSignUp=true
```

Two credentials are generated into `<release>-auth` on install and read back on
upgrade, so they stay stable: `secret` (signs sessions and tokens — changing it
signs everyone out) and `serviceKey` (the operator's API key for dynamic client
registration). Rotate either by setting it explicitly and upgrading. As with
ClickHouse, `helm template` cannot read the existing secret, so rendering
offline invents new values every time.

The Kitchen UI is registered as an OAuth client on start, with the client id
`kitchen-ui` and the redirect URI `<kitchen.api.externalURL>/auth/callback`. It
is a public client — a browser application keeps no secret — so the provider
requires PKCE. Point it somewhere else, or add a development callback, with
`auth.ui.redirectURIs`; turn it off with `auth.ui.enabled=false`.

Accounts, sessions, OAuth clients and consents live in Postgres, with
connection details in `<release>-postgres` (`host`, `port`, `database`,
`username`, `password`, `dsn`). Point at an existing Postgres instead:

```sh
--set postgres.enabled=false \
--set postgres.external.host=postgres.databases.svc \
--set postgres.auth.password=<password>
```

Install without an identity provider — no login for the UI, no issuer for apps
— with `--set auth.enabled=false --set postgres.enabled=false`.

## Preview protection

Preview URLs are gated behind platform login by default: an anonymous request
to `shop-pr-42.<baseDomain>` lands on the identity provider and only a
signed-in platform user reaches the application, which needs no changes and
never sees the platform's session. The design is in
[docs/AUTH.md](../../docs/AUTH.md#preview-protection-forward-auth).

The gate is deployed by the **operator**, not by this chart — it cannot start
before an OAuth client has been registered for it, and only the operator can
register one. What the chart contributes is the switch, the hostname and the
image (the gate is a second binary in the operator's image, so there is nothing
extra to pull):

```sh
--set previewGate.enabled=true \
--set previewGate.host=previews.apps.example.com \
--set previewGate.sessionTTL=8h
```

`previews.<baseDomain>` needs to resolve like every other generated URL, which
the wildcard DNS record already covers.

The gate appears a reconcile after the identity provider starts answering, not
with the rest of the release — `helm install --wait` cannot wait for something
the operator has not created yet — so the condition is the thing to watch:

```sh
kubectl get kitchen default -o jsonpath='{.status.conditions[?(@.type=="PreviewGateReady")].message}'
kubectl -n kitchen-system rollout status deploy/kitchen-preview-gate
```

Protection is per project, on by default:

```yaml
spec:
  previews:
    protected: false     # serve this project's previews openly, on purpose
```

A project that asks for protection when the platform runs no gate gets **no
route at all** rather than a public one — the workload deploys, the URL is
withheld, and the Environment's `PreviewProtected` condition says why.

The operator keeps the gate's OAuth client in `kitchen-preview-gate-oidc` and
its session signing key in `kitchen-preview-gate`. Deleting the signing key
rotates it, which signs every preview visitor out; deleting the client secret
makes the operator register a new client on the next reconcile (the old one is
then orphaned at the identity provider).

## Platform health

The operator surveys the platform's workloads on every reconcile and records
what it sees on the Kitchen singleton. It finds them by label —
`app.kubernetes.io/part-of=kitchen` in `kitchen-system` — so it covers what the
chart installs and what the operator creates alike, under any release name, and
picks up new components without being told about them.

```sh
kubectl get kitchen default
```

```
NAME      BASEDOMAIN         GATEWAY        COMPONENTS   AGE
default   apps.example.com   203.0.113.10   6/6 healthy  5h
```

`status.components` has one entry per workload, in name order:

```sh
kubectl get kitchen default -o jsonpath='{range .status.components[*]}{.name}{"\t"}{.available}/{.desired}{"\t"}{.message}{"\n"}{end}'
```

```
auth            1/1
clickhouse      1/1
controller      1/1
logs            0/1     0 of 1 pods available: Error creating: pods "kitchen-logs-gc9m4" is
                        forbidden: violates PodSecurity "baseline:latest": hostPath volumes
postgres        1/1
preview-gate    2/2
```

A component short of pods carries the reason from its most recent warning
event, because that is frequently the only place one exists. Pods rejected at
admission, refused by quota, or unschedulable produce *no pod object at all*, so
`kubectl get pods` looks healthy and there is nothing to describe; the count and
the event are the whole signal.

The summary is also a condition, for scripting:

```sh
kubectl wait --for=condition=ComponentsHealthy kitchen/default --timeout=5m
```

Note the distinction between this and the other conditions. `ComponentsHealthy`
is about pods — whether what was asked for is running. `Ready`,
`GatewayProgrammed`, `CertificateReady` and the rest are about reconciliation —
whether the operator could do its job. A platform can be fully reconciled and
still have a component down, which is exactly the case worth surfacing, so
`Ready` deliberately does not fold this in.

Something absent from `status.components` was never created; something present
with `healthy: false` was created and is not running. An empty list means
nothing carries the label at all, which on a current chart means the workloads
are missing rather than unhealthy.

## Upgrade

```sh
helm upgrade kitchen ./charts/kitchen --namespace kitchen-system --reuse-values
```

The chart's `version` and `appVersion` are always the same number, and both are
[SemVer](https://semver.org/spec/v2.0.0.html): one release publishes the chart
and the two images it deploys together, and `image.tag` defaults to
`appVersion`, so a chart can never point at an operator from another release.
That number is what the dashboard reports on its settings page, which is the
quickest way to see what a cluster is actually running.

While the major version is 0, **a breaking change bumps the minor** — 0.4.2 to
0.5.0 may need manual steps, and the release notes say which. The changelog is
generated from the commits in the release, so anything marked
`BREAKING CHANGE:` is called out there.

CRDs ship as ordinary templates rather than in the chart's `crds/` directory,
so `helm upgrade` applies schema changes — that is the CRD migration path.
Removing a CRD field still needs care: the API server rejects a stored object
that no longer validates, so land conversion work before shipping a breaking
schema.

### Letting the platform update itself

Off by default. With it on, an upgrade is a button on the dashboard's settings
page instead of a command:

```sh
helm upgrade kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --reuse-values \
  --set selfUpdate.enabled=true
```

That one command is unavoidable — a chart cannot grant itself permissions it
was not installed with — and it is the right place for the decision, because
what it creates is a ServiceAccount bound to **cluster-admin**. The upgrade
applies this whole chart: CRDs, ClusterRoles, the namespace, cert-manager. A
narrower role would have to enumerate every kind the chart has ever contained
and would break the first time a release adds one, and a self-update that dies
half-way through rewriting the platform is worse than no self-update.

The account is separate from the operator's, so the grant is one object, and
`--set selfUpdate.enabled=false` takes it away again.

**Know who can use it.** Only something that can create a pod with that account
in `kitchen-system` can reach the grant, which means the operator — but the
operator creates one whenever an authenticated API caller asks it to, and the
API has no roles yet. Until it does, everyone who can sign in to the dashboard
can upgrade the platform. That is a fair trade on a homelab and a poor one on
an installation with accounts you do not control.

What happens when the button is pressed:

- The operator creates a `PlatformUpdate`, checks it, and runs
  `helm upgrade --reset-then-reuse-values --atomic` in a Job. `--atomic` rolls
  the release back if it does not come up; `--reset-then-reuse-values` keeps
  your overrides while picking up values the new chart version added, which
  plain `--reuse-values` would silently skip.
- The Job runs the upgrade rather than the operator, because the operator does
  not survive it: applying the new manager Deployment kills the pod, and a helm
  process killed mid-upgrade leaves the release `pending-upgrade` with nothing
  left to finish or roll it back.
- Refused before anything is applied: a downgrade (use `helm rollback`, which
  knows what the previous release contained), a version the platform is already
  on, an upgrade crossing a minor version unless `selfUpdate.allowMinor=true`,
  and a second upgrade while one is in flight. An operator built from source
  reports version `dev` and cannot self-update at all.
- `kubectl get platformupdates` is the history, and the Job's logs are the
  helm output — collected into ClickHouse like any other, under the component
  `self-update`.

It cannot rescue an installation that needs manual steps first: read
[Upgrading to a chart that owns the namespace](#upgrading-to-a-chart-that-owns-the-namespace)
and [Upgrading from 0.1.0](#upgrading-from-010) below, which are exactly the
cases where a `helm upgrade` fails part-way and leaves the release mixed.

### Upgrading to a chart that owns the namespace

Every release installed before `namespace.create` existed has a `kitchen-system`
that Helm does not own, and the upgrade refuses to import it. Either adopt it
once — see [Adopting an existing
namespace](#adopting-an-existing-namespace) — or upgrade with
`--set namespace.create=false` and keep managing its Pod Security labels
yourself. Adopting is a metadata-only change: nothing is restarted, and the
labels the chart then applies are what make log collection work.

### Upgrading from 0.1.0

Releases at 0.1.0 cannot be upgraded in place. Their ClickHouse and Postgres
StatefulSets stamp the chart version onto `volumeClaimTemplates`, which is an
immutable field, so any upgrade to a different version is rejected:

```
Error: UPGRADE FAILED: server-side apply failed for object kitchen-system/kitchen-clickhouse
apps/v1, Kind=StatefulSet: StatefulSet.apps "kitchen-clickhouse" is invalid: spec: Forbidden:
updates to statefulset spec for fields other than 'replicas', 'ordinals', 'template', ... are forbidden
```

Newer charts no longer put a versioned label there, but that does not repair a
StatefulSet already in the cluster — the live object still disagrees. Delete
the two StatefulSets first, orphaning what they manage, then upgrade:

```sh
kubectl -n kitchen-system delete sts kitchen-clickhouse kitchen-postgres --cascade=orphan
helm upgrade kitchen ./charts/kitchen --namespace kitchen-system --reuse-values
```

`--cascade=orphan` leaves the pods running and the PVCs intact; the upgrade
recreates the StatefulSets and they adopt the existing pods, so neither the
telemetry store nor the accounts database is interrupted. Confirm the volumes
came back with `kubectl -n kitchen-system get pvc`.

A failed upgrade does not roll back what it already applied, so a release
caught by this is left mixed — the Deployments on the new version, the
StatefulSets on the old one. The recovery above resolves that too: it ends in a
completed upgrade at one version.

The `Kitchen` singleton is applied as a **post-install hook** and is *not*
re-applied on upgrade. Platform config is also editable from the management UI,
and re-applying it every upgrade would silently revert those edits. Set
`kitchen.applyOnUpgrade=true` to make Helm the source of truth instead; the
object is then deleted and recreated on each upgrade (nothing is owned by it,
so the Gateway and tunnel survive).

## Uninstall

```sh
helm uninstall kitchen --namespace kitchen-system
```

Deliberately left behind: the CRDs (annotated `helm.sh/resource-policy: keep`),
every custom resource they hold, the `Kitchen` singleton — uninstalling the
control plane should not delete every project and its running environments —
and the `kitchen-system` namespace, kept by the same annotation. The namespace
matters because it is what the PVCs behind Postgres and ClickHouse live in:
were it deleted with the release, the platform's accounts and telemetry would
go with it. To tear it all down:

```sh
kubectl delete kitchen default
kubectl delete crd -l app.kubernetes.io/part-of=kitchen
kubectl delete namespace kitchen-system
```

## Values

| Key | Default | Description |
|---|---|---|
| `nameOverride` / `fullnameOverride` | `""` | Override generated resource names. |
| `namespaceCheck` | `true` | Refuse to render outside `kitchen-system`. |
| `namespace.create` | `true` | Manage the platform namespace as part of the release. Still needs `--create-namespace`; see [The chart owns the namespace](#the-chart-owns-the-namespace). |
| `namespace.podSecurity` | `privileged` | Pod Security level on the platform namespace. Anything stricter needs `logs.enabled=false`. |
| `namespace.labels` | `{}` | Extra labels for the platform namespace. |
| `image.repository` | `ghcr.io/bermos/kitchen` | Operator image. |
| `image.tag` | `""` | Defaults to the chart's `appVersion`. |
| `image.digest` | `""` | Pin by digest; wins over `tag`. |
| `image.pullPolicy` | `IfNotPresent` | |
| `imagePullSecrets` | `[]` | |
| `replicaCount` | `1` | Extra replicas serve webhooks and fail over faster. |
| `leaderElection` | `true` | Required when `replicaCount > 1`. |
| `developmentLogging` | `false` | Console encoder, debug level. |
| `serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `rbac.create` | `true` | Manager ClusterRole, leader election Role, bindings. |
| `selfUpdate.enabled` | `false` | Let the platform upgrade its own release from the dashboard. Creates a ServiceAccount bound to **cluster-admin**; see [Letting the platform update itself](#letting-the-platform-update-itself). |
| `selfUpdate.chart` | `oci://ghcr.io/bermos/charts/kitchen` | Chart the upgrade pulls from. |
| `selfUpdate.releaseName` | `""` | Release to upgrade. Defaults to this release's own name. |
| `selfUpdate.allowMinor` | `false` | Allow an upgrade that crosses a minor version — pre-1.0, where breaking changes land. |
| `selfUpdate.timeout` | `15m` | How long helm is given to finish. |
| `selfUpdate.serviceAccountName` | `""` | Generated when empty. |
| `selfUpdate.image.repository` / `.tag` | `alpine/helm` / `3.19.0` | Image the update job runs helm from. |
| `crds.install` | `true` | Install the `kitchen.bermos.dev` CRDs. |
| `crds.keep` | `true` | Keep CRDs (and custom resources) on uninstall. |
| `kitchen.create` | `true` | Create the `Kitchen` singleton. Needs `baseDomain`. |
| `kitchen.applyOnUpgrade` | `false` | Re-apply the singleton on every upgrade. |
| `kitchen.baseDomain` | `""` | Generated URLs are `<slug>.<baseDomain>`. |
| `kitchen.clusterName` | `""` | What the dashboard's status bar calls this cluster. Defaults to the first label of `baseDomain`. |
| `kitchen.api.externalURL` | `""` | Defaults to `kitchen.<baseDomain>`, under the scheme `kitchen.tls.mode` serves. |
| `kitchen.ingress.gatewayClassName` | `cilium` | GatewayClass for the shared Gateway. |
| `kitchen.ingress.cloudflared.enabled` | `false` | Run a cloudflared tunnel as the edge. |
| `kitchen.ingress.cloudflared.tunnelSecretName` | `""` | Secret with the tunnel token under `token`. |
| `kitchen.tls.mode` | `acme` | `acme`, `cloudflared` or `none`. Also the scheme of every published URL: `none` serves HTTP only. |
| `kitchen.tls.acme.email` | `""` | CA contact address. Required in `acme` mode. |
| `kitchen.tls.acme.server` | Let's Encrypt production | ACME directory URL. Use the staging directory while setting up. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName` | `""` | Secret holding a Cloudflare API token (`Zone:DNS:Edit` + `Zone:Zone:Read`). Required in `acme` mode. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretKey` | `api-token` | Key inside that secret. |
| `cert-manager.enabled` | `true` | Install cert-manager with the platform. Disable if the cluster already runs one. |
| `cert-manager.crds.enabled` / `.keep` | `true` / `true` | Install cert-manager's CRDs, and keep them on uninstall. |
| `kitchen.auth` | from `auth.*` / `previewGate.*` | The singleton's `auth` block mirrors `auth.enabled`, the resolved host, the secret the operator registers clients with, and the preview gate. |
| `kitchen.builds.defaultStrategy` | `auto` | `auto`, `dockerfile` or `buildpacks`. |
| `kitchen.builds.concurrency` | `2` | Builds running at once. |
| `kitchen.observability.clickhouse.retentionDays` | `30` | Telemetry retention. |
| `kitchen.observability.hubble.relayAddress` | `""` | host:port of Hubble Relay's gRPC endpoint (e.g. `hubble-relay.kube-system.svc.cluster.local:80`). When set, the operator ships flow observations into the telemetry store for the dashboard's traffic view. Empty disables flow collection. |
| `clickhouse.enabled` | `true` | Run a single-node ClickHouse in the release. |
| `clickhouse.image.repository` / `.tag` | `clickhouse/clickhouse-server` / `26.3.17.110-alpine` | Current LTS line. |
| `clickhouse.auth.database` / `.username` | `kitchen` / `kitchen` | Created on first start. |
| `clickhouse.auth.password` | `""` | Generated on install, preserved on upgrade. |
| `clickhouse.service.type` / `.httpPort` / `.nativePort` | `ClusterIP` / `8123` / `9000` | |
| `clickhouse.persistence.enabled` | `true` | PVC for the data directory. |
| `clickhouse.persistence.size` / `.storageClass` / `.accessModes` | `20Gi` / cluster default / `[ReadWriteOnce]` | |
| `clickhouse.resources` | 200m/1Gi → 4Gi | |
| `clickhouse.extraConfig` | `{}` | Filename → XML for `config.d`, passed through `tpl`. |
| `clickhouse.external.host` / `.httpPort` / `.nativePort` | `""` / `8123` / `9000` | Point at an existing ClickHouse. |
| `clickhouse.acknowledgeNoStore` | `false` | Install with no telemetry store at all. |
| `logs.enabled` | `true` | Run the Vector collector. Skipped when there is no store. |
| `logs.image.repository` / `.tag` | `timberio/vector` / `0.57.0-alpine` | |
| `logs.extraLabelSelector` / `.extraFieldSelector` | `""` | Narrow which pods are collected. |
| `logs.excludePathsGlobPatterns` | `[]` | Extra log file globs to skip. |
| `logs.globCooldownMs` | `5000` | How often the node is rescanned for new log files. |
| `logs.batch.maxEvents` / `.timeoutSeconds` | `5000` / `5` | Insert batching; the timeout is log latency. |
| `logs.buffer.maxEvents` | `20000` | Events held per node while the store is unreachable. |
| `logs.serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `logs.rbac.create` | `true` | ClusterRole to read pod/namespace/node metadata. |
| `logs.hostLogsPath` / `.hostDataPath` | `/var/log` / `/var/lib/kitchen/logs` | Node paths for logs and read offsets. |
| `logs.resources` | 100m/128Mi → 2Gi | The limit is sized for catching up on a backlog, not for steady state; see [Sizing the collector](#sizing-the-collector). |
| `logs.tolerations` | `[{operator: Exists}]` | Collect from tainted nodes too. |
| `logs.terminationGracePeriodSeconds` | `60` | Time to flush the buffer on shutdown. |
| `postgres.enabled` | `true` | Run a single-node Postgres for the identity provider. |
| `postgres.image.repository` / `.tag` | `postgres` / `17.6-alpine` | |
| `postgres.auth.database` / `.username` | `kitchen_auth` / `kitchen` | Created on first start. |
| `postgres.auth.password` | `""` | Generated on install, preserved on upgrade. |
| `postgres.service.type` / `.port` | `ClusterIP` / `5432` | |
| `postgres.persistence.enabled` | `true` | PVC for the data directory. Accounts die with the pod without it. |
| `postgres.persistence.size` / `.storageClass` / `.accessModes` | `8Gi` / cluster default / `[ReadWriteOnce]` | |
| `postgres.resources` | 100m/256Mi → 1Gi | |
| `postgres.external.host` / `.port` | `""` / `5432` | Point at an existing Postgres. |
| `auth.enabled` | `true` | Deploy the identity provider. Needs a Postgres. |
| `auth.image.repository` / `.tag` / `.digest` | `ghcr.io/bermos/kitchen-auth` / `""` / `""` | Tag defaults to `appVersion`. |
| `auth.replicaCount` | `1` | Stateless; state lives in Postgres. |
| `auth.secret` | `""` | Signing secret. Generated on install, preserved on upgrade. |
| `auth.serviceKey` | `""` | Operator API key for client registration, ≥64 characters. |
| `auth.serviceAccountEmail` | `operator@kitchen.local` | Machine account owning that key. |
| `auth.bootstrap.enabled` / `.token` | `true` / `""` | One-time first-administrator link. |
| `auth.ui.enabled` | `true` | Register the Kitchen UI as an OAuth client (PKCE, no secret). |
| `auth.ui.clientId` | `kitchen-ui` | Client id the UI authenticates with. |
| `auth.ui.redirectURIs` | `[]` | Defaults to `<kitchen.api.externalURL>/auth/callback`. |
| `auth.github.clientId` / `.clientSecret` | `""` | Upstream GitHub OAuth app. |
| `auth.github.existingSecret` / `.existingSecretKey` | `""` / `clientSecret` | Read the client secret from an existing secret. |
| `auth.allowSocialSignUp` | `false` | Let an unknown GitHub account create a Kitchen account. |
| `auth.trustedOrigins` | `[]` | Extra browser origins. |
| `auth.port` | `8080` | Container port. |
| `auth.service.type` / `.port` | `ClusterIP` / `80` | |
| `auth.route.enabled` | `true` | Publish the issuer on the shared Gateway. |
| `auth.route.host` | `""` | Defaults to `auth.<baseDomain>`. This is the OIDC issuer. |
| `auth.route.gateway.name` / `.namespace` | `kitchen` / `kitchen-system` | Must match the operator's constants. |
| `auth.resources` | 50m/128Mi → 512Mi | |
| `auth.logLevel` | `info` | `debug`, `info`, `warn`, `error`. |
| `previewGate.enabled` | `true` | Gate protected previews behind platform login. Needs `auth.enabled`. |
| `previewGate.host` | `""` | Where logins come back to. Defaults to `previews.<baseDomain>`. |
| `previewGate.replicas` | `2` | The gate is in the request path of every protected preview. |
| `previewGate.sessionTTL` | `8h` | How long a visitor stays signed in to a preview. |
| `api.port` | `8092` | Container port for the REST API. |
| `api.service.type` / `.port` / `.annotations` | `ClusterIP` / `80` / `{}` | |
| `api.route.enabled` | `true` | Publish the API on the shared Gateway under `/api/`. |
| `api.route.host` | `""` | Defaults to the host of `kitchen.api.externalURL`. |
| `api.route.gateway.name` / `.namespace` | `kitchen` / `kitchen-system` | Must match the operator's constants. |
| `api.extraAudiences` | `[]` | Token audiences accepted beyond the issuer and the API's own URL. |
| `webhookReceiver.port` | `8090` | Container port for the receiver. |
| `webhookReceiver.service.type` / `.port` / `.annotations` | `ClusterIP` / `80` / `{}` | |
| `webhookReceiver.route.enabled` | `true` | Publish the receiver on the shared Gateway. |
| `webhookReceiver.route.host` | `""` | Defaults to the host of `kitchen.api.externalURL`. |
| `webhookReceiver.route.gateway.name` / `.namespace` | `kitchen` / `kitchen-system` | Must match the operator's constants. |
| `metrics.enabled` | `true` | Serve controller-runtime metrics. |
| `metrics.port` | `8443` | |
| `metrics.secure` | `true` | HTTPS + TokenReview/SubjectAccessReview. |
| `metrics.serviceMonitor.enabled` | `false` | Prometheus Operator ServiceMonitor. |
| `metrics.serviceMonitor.labels` | `{}` | e.g. `release: kube-prometheus-stack`. |
| `metrics.serviceMonitor.insecureSkipVerify` | `true` | The metrics cert is self-signed by default. |
| `healthProbe.port` | `8081` | `/healthz` and `/readyz`. |
| `resources` | 10m/128Mi → 500m/512Mi | |
| `podSecurityContext` / `securityContext` | restricted PSS | Non-root, read-only rootfs, no capabilities. |
| `extraArgs` / `extraEnv` / `extraVolumes` / `extraVolumeMounts` | `[]` | Escape hatches. |
| `podAnnotations` / `podLabels` | `{}` | |
| `nodeSelector` / `tolerations` / `affinity` / `topologySpreadConstraints` | empty | Scheduling. |
| `priorityClassName` | `""` | |
| `terminationGracePeriodSeconds` | `10` | |

Scraping metrics from outside the mesh needs the
`<release>-metrics-reader` ClusterRole bound to the scraper's ServiceAccount.

## REST API

With `api.route.enabled=true` (the default), the operator's API answers next to
the webhook receiver on the same public name, split by path:

```
https://<kitchen.api.externalURL host>/api/v1/...
```

Every endpoint is behind the identity provider — there is no unauthenticated
mode, and an installation with `auth.enabled=false` answers 401 everywhere.
Tokens are validated statelessly against the issuer's JWKS, so the operator
keeps no sessions. A token for CI:

```sh
TOKEN=$(curl -sS -H "x-api-key: $KITCHEN_API_KEY" \
  https://auth.apps.example.com/token | jq -r .token)

curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects
```

The endpoints and the token flows are documented in
[docs/API.md](../../docs/API.md).

## Git webhooks

With `webhookReceiver.route.enabled=true` (the default), providers deliver to:

```
https://<kitchen.api.externalURL host>/webhooks/git/<connection>
```

which is the URL the Project reconciler registers with the git provider. The
route attaches to the Gateway the operator creates, so it stays unaccepted
until the Kitchen singleton has been reconciled.

## Development

`templates/crds.yaml` and `templates/manager-clusterrole.yaml` are generated
from `config/` by `hack/gen-helm-manifests.sh` — do not edit them by hand:

```sh
make helm-manifests   # after make manifests
make helm-lint
make helm-template
make helm-install BASE_DOMAIN=apps.example.com IMG=ghcr.io/bermos/kitchen:dev
```
