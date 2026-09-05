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

  Turn **Hubble and Hubble Relay** on with it (`hubble.enabled=true`,
  `hubble.relay.enabled=true`). Every request to every application crosses the
  Gateway's Envoy, and Relay is the only place those observations can be read
  from — without it the platform has no vantage point on its own traffic at
  all, and `kitchen.observability.hubble.relayAddress` has nothing to point at.

  Two of its settings are worth choosing rather than inheriting:

  - `hubble.redact.http.urlQuery=true`. Kitchen does not store query strings,
    and redacting at the source keeps a token somebody put in a URL out of the
    flow stream as well — the same rule, enforced twice.
  - `hubble.eventBufferCapacity`, the per-node ring buffer, which holds 4095
    events by default. When it overflows Relay says so with a `LostEvent`
    notice and what it dropped is simply absent: traffic and error rates
    under-report, with no gap on the screen to say they are. Raise it on nodes
    that carry sustained traffic.
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
- **`user.max_user_namespaces` above zero on every node that runs builds**, if
  anything is built with the **Dockerfile strategy**. BuildKit runs rootless
  here, so it creates a user namespace before it does anything at all. Talos
  ships the sysctl at `0`; `rootlesskit` cannot fork its child, and the error
  it reports for that is `no space left on device`, which names neither the
  sysctl nor the cause. Every Dockerfile build then fails a few seconds after
  it starts. On Talos the value can be set without a reboot:

  ```sh
  talosctl -n <node> patch mc --mode=no-reboot -p @- <<'YAML'
  machine:
    sysctls:
      user.max_user_namespaces: "64000"
  YAML
  ```

  An installation that builds with buildpacks alone can skip it — the CNB
  lifecycle enters as the builder image's own user and creates no user
  namespace — which is the same carve-out
  [`kitchen.appNamespaces.podSecurity`](#application-namespaces-have-a-level-of-their-own)
  makes for `baseline`. The two are the node-level pair the Dockerfile strategy
  needs: a Kubernetes namespace relaxed enough to admit the builder's pod, and a
  node that lets the builder create the user namespace it runs in.
The platform namespace is **not** in that list — the chart creates and labels
`kitchen-system` itself; see [Install](#install).

cert-manager is **not** in that list either: the chart ships it as a sub-chart
(`cert-manager.enabled`, on by default), because Kitchen owns the cluster it is
installed into. Set `cert-manager.enabled=false` for a cluster that already
runs one.

Two optional features add a dependency, and both are the same arrangement.
**KEDA and its HTTP add-on**, if you want `scaleToZero.enabled`: they cannot be
sub-charts of anything — see [Scale to zero](#scale-to-zero) — so they go in as
two Helm releases of their own. And **CloudNativePG**, if you want the platform
to provision databases into this cluster rather than through a hosted provider
— see [Databases](#databases). Neither has to mean a command of yours:
`scaleToZero.install.enabled` and `databases.install.enabled` let the
**operator** run those installs itself, which is a thing a chart cannot do and
a controller can. Without either, the feature simply stays off; and where the
dependency is already in the cluster, the platform uses what it finds and
installs nothing.

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
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.acme.email=you@example.com \
  --set kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName=cloudflare-api-token
```

The default TLS mode is `acme`, and it is the two `kitchen.tls.acme` values
that make it work, so the chart requires them there: the API server refuses a
Kitchen in acme mode with no `acme` block, and a release that rendered one
would fail at the post-install hook rather than at the point the value was
missing. Neither is needed in the other modes — `--set kitchen.tls.mode=none`
brings a cluster up before DNS and certificates exist.

### The chart owns the namespace

`kitchen-system` is part of the release (`namespace.create`, on by default), so
its Pod Security level is set rather than inherited. That matters because the
telemetry agent mounts the node's log directory and root filesystem, and
`hostPath` is admitted at the `privileged` level alone — `baseline` forbids it
outright, so there is no narrower level that still collects anything. Clusters
differ in what they default to: kind is `privileged` and notices nothing, Talos
is `baseline` and the agent's pods are refused at admission with no pod ever
created. The namespace therefore carries
`pod-security.kubernetes.io/{enforce,audit,warn}` at `namespace.podSecurity`,
and setting that stricter while `collector.enabled` is true is refused at render
time rather than discovered later.

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

### What the platform namespace accepts

`networkPolicy.enabled` (on by default) puts a default-deny ingress
NetworkPolicy on `kitchen-system` and then allows traffic back, one rule per
thing that legitimately receives it. Without it, everything the platform runs
was one DNS name away from every application pod on the cluster — including a
preview built from a pull request nobody has reviewed — and that included the
identity provider's Postgres, the telemetry store, and the private listener
that enumerates accounts and mints CI keys for any project.

What is allowed, and to whom:

| Allowed | From | Why |
|---|---|---|
| The API and the dashboard (`api.port`), the git webhook receiver (`webhookReceiver.port`) | anywhere | published on the shared Gateway |
| The metrics endpoint (`metrics.port`) | anywhere | scraped by a Prometheus that is somewhere else by definition; protected by TokenReview when `metrics.secure` is on |
| The identity provider's published port (`auth.port`) | anywhere | the OIDC issuer, published on the Gateway |
| The registry (`registry.service.port`) | anywhere | [published on the internet on purpose](#why-it-is-published-on-the-internet-rather-than-reached-in-cluster) |
| The preview gate | anywhere | a proxy in the request path of a published URL |
| The telemetry agent's OTLP receivers | anywhere | every workload on the platform exports here; it is what the endpoint is for |
| The object store (`objectStore.service.port`) | the platform namespace, and namespaces carrying `kitchen.bermos.dev/project` | an `objectStore` claim hands an application pod this address |
| Everything else — ClickHouse, the identity provider's Postgres, `auth.internalPort`, the health ports | the platform namespace alone | nothing outside it has business there |

Three things about the shape are worth knowing before changing it:

- **It is ingress only.** There is no egress policy and adding one is not a
  small change: under Cilium the API server is reached as the `host` or
  `remote-node` identity, which an `ipBlock` does not match, so a default-deny
  egress with an `ipBlock: 0.0.0.0/0` escape hatch takes the operator's own API
  server connection out with it. The platform also talks to whatever git
  provider, ACME server, container registry and object store an installation
  points it at, which the chart cannot enumerate.
- **A published port is allowed from *everywhere*, not from a namespace.**
  Traffic off the shared Gateway has been proxied by Cilium's Envoy and carries
  the reserved `ingress` identity, which no `networking.k8s.io/v1` peer can
  select — not a namespace selector, not an `ipBlock`. An ingress rule with
  ports and no `from` matches every source, that one included. Nothing is given
  away: those ports answer the internet already. (This is also why the chart
  writes plain NetworkPolicy rather than CiliumNetworkPolicy, which *can* name
  that identity: a `cilium.io/v2` object cannot be rendered on a cluster
  without Cilium's CRDs, and would fail every `helm template` and every
  CI install that does not run Cilium.)
- **The bundled cert-manager is exempted whole**, because its admission webhook
  is called by the kube-apiserver — another identity a NetworkPolicy cannot
  name. A deny that caught it would stop every Certificate in the cluster being
  admitted.

Enforcement belongs to the CNI, not to the chart. Cilium is a prerequisite and
enforces `networking.k8s.io/v1` NetworkPolicy, so on a supported cluster these
are real. On a cluster whose CNI does not enforce NetworkPolicy the objects are
created and mean nothing, silently — which is the usual way a network policy
comes to protect nothing.

That is measured rather than reasoned about: CI installs this chart on a Cilium
cluster on every change, and from inside a real application namespace it asserts
that the telemetry store, the identity provider's Postgres and the private
`/kitchen` listener answer nothing at all, while the object store and the
issuer still answer. The job is *Chart install on Cilium* in
[`.github/workflows/helm.yml`](../../.github/workflows/helm.yml).

**Inter-application policy is deliberately not here.** Two applications in two
projects can still reach each other, and [docs/SCOPE.md](../../docs/SCOPE.md)
says why: Kitchen has no multi-tenant threat model. What this policy claims is
narrower and worth stating plainly — an application cannot reach the platform's
own stores. If you want app-to-app isolation as well, write it yourself against
the `kitchen.bermos.dev/project` label every application namespace carries.

Set `namespace.create=false` to manage the namespace yourself, in which case
its Pod Security labels are yours to get right.

#### Application namespaces have a level of their own

`kitchen-system` is not the only namespace whose Pod Security level has to be
set rather than inherited. Every project gets a namespace of its own —
`kitchen-<project>`, holding its builds, its environments' workloads and the
secrets behind its resource claims — and the operator labels those the same
three ways, at `kitchen.appNamespaces.podSecurity`.

`privileged` is the default because that is what the **Dockerfile build
strategy** needs. BuildKit runs rootless here, which means it creates a nested
user namespace and mounts its own overlayfs inside it: the runtime's default
seccomp profile blocks the first and the default AppArmor profile blocks the
second, so the builder asks for both unconfined — and Pod Security admits
neither below `privileged`.

The failure when it is not set is quiet, which is the reason this is a value
and not a footnote. A Job whose pods admission refuses **creates no pod at
all**: `kubectl get pods -n kitchen-<project>` is empty, `kubectl get jobs`
says `Running`, and the rejection is a `FailedCreate` event on the Job. On a
cluster defaulting to `baseline` — Talos does — every Dockerfile build hangs
there while buildpacks builds, which ask for neither relaxation, keep working.

Lower it to `baseline` on an installation that builds with buildpacks alone;
`kitchen.builds.defaultStrategy=dockerfile` with anything below `privileged` is
refused at render time. `restricted` additionally refuses most application
images, which pick their own user and capabilities, so it suits an installation
that vets what it deploys and little else.

The level is reconciled, not only written at creation: raising or lowering the
value and running `helm upgrade` relabels the namespaces that already exist on
each project's next reconcile.

This is one of two node-level facts the Dockerfile strategy depends on, and the
only one the operator can write. The other is the node's own
`user.max_user_namespaces`, which has to be above zero for rootless BuildKit to
create its user namespace at all and which no label reaches — see
[Prerequisites](#prerequisites). They fail differently and both fail quietly:
the level refuses the pod at admission, the sysctl lets the pod start and kills
the builder inside it seconds later.

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
`httpPort`, `nativePort`, `database`, `username`, `password`, `scheme`,
`caFile`, `dsn`), whether ClickHouse runs here or elsewhere, so the agent and
the operator have one place to look.

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

### How the store is reached

`clickhouse.tls.enabled` (on by default) is what stops the store answering in
plaintext. Until it existed, ClickHouse served plain HTTP on 8123 and the plain
native protocol on 9000, so every log line, every query and the store's own
password crossed `kitchen-system` readable — by anything that lands in the
namespace, and by the node. [What the platform namespace
accepts](#what-the-platform-namespace-accepts) closed that namespace to
applications; it did not encrypt what is inside it.

What it does:

- **The operator mints a CA**, through the cert-manager this chart already
  bundles: a self-signed `Issuer`, a `Certificate` with `isCA`, and a second
  `Issuer` that signs with the result — all three in `kitchen-system`, all
  three created by the operator rather than by this chart, because
  cert-manager's own webhook admits them and on a first install they cannot
  exist until it is serving.
- **ClickHouse gets a certificate from it**, for every name a client in the
  cluster reaches its Service by, mounted into the pod. The store then serves
  the HTTP interface on `clickhouse.tls.httpsPort` (8443) and the native
  protocol on `clickhouse.tls.nativePort` (9440), and **the plaintext
  listeners are removed from its configuration** rather than left unused: 8123
  and 9000 are refused connections, not quieter ones.
- **The platform's own clients verify it**, hostname and chain, against the CA
  — `verify-full`, not `require`. They can, because they are the platform's:
  the operator publishes the CA certificate (and never its private key) as the
  ConfigMap `kitchen-internal-ca`, and the operator's own pod and the telemetry
  agent mount it. There is no setting anywhere in the platform that connects to
  this store without verifying it.

Two things follow that are easy to be surprised by:

- **It has nothing to do with `kitchen.tls.mode`.** That decides how the
  platform is published to the internet. This decides whether two pods in one
  namespace talk in the clear, and the CA signs itself, so an installation on
  `tls.mode: none` — no ACME account, no wildcard, no DNS — still gets all of
  it.
- **The store's pod, and the telemetry agent's, wait for it.** On a first
  install ClickHouse sits in `ContainerCreating` until the operator has created
  the CA and cert-manager has issued from it, and every node's telemetry agent
  waits the same way for the CA bundle. Both are seconds and need no ordering
  from you — the operator issues from a connection secret rather than from the
  Kitchen singleton, which is a post-install hook and would otherwise be
  waiting for the store that is waiting for it. It is a wait rather than a
  fallback on purpose: there is no state in which the store answers in the
  clear because its certificate was late, or the agent ships a log line in the
  clear because the CA was.

`kubectl exec` into the pod reaches it with `clickhouse-client --secure`; add
`--accept-invalid-certificate` for a connection to `127.0.0.1`, which no
certificate names. The CA is inside the pod at
`/etc/clickhouse-server/tls/ca.crt` if you would rather check it.

An **external** store is somebody else's certificate to manage, so the operator
issues nothing for it: set `clickhouse.external.tls=true` when it serves TLS
signed by a CA the platform's components already trust, and the platform
verifies it against the host's roots. Left off, the connection is plaintext and
the platform says so rather than pretending otherwise — see below.

**Turning it off is the one way to run the bundled store in plaintext**, and
the platform will not be quiet about it: `clickhouse.tls.enabled=false` leaves
`InternalCAReady` False on the Kitchen singleton with reason `StoreInTheClear`,
naming what is readable. That condition does not hold the platform short of
Ready — it is a choice somebody made — but it is in the list an operator reads,
beside a healthy `internal-ca` row in `status.components` on an installation
where the CA did issue.

## The telemetry agent

One OpenTelemetry collector runs on every node, and it is the only way any
signal enters the platform. It tails every container log file the kubelet
writes, scrapes the kubelet and the node itself, and receives OTLP from
applications and from the operator — writing all of it straight into ClickHouse.

Kitchen's labels become **resource attributes**, and the schema materializes a
column out of each, so any row can be traced back to what produced it:

| Attribute | Column | From |
|---|---|---|
| `kitchen.project` | `project` | the pod's `kitchen.bermos.dev/project` label, falling back to the **namespace**'s — so a claimed database or a sidecar nobody labelled still belongs to its project |
| `deployment.environment.name` | `environment` | `kitchen.bermos.dev/environment` (production or a preview) |
| `kitchen.build` | `build` | `kitchen.bermos.dev/build` |
| `kitchen.source` | `source` | `platform` in `kitchen-system`, else `build` if there is a build label, else `runtime` if there is a project or environment, else `cluster` |
| `k8s.namespace.name`, `k8s.pod.name`, `k8s.container.name`, `k8s.node.name` | `namespace`, `pod`, `container`, `node` | the pod the signal came from |

`cluster` is not a euphemism for "unlabelled Kitchen thing". The agent tails
every container on the node, so a line with no Kitchen label is Cilium,
cert-manager, a CSI sidecar — somebody else's. They are still collected, because
a sick node is exactly when Kitchen looks broken, but under a name that says
whose they are.

The tables — `otel_logs`, `otel_traces`, the five `otel_metrics_*` and the
five-minute rollup, plus `events` for the activity feed and `flows` for the
traffic view — are created by the **operator**, not the chart and not the agent
(`create_schema` is off). They are applied from
`Kitchen.spec.observability.clickhouse`, and their TTL follows `retentionDays`,
so changing retention in the UI or with `kubectl` is enough:

```sh
kubectl patch kitchen default --type=merge \
  -p '{"spec":{"observability":{"clickhouse":{"retentionDays":7}}}}'
```

Watch it take: `kubectl get kitchen default -o jsonpath='{.status.conditions}'`
carries a `TelemetrySchemaReady` condition.

That ownership makes the first install ordered: with `create_schema` off the
exporter opens the configured database directly instead of `default`, so until
the operator has reconciled once there is nothing for the agent to connect to
and it retries. **A collector that restarts a few times on a fresh cluster and
then settles is that ordering, not a fault.**

Ask the store what a build did:

```sql
SELECT Timestamp, Body FROM otel_logs
WHERE build = 'shop-bld-8f3a2c1d0abc' ORDER BY Timestamp
```

or what an app is doing right now:

```sql
SELECT Timestamp, container, Body FROM otel_logs
WHERE project = 'shop' AND environment = 'shop-production'
  AND Timestamp > now() - INTERVAL 15 MINUTE
ORDER BY Timestamp DESC
```

Build logs outlive the pods that wrote them: the build Job keeps its finished
pod for an hour so the agent can catch up, and the lines are in ClickHouse long
before that.

### What a log line carries

A JSON line's own fields are parsed out and land in `LogAttributes`, flattened
with dots — so `http.status` is a map lookup rather than a substring search of
the message. The body is kept as it was written either way, and a line that is
not JSON, or is malformed JSON, still ships with its message intact.

The count is capped at `collector.logs.maxStructuredFields` (64). A line
carrying hundreds of fields is a dump rather than a log, and letting one widen
the map would put its keys on every line written beside it; over the cap the
line ships with its body and no attributes.

Two things are lifted out of those fields into columns of their own:

- **Severity.** `SeverityText`/`SeverityNumber`, read from the line's `level`,
  `severity` or `lvl` field, first match wins
  (`collector.logs.levelFields`). A line that is not JSON is scanned for the
  common spellings instead. One that says nothing recognisable keeps no
  severity — honest, rather than guessed at from the stream. The text is
  normalised to one spelling per severity, so `SeverityText = 'warn'` means
  every warning however it was written.
- **Trace and span ids.** `TraceId`/`SpanId`, from `trace_id`, `traceId`,
  `trace.id` and the other usual spellings
  (`collector.logs.traceIdFields`/`spanIdFields`). The name belongs to whichever
  instrumentation library the application uses, not to the application. This is
  what makes the dashboard's link between a log line and the request it came
  out of work in both directions.

Both leave the original field in place: a line's expanded fields should not
disagree with the JSON it was parsed from.

### What is not collected

**Node and system logs.** The agent collects container logs only — nothing from
the systemd journal, the kernel or the kubelet's own service. This is a
decision, not a gap:

- The stock `otel/opentelemetry-collector-contrib` image is built `FROM
  scratch`. The journald receiver works by shelling out to `journalctl`, which
  is not in the image.
- A receiver that returns an error from `Start()` **aborts the whole
  collector**, so a journald receiver on a node without journald would take the
  log, metric and OTLP pipelines down with it. It cannot be added "just in
  case".
- Talos, which Kitchen targets, has no journald at all.

If a node's own logs are what you need, they are on the node.

### Narrowing what is collected

By default every pod on every node is collected, including the platform's own
components — that is what makes the store useful when Kitchen itself
misbehaves. To reduce the volume:

```sh
--set 'collector.logs.excludeNamespaces={kube-system}'   # skip a namespace
--set collector.logs.enabled=false                       # no container log files at all
--set collector.metrics.node.enabled=false               # no node metrics
--set collector.enabled=false                            # no agent
```

There is no pod label or field selector. The agent chooses log *files* by path,
not pods by query, and a file's path carries only the namespace, pod name, uid
and container. `collector.logs.exclude` takes raw file globs for anything a
namespace does not express.

Note what `collector.enabled=false` costs, since the DaemonSet is no longer just
a log collector: no container logs, no kubelet or node metrics, and **no OTLP
endpoint** — applications and the operator both lose the address they export to.
With no telemetry store configured it is not rendered at all anyway.

### Sizing the agent

Requests are 100m CPU and 256Mi; the limit is 1Gi with `GOMEMLIMIT` at 800MiB,
which is the same number expressed to the Go runtime.

The two halves are what make the limit a limit rather than a cliff. The
`memory_limiter` processor refuses new data at 80% of the container's memory,
and the export queue is bounded at `collector.export.queueSize` signals, so the
behaviour under a backlog or a wedged store is **dropped signals** — visible,
bounded, self-correcting once the store returns. The collector this replaced had
neither, and its failure under the same conditions was an OOM kill before it
could commit a read offset, then a restart that re-read the same backlog: a loop
that shipped every line dozens of times rather than falling behind honestly.

So if the agent is dropping data, raising the memory limit is the second thing
to try. The first is `collector.export.queueSize` and
`collector.export.batch.minSize`, because a queue that empties too slowly is
usually a store that is too slow, not an agent that is too small. Raise
`goMemLimit` with the limit whenever you do change it — a limit the runtime does
not know about is a limit the kernel enforces.

### If nothing arrives

Check the agent is actually running, which is not the same as checking that it
was installed:

```sh
kubectl -n kitchen-system get ds -l app.kubernetes.io/component=collector
```

`DESIRED 1, AVAILABLE 0` with no pod anywhere means its pods are being refused
before they are created — Pod Security is the usual reason, and the rejection is
recorded as a `FailedCreate` event on the DaemonSet rather than as a pod in a
bad state:

```sh
kubectl -n kitchen-system describe ds -l app.kubernetes.io/component=collector
```

```
Warning  FailedCreate  daemonset-controller  Error creating: pods "kitchen-collector-gc9m4" is
forbidden: violates PodSecurity "baseline:latest": hostPath volumes (volumes "state", "var-log", "hostfs")
```

Fix it by labelling the namespace as described under
[Prerequisites](#prerequisites), then `kubectl -n kitchen-system rollout restart
ds -l app.kubernetes.io/component=collector`. The same message is on the Kitchen
object's `collector` component, which is where to look first.

If the pods are running and the store is still empty, check the schema is there
— the agent does not create it:

```sh
kubectl get kitchen default -o jsonpath='{.status.conditions[?(@.type=="TelemetrySchemaReady")].message}'
kubectl -n kitchen-system logs ds/kitchen-collector --tail=50
```

## Resource metrics

CPU and memory for every application container, and the fill level of every
volume its pods have mounted, come off the node's kubelet, scraped by the agent
every `collector.metrics.intervalSeconds`. Nothing in the API server knows how
full a volume is, so per-PVC usage arrives this way or not at all. What no
receiver can see — restart counts, OOM kills, the limits a release configured,
how many pods an environment is running — the operator reads from the API server
and **exports to the agent over OTLP**, like any other workload. Both halves
land in the same `otel_metrics_*` tables, on rows carrying the same project and
environment, which is what turns the dashboard's environment page from an
instant into a history: whether it was always using this much memory, when it
scaled, whether it was OOMKilled overnight.

Nothing else is installed for it. There is no Prometheus, no kube-state-metrics
and no scrape configuration, because the join a scrape pipeline would need —
from `(namespace, pod, container)` back to the project and environment that own
the pod — lives in the pod's labels, and the agent already applies them to every
signal it touches.

```yaml
kitchen:
  observability:
    metrics:
      enabled: true          # the operator's half: restarts, OOM kills, limits, replicas
      intervalSeconds: 30
collector:
  metrics:
    kubelet:
      enabled: true          # CPU and memory per pod and container, and volume usage per PVC
    node:
      enabled: true          # the node itself: CPU, load, memory, network, disk, filesystem
    intervalSeconds: 30
```

The interval is the one knob worth touching. Below 30s the row count climbs
faster than the answers improve; much above a minute and a short spike falls
between two samples and is never seen. A window wider than a few hours is drawn
from a five-minute rollup the store maintains itself, so widening the range
costs the same as narrowing it.

The agent reads the kubelet on its own node, over the kubelet's own port, and
never through the API server's proxy to it. Two grants cover that: `nodes/stats`
for the summary endpoint every metric is scraped from, and `nodes/proxy` for
`/pods`, which is the only place the claim behind a mounted volume is named —
without it a filling PVC can be measured but not identified, and every
configMap and projected-token mount looks like a volume worth charting. The
kubelet maps `/pods` to the narrower `nodes/pods` only where the
`KubeletFineGrainedAuthz` feature gate is on (1.33 and later) and admits the
request if either grant allows it, so the ClusterRole carries both.

Turning `collector.metrics.kubelet.enabled` off leaves the CPU, memory and
volume series empty; restarts, limits and replicas keep arriving, because they
come from the API server either way.

## Traces

Traces are the one telemetry the platform cannot collect on an application's
behalf. Logs are on the node, flows are in the CNI, resource usage is in the
kubelet — but only the application knows that this request was a checkout and
that it spent 380 of its 420 milliseconds waiting for a database.

So the agent runs the OTLP receiver — OTLP/HTTP on 4318 and OTLP/gRPC on 4317 —
the chart puts a ClusterIP Service in front of it, and the operator hands every
environment its address through OpenTelemetry's own environment variables:

```
OTEL_EXPORTER_OTLP_ENDPOINT=http://kitchen-otlp.kitchen-system.svc.cluster.local:4318
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_SERVICE_NAME=<project>
OTEL_RESOURCE_ATTRIBUTES=service.name=<project>,kitchen.project=<project>,kitchen.environment=<environment>,deployment.environment.name=<environment>
```

Which means instrumenting an application is adding its language's SDK and
nothing else — no endpoint to look up, no collector to deploy, no resource
attributes to remember. Node:

```sh
npm install @opentelemetry/api @opentelemetry/auto-instrumentations-node
NODE_OPTIONS="--require @opentelemetry/auto-instrumentations-node/register"
```

An application that sets any of those variables itself wins: the platform's go
in first, and the kubelet takes the last value of a repeated name.

That one name reaches the agent on the caller's **own node**: the Service is
`internalTrafficPolicy: Local`, so kube-proxy answers with the local endpoint
and the traffic never leaves the node. The cost is that when no agent pod is
Ready there — during a rolling update of the DaemonSet, or on a node the agent
does not tolerate — there is no local endpoint and the connection is dropped
rather than falling back to another node. This was chosen over putting a gateway
Deployment in front of the agents, which trades that window for a second hop, a
second copy of every signal in flight and a component to scale. It applies to
the operator too: it exports its own usage metrics to this Service from whichever
node it happens to run on.

The receiver deliberately has no HTTPRoute. Spans come from workloads already
inside the cluster, and an OTLP endpoint on the public Gateway would be an
unauthenticated write surface on the telemetry store. `traces.endpoint` exists
for putting something else — a sampling collector, a second backend — in front
of it.

Log lines carry the trace id they were written under whenever the application
logs one; see [What a log line carries](#what-a-log-line-carries).

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
`username`, `password`, `sslmode`, `caFile`, `dsn`). Point at an existing
Postgres instead:

```sh
--set postgres.enabled=false \
--set postgres.external.host=postgres.databases.svc \
--set postgres.auth.password=<password>
```

Install without an identity provider — no login for the UI, no issuer for apps
— with `--set auth.enabled=false --set postgres.enabled=false`.

### How the accounts database is reached

`postgres.tls.enabled` (on by default) is what stops the identity provider's
database answering in plaintext. Until it existed this was the stock Postgres
image with `ssl` off, so every session, every OAuth client secret and the
database's own password crossed `kitchen-system` readable — by anything that
lands in the namespace, and by the node. [What the platform namespace
accepts](#what-the-platform-namespace-accepts) closed that namespace to
applications; it did not encrypt what is inside it.

It is the same machinery as [the telemetry store's](#how-the-store-is-reached),
and the same CA:

- **The operator issues the database a certificate** from the platform's
  internal CA, for every name a client in the cluster reaches its Service by,
  and the pod mounts it. The chart asks for it by naming a Secret in the
  connection secret's `certificateSecret`; the operator fills it.
- **The server refuses plaintext**, rather than merely offering TLS. Its
  host-based rules come from a ConfigMap this chart owns — `hostssl` for
  everything over TCP and no `host` line at all — so a client that will not do
  TLS is turned away with *no pg_hba.conf entry … SSL off*. The Unix socket
  stays open and unencrypted: it is inside the pod's own filesystem, it is how
  the image initialises the cluster, and it is what all three probes use. That
  is also why `kubectl exec … psql -U kitchen kitchen_auth` still works.
- **Every client verifies it** — hostname and chain — against the CA the
  operator publishes as the ConfigMap `kitchen-internal-ca`: the identity
  provider, the operator (which dumps this database on the backup schedule),
  a scheduled backup's pod and the restore Job. The DSN says so once, in
  `sslmode=verify-full&sslrootcert=…`, and they all read that same DSN.

**`verify-full` and nothing weaker, on purpose.** Two drivers connect to this
database and they do not agree about the weaker modes: the operator's is
libpq's (pgx), where `require` encrypts without verifying anything, while the
identity provider's is node-postgres, where `require`, `verify-ca` and
`verify-full` all verify and only its own `no-verify` does not. `verify-full`
is the one mode that means the same thing to both.

**The pods wait for their material rather than starting without it.** On a
first install Postgres sits in `ContainerCreating` until the operator has
created the CA and cert-manager has issued from it, and the identity provider
waits the same way for the CA bundle. Both are seconds. It is a wait rather
than a fallback on purpose: there is no state in which this database admits an
unencrypted connection because its certificate was late, and none in which the
identity provider connects without verifying because the CA was.

An **external** Postgres is somebody else's certificate to manage, so the
operator issues nothing for it. `postgres.external.sslmode` is what its clients
ask for — `verify-full` is the value that behaves the same under both drivers,
and verification is then against the host's roots, since there is nowhere here
to mount a CA of your own. Left empty, the connection is plaintext and the
platform says so rather than pretending otherwise.

**Turning it off is the one way to run the bundled database in plaintext**, and
the platform will not be quiet about it: `postgres.tls.enabled=false` leaves
`InternalCAReady` False on the Kitchen singleton with reason `StoreInTheClear`,
naming what is readable.

### Who owns the platform

An account is either an **operator** — everything, everywhere, plus `admin` on
every project — or an ordinary **member**. The list lives on the `Kitchen`
singleton, under `spec.access.operators`, and is edited from the settings
screen (`PATCH /settings`) once somebody holds the role.

Somebody has to hold it first, and there are two ways that happens:

- **The bundled identity provider seeds it.** With no list at all, the operator
  seeds one from the accounts that exist — the account the bootstrap link
  created on a fresh install, every existing account on an installation
  upgrading into enforcement — and then never touches it again. An empty list
  is a decision and is left alone; an absent one is what "nobody has said yet"
  looks like.
- **The chart names it**, with `kitchen.access.operators`:

  ```sh
  --set kitchen.access.operators[0]=anna@example.com \
  --set kitchen.access.operators[1]=bo@example.com
  ```

  Each entry is an email address, the issuer's opaque `sub`, or a map of
  `subject` and an informational `email`. An address is honoured only for a
  token whose `email_verified` claim is true.

**On a federated issuer the second one is not optional.** Seeding reads
Kitchen's own account directory, which the bundled provider serves and a
Keycloak or an Auth0 does not — OpenID Connect defines no way to enumerate
accounts. Point the platform at one without naming operators and nothing is
ever written: every account is a member, every operator-only route refuses
everybody, and that includes the `PATCH /settings` that would name an
operator. The singleton says so rather than leaving it to be discovered:

```sh
kubectl get kitchen default \
  -o jsonpath='{.status.conditions[?(@.type=="OperatorsConfigured")].message}'
```

An installation that has already got itself into that state recovers with an
upgrade that both names the operators and re-applies the singleton:

```sh
helm upgrade kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --reset-then-reuse-values \
  --set kitchen.applyOnUpgrade=true \
  --set kitchen.access.operators[0]=anna@example.com
```

Once the value is set, the chart is the source of truth for who owns the
platform on every upgrade that re-applies the singleton: somebody added on the
settings screen and not added here is removed again. Left empty, an upgrade
preserves whatever list is live — see [Upgrade](#upgrade).

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

## The bundled registry

A build has to push somewhere. Kitchen runs the somewhere: a
[zot](https://zotregistry.dev/) registry with a PVC, published at
`registry.<baseDomain>`, and a `dockerRegistry` Connection the operator seeds
to point at it. `helm install`, open the dashboard, create a project from a
repository, and the first build pushes and deploys — no registry account, no
credential, no `kubectl`.

### Why it is published on the internet rather than reached in-cluster

**The kubelet pulls, not the pod.** The node's container runtime resolves and
pulls every image, and it does not care what the cluster thinks of a
certificate: a Service address with an in-cluster CA fails at the node, and
plain HTTP fails for the same reason in reverse. Everyone who has solved this
solved it by configuring the node — an insecure-registry entry on a localhost
NodePort (MicroK8s, minikube), a CA pushed into CRI-O's trust store
(OpenShift), a DaemonSet writing `/etc/containerd/certs.d` (Spegel) — and all
of that is fine for a distribution that owns its nodes. Kitchen is a chart
installed into someone else's cluster; Cilium and a StorageClass are the only
prerequisites, and node configuration has never been one.

A route on the shared Gateway uses the platform's own wildcard certificate,
which is publicly trusted, so **every node already trusts it with nothing
configured**. The costs, stated up front:

- Image pulls leave the cluster and come back in through the Gateway.
- The registry is reachable from outside, so its credential is doing real work
  rather than being a formality: it admits nobody anonymously.
- It needs the platform to actually have TLS. In `kitchen.tls.mode=none`
  nothing is rendered and nothing is published; `RegistryReady` on the Kitchen
  object is False with the reason `TLSModeNone`, and projects need a registry
  connection of their own.

`registry.<baseDomain>` resolves through the same wildcard DNS record as every
other generated URL, and is covered by the same wildcard certificate.

Two things follow from the traffic being external that are worth knowing before
you rely on it:

- **The push is external too.** A build pod resolves `registry.<baseDomain>`
  and connects to the Gateway's own address from inside the cluster, so the
  cluster has to be able to reach its own load balancer. Most CNIs hairpin
  this without being asked and cloudflared makes it a genuine round trip, but a
  cluster that cannot resolve its own base domain from the inside — the same
  case `kitchen-auth`'s `internalURL` exists for — cannot use this registry.
- **cloudflared caps request bodies.** A tunnel puts Cloudflare's proxy in
  front of every push, and its body limit (100 MB on the free plan) applies to
  image layers like anything else. A build whose layers exceed it needs a
  registry reached some other way.

### Why zot

"Lightweight" and "garbage collection" turn out to be the same question. Every
build pushes a tag and nothing else ever deletes one, so the registry that
matters is the one that can reclaim disk **while running**: Distribution's
garbage collector requires stopping the registry, and Harbor brings Postgres,
Redis and Trivy to bundle. zot is a single Go binary with no database, is a
CNCF sandbox project, speaks OCI 1.1, and enforces retention policies online.

The default policy keeps the 20 most recently pushed tags per repository and
anything pushed in the last 30 days, whichever is more, and deletes untagged
manifests. Tune it, or turn it off and watch the volume yourself:

```sh
--set registry.retention.keepTags=50 \
--set registry.retention.keepPushedWithin=2160h \
--set registry.persistence.size=100Gi
```

### Why it is told to accept Docker media types

zot is OCI-native, and answers `415 Unsupported Media Type` to a manifest media
type it was not configured to admit. Both of Kitchen's build strategies push
Docker manifest schema 2
(`application/vnd.docker.distribution.manifest.v2+json`) by default: the Cloud
Native Buildpacks lifecycle's exporter writes it and offers no option, and
BuildKit switches to OCI media types only when an attestation asks it to. So
the rendered configuration carries `http.compat: ["docker2s2"]`, without which
every build fails at its last step — after the image is built and every layer
uploaded — with `MANIFEST_INVALID`, against a registry whose pod, probes and
route all read as healthy.

It is not a value, because there is no installation that wants it off: the
setting admits Docker's media types **in addition to** OCI's, and the registry
exists to be pushed to by these two builders.

### The seeded connection

The operator creates the Connection **once** and remembers in
`status.registry.connection` that it did. Delete it and it stays deleted: an
installation that would rather use Harbor or GHCR should be able to end up with
only its own connections, and a platform that kept reinstating this one would
make that impossible. While it is there and still labelled
`app.kubernetes.io/managed-by: kitchen`, its URL and credential are kept in
step — a base domain that moves or a password that rotates would otherwise
leave it quietly wrong.

The route and the Connection are the operator's rather than this chart's, for
the same reason as the preview gate: the shared Gateway is created by a
reconciler, and a credential the API never reads back has to be written by
something inside the cluster. So they appear a reconcile after install rather
than with the release:

```sh
kubectl get kitchen default -o jsonpath='{.status.conditions[?(@.type=="RegistryReady")].message}'
kubectl -n kitchen-system rollout status statefulset/kitchen-registry
```

### Bringing your own

```sh
--set registry.enabled=false
```

Turning it off deletes the route and the seeded Connection with it — a
connection naming a registry nothing serves is a picker entry that fails every
build chosen with it. Add your own registry on the connections page; nothing
downstream treats the bundled one as a special case.

## The bundled object store

An application has to have somewhere to put a file it did not build into its
image — user uploads, generated exports — and the container filesystem loses
it on the next deploy. Kitchen can run the somewhere: a single
[MinIO](https://min.io/) with a PVC, and an `s3` Connection the operator
seeds to point at it. It is **off by default** — it is a workload with a
volume, and an installation that already has S3 or R2 brings it as a
Connection of its own:

```sh
--set objectStore.enabled=true
```

An `objectStore` claim then becomes a bucket in it, with a user and a policy
scoped to that bucket minted from the store's root credential — a bucket per
claim, never a prefix in a shared one, and no application is ever handed the
root. Each preview environment gets an empty bucket of its own, torn down
with the preview.

### How applications reach it

`objectStore.tls.enabled` (on by default) is what stops the store answering in
plaintext. Until it existed, MinIO served plain HTTP on 9000 — so every object,
every upload and every bucket credential crossed the wire readable, and this is
the one bundled store [the platform namespace
accepts](#what-the-platform-namespace-accepts) traffic to from application
namespaces, so that was between two namespaces as well as inside
`kitchen-system`.

What it does:

- **The store gets a certificate from the platform's internal CA** — the same
  self-signed root, CA certificate and CA issuer the [telemetry
  store](#how-the-store-is-reached) uses, minted by the operator through the
  bundled cert-manager — issued for every name a client in the cluster reaches
  its Service by, `kitchen-objectstore.kitchen-system.svc.cluster.local`
  included, because that is the address an application's binding carries.
- **MinIO serves TLS on the one port it listens on.** There is no second,
  plaintext listener to remove: a certificate in the directory `--certs-dir`
  names is what makes 9000 TLS, and a client that will not do TLS is refused
  rather than served. cert-manager writes `tls.crt` and `tls.key`, MinIO reads
  `public.crt` and `private.key`, so the volume projects the two under the
  names the server looks for.
- **The store's pod waits for its certificate** rather than starting without
  one. On a first install that is a wait of seconds and never a fallback: at no
  point does this store answer a plaintext request because its certificate was
  late.
- **The platform's own clients verify it** — hostname and chain, against the CA
  — because they are the platform's and can carry the bundle: the operator
  provisions every bucket over it, and a scheduled backup uploading to this
  store verifies it the same way.
- **Every application is handed the CA in its binding.** An application pod
  cannot mount a ConfigMap in `kitchen-system`, and no image the platform did
  not build has a private root in its trust store — so the binding Secret
  carries a `caCert` key holding the CA certificate itself, and an S3 client
  configured with it gets the same `verify-full` the platform's own components
  do. It is absent, not empty, for a store whose certificate a public root
  already vouches for.

None of it depends on `kitchen.tls.mode`: that decides how the platform is
published to the internet, and this decides what two pods in one cluster say to
each other. `objectStore.tls.enabled=false` is the one way to run the bundled
store in plaintext, and leaves `InternalCAReady` False on the Kitchen singleton
with reason `StoreInTheClear`, naming what is readable.

### Why it is not published on the Gateway

The registry has to be, because the node's container runtime pulls images
and trusts nothing the cluster says about a certificate. Nothing like that is
in the path here: an application runs in the cluster and reaches the store at
`kitchen-objectstore.kitchen-system.svc.cluster.local:9000`, on a Service
address nothing outside can reach — so there is no hostname to publish, and it
works in `kitchen.tls.mode=none`, where the store still serves TLS from the
platform's own CA.

The corollary is stated rather than hidden: **a bucket in the bundled store
cannot be publicly readable**, because there is no public to read it. A claim
that asks for `publicRead` is refused with that reason rather than granted a
policy that publishes nothing; serve the objects through the application, or
claim through an `s3` connection to a store that is on the internet.

### The seeded connection

The operator creates the Connection **once**, remembers in
`status.objectStore.connection` that it did, and leaves a deleted one deleted
— the same terms as the registry's. While it is there and still labelled
`app.kubernetes.io/managed-by: kitchen`, its endpoint and credential are kept
in step. The root secret key is generated on install and read back on
upgrade, so it stays stable; rotating it would invalidate the seeded
connection and stop every new bucket from being provisioned. `helm template`
cannot read it back, so set `objectStore.auth.secretAccessKey` when rendering
offline.

```sh
kubectl get kitchen default -o jsonpath='{.status.conditions[?(@.type=="ObjectStoreReady")].message}'
kubectl -n kitchen-system rollout status statefulset/kitchen-objectstore
```

### Not in the backup

The store's volume is not in the platform's backup archive, any more than the
registry's is — see [BACKUP.md](../../docs/BACKUP.md). An installation that
keeps production uploads here backs the volume up itself, or claims through a
store that is backed up already.

## Scale to zero

An environment nobody is using can drop to no pods at all, and the next request
to its URL starts it again. That is what makes a dozen open pull requests
nearly free: a preview costs a URL and a Deployment record until someone opens
it.

**An idle environment stops doing everything, not only serving.** There are no
pods, so there is nothing to run a background loop, a poller, a scheduler or an
ingest job either — and the next request restarts them from wherever they were
when the environment went quiet. For an application that works only when asked,
that is exactly right and costs nothing. For one that does work nobody
requested, it is a silent hole: the environment comes back, serves, and reports
nothing wrong, while whatever it was collecting has a gap in it that looks
exactly like the upstream having been down. Previews idle by default, and a
preview pointed at a real datastore will quietly write a partial record of a
period it was asleep for.

A project whose workload is not request-driven says so, and then none of its
environments idle — previews included:

```yaml
spec:
  runtime:
    notRequestDriven: true
```

The environment says which of the two it is in its `ScaleToZero` condition, so
"this one does not idle" is never something to be worked out from the absence
of an `HTTPScaledObject`. Worker Deployments from `spec.processes` are outside
this entirely: they serve no HTTP, so a request-driven idling policy has
nothing to say about them either way and they keep their replicas.

It is off by default, and it needs [KEDA](https://keda.sh) and its
[HTTP add-on](https://github.com/kedacore/http-add-on) in the cluster. They go
in as their own two Helm releases, which is how upstream ships them and — see
[why they are not sub-charts](#why-keda-is-not-a-sub-chart) — the only way Helm
can install them at all.

**Either the platform installs them, or you do.**

### Letting the platform install them

```sh
--set scaleToZero.enabled=true
--set scaleToZero.install.enabled=true
```

The operator then runs those same two installs itself, as a Job in
`kitchen-system`: KEDA first, waited for, then the add-on — the ordering Helm
cannot express inside one release but a controller can simply do. The versions
are pinned by the operator, not floated, so the pair that goes in is the pair
that release was tested with; `scaleToZero.install.version` and
`.addOnVersion` override them, and `.chartRepository` points at a mirror for a
cluster that cannot reach `kedacore.github.io`. Which entries the operator can
install at all is a catalogue compiled into it: an addon names an entry and a
namespace, never a chart, because its install job can apply CRDs and
ClusterRoles.

It is off by default because the job is bound to **cluster-admin** — installing
KEDA applies CRDs, ClusterRoles and a namespace — so `install.enabled` creates
a ServiceAccount of its own with that binding, the way `selfUpdate.enabled`
does. The grant is one object, revocable by turning the value off, and gone
when the release is. Nothing from an API request ever reaches that job's
command line: the operator builds it from its own configuration.

The value is the *grant* — "the operator may hold an account that can install
this" — and the operator then seeds a `keda` **Addon**, which is the *request*.
Both are required, and either can be withdrawn on its own; an Addon somebody
deletes stays deleted.

Progress and outcome are on that Addon, and read on **Platform → Addons**:

```sh
kitchen api GET /addons/keda
```

`managed` is the important half: `true` where the platform installed KEDA and
may upgrade it, `false` where it found KEDA already there and will never write
to it. The singleton keeps a `ScaleToZeroReady` roll-up of the same verdict.
See [On a cluster that already runs KEDA](#on-a-cluster-that-already-runs-keda).

### Installing them yourself

```sh
helm repo add kedacore https://kedacore.github.io/charts
helm install keda kedacore/keda --namespace keda --create-namespace
helm install keda-add-ons-http kedacore/keda-add-ons-http --namespace keda \
  --set interceptor.readinessTimeout=90s
```

Then point Kitchen at them — the defaults already match the commands above:

```sh
--set scaleToZero.enabled=true
--set scaleToZero.interceptor.namespace=keda
```

Turning the switch on without them costs nothing: every environment stays on
plain Deployment routing and says so in its `ScaleToZero` condition. The
operator never routes an application through an interceptor it cannot find.

**On an installation that already exists, `--set scaleToZero.enabled=true`
alone changes nothing.** The switch lives on the Kitchen singleton, which is a
post-install hook: `helm upgrade` deliberately leaves it alone so that edits
made in the UI survive (see [Values](#values), `kitchen.applyOnUpgrade`). Add
`--set kitchen.applyOnUpgrade=true` to let the chart own it, or turn it on
where it lives:

```sh
kubectl patch kitchen default --type=merge \
  -p '{"spec":{"scaleToZero":{"enabled":true}}}'
```

Either way the operator re-reconciles every Environment, because it watches the
singleton — the switch reaches environments that have no other reason to change.

Which environments actually idle is then each project's own decision:

```yaml
spec:
  scaleToZero:
    mode: previews     # previews (default) | always | never
    idleAfter: 5m      # quiet for this long, then no pods
    maxReplicas: 5     # ceiling once traffic arrives
```

`previews` idles preview environments and leaves production running — the
default, because an open pull request costs nothing while nobody is looking at
it and real users should never pay a cold start. `always` idles production too;
it is deliberately an opt-in, and until it is given a production environment
never drops below `spec.runtime.replicas`. A `maxReplicas` below the replica
count the environment already runs is raised to it, so idling can never shrink
an environment.

`spec.runtime.notRequestDriven` overrides all three: it is a statement about
what the workload *is* rather than about what it should cost, so it turns
idling off for previews as well as production and no `mode` re-enables it.
Reach for it rather than `mode: never` whenever the reason is "this
application does work nobody asked for" — the environment then says so, which
`never` cannot.

### Why KEDA is not a sub-chart

Kitchen bundles cert-manager rather than asking for it, so the obvious thing
would be to bundle these too. It cannot be done, and the reason is worth
writing down because it looks like a packaging preference and is not:

The HTTP add-on ships a `keda.sh/v1alpha1` **ScaledObject** of its own — it
autoscales its own interceptor fleet — while that CRD comes from the KEDA
chart. Helm builds and validates a release's entire manifest against the API
server *before* applying any of it, so a custom resource whose CRD arrives in
the same release can never resolve:

```
Error: INSTALLATION FAILED: unable to build kubernetes objects from release
manifest: resource mapping not found for name: "keda-add-ons-http-interceptor"
... no matches for kind "ScaledObject" in version "keda.sh/v1alpha1"
```

Neither escape hatch covers both paths. A `pre-install` hook is built *after*
the main manifest, so it fails identically. A `crds/` directory works on
install but is never applied on upgrade — the same reason Kitchen ships its own
CRDs as templates — so a KEDA version bump would leave stale schemas behind,
and keeping a second copy as templates makes Helm refuse the install for
ownership metadata it cannot template. A bundled copy also breaks the cluster
that already runs KEDA, since Helm will not adopt CRDs another release owns.

Two releases, as upstream documents them, has none of these problems: KEDA
upgrades its own CRDs through its own chart, and Kitchen's chart carries only
the address.

**None of this binds the operator**, and that is the whole of
`scaleToZero.install`. Every constraint above is Helm's: one release, one
manifest, validated in full before anything is applied. A controller installs
one release, waits for its CRDs to be established, and installs the next — the
same reason the cert-manager `ClusterIssuer` and the wildcard `Certificate` are
created by the operator rather than by a template. So the chart still bundles
nothing of KEDA's, and the platform can still end up having installed it.

### What the first request pays

An idling environment's URL does not point at the application any more: it
points at the add-on's **interceptor**, which holds the request, asks KEDA to
scale the workload up, and forwards it once a pod answers. That wait is the
cold start, and it is real — a few seconds where the image is already on the
node, considerably longer where it has to be pulled first. The interceptor
gives up after its `interceptor.readinessTimeout`, a value on the *add-on's*
release, so a genuinely broken environment fails visibly instead of hanging:

```sh
helm upgrade keda-add-ons-http kedacore/keda-add-ons-http --namespace keda \
  --set interceptor.readinessTimeout=3m
```

Protected previews compose with it: the route still goes to the forward-auth
gate, and it is the gate that is pointed at the interceptor. A visitor signs in
first and cold-starts the application second.

### Watching it

The Environment says what it decided and why:

```sh
kubectl get environment shop-pr-42 \
  -o jsonpath='{.status.conditions[?(@.type=="ScaleToZero")].message}'
kubectl -n kitchen-shop get httpscaledobject
```

Nothing here is allowed to take an application off the air. If the
`HTTPScaledObject` API is not served — the switch turned on without the
add-on, or the add-on still starting — the environment falls back to plain
Deployment routing with its own replicas and says so in that condition, rather
than parking behind an interceptor with nothing to wake it.

### On a cluster that already runs KEDA

Nothing to install — point `scaleToZero.interceptor.*` at the add-on you have.
Kitchen never touches KEDA's own objects, so the two installations stay
independent.

**This holds with `install.enabled` on, too.** Where the add-on's API is
already served, the operator installs nothing at all: it records the fact with
`managed: false` and leaves both releases alone for the rest of the
installation's life. An installation that would rather run its own KEDA — a
shared one, a pinned one, one its GitOps owns — has to be able to, and a
platform that upgraded it out from under you would be a worse neighbour than
one that never offered. The half-installed case is refused rather than guessed
at: KEDA present without its add-on gives a `ScaleToZeroReady` condition saying
so, because installing KEDA over a release the platform does not own is exactly
what it will not do.

Turning `scaleToZero.enabled` back off returns every environment to plain
Deployment routing on the next reconcile and deletes the scaled objects it
created. KEDA and the add-on are yours to uninstall separately, whenever.

## Databases

A `postgres` resource claim asks a connection to provision a database, and
there are two providers behind it: **Neon**, for a hosted one, and
**CloudNativePG**, for a database this cluster runs itself. The second exists
because a platform whose pitch is "bring your own Kubernetes cluster" should
not need a SaaS account before it can give an application a database — an
air-gapped installation, or one that will not put application data at a third
party, has one either way now.

Nothing in this chart is needed to *use* it. A `cnpg` connection provisions
into whichever CloudNativePG the cluster runs, whoever installed it, and it is
the one connection with no credential at all: the operator provisions with its
own service account, so there is nothing to store and nothing to rotate.

```sh
# on the connections page, or:
kitchen api POST /connections --data '{"name": "postgres", "provider": "cnpg"}'
```

### Letting the platform install CloudNativePG

If the cluster runs none, the operator can install it — the same shape, and the
same grant, as `scaleToZero.install`:

```sh
--set databases.install.enabled=true
```

That creates a ServiceAccount bound to **cluster-admin**, and the operator
seeds a `cloudnative-pg` **Addon** asking for the install. The value is the
*grant* — "the operator may hold an account that can install this" — and the
Addon is the *request*; both are required, and either can be withdrawn on its
own. The account is separate from the manager's so the grant is one visible
object, revocable by setting the value back to false and removed with the
release; the operator can only ever use it to run the one
`helm upgrade --install` that `internal/controller/addon_controller.go` builds
from the compiled catalogue, whose argv nothing from a request reaches. The
chart version is pinned in the operator rather than floated, next to the
catalogue of Postgres images a claim's extensions are promised from — the two
move together.

**On a cluster that already runs CloudNativePG this does nothing at all.** The
Addon records `status.managed: false` and the operator never writes to a
release it does not own — which is the same rule KEDA gets, with one difference
worth knowing: what the record decides is who may *upgrade* CloudNativePG, not
who may use it. Claims provision into it just the same.

The addons screen under **Platform → Addons** is where all of this is read and
changed; `GET /api/v1/addons` is the same answer for a terminal.

### Where the databases live, and what deleting one means

`databases.namespace` (default `kitchen-databases`) is where the provisioned
databases go. It is deliberately not a project's own namespace: deleting a
project deletes that one, and a claim under `deletionPolicy: Retain` has to
survive exactly that. Where CloudNativePG itself runs is the Addon's
`spec.namespace`, defaulting to upstream's own `cnpg-system`, so an
installation that later takes it over by hand finds it where the CloudNativePG
documentation says it will be.

Deleting a claim under `Delete` deletes the database and CloudNativePG collects
its volume with it. Under `Retain` — the default — the database stays where it
is, still holding its volume, and a claim of the same name created later
against the same connection finds it and rebinds. Retaining is not free: the
volume is still there and still costs whatever it costs.

**These databases are not in the platform's backup archive**, and that is worth
knowing before production data lands in one: the archive carries the custom
resources and the platform's own secrets, so a restore brings the *claim* back
and not the data behind it ([BACKUP.md](../../docs/BACKUP.md)). CloudNativePG
has its own backup machinery, and an installation keeping production data here
should point it somewhere.

### Asking for the Postgres you actually need

A claim can name a major version, the extensions its first migration will call
for, and the volume behind it. They are resolved to an image *before* anything
is created, and a claim naming an extension no image the platform can run
supplies is refused as a claim — `Failed`, with a message saying what could not
be supplied and what is available — rather than binding and letting the
application die on a `CREATE EXTENSION` three minutes into its first rollout.
Extensions are created at bootstrap as superuser, so an application never needs
the right to create them itself.

Out of the box that catalogue is CloudNativePG's own two image families: the
standard PostgreSQL images (which add pgaudit, pgvector and Postgres Failover
Slots to the contrib set) and the PostGIS build on top of them. An installation
that runs its own images says so on the connection's `config.images`, and its
claims are refused against that list instead — which is the operator's decision
and not the developer's, because asking for an extension should not be a way to
choose the image it arrives in.

### Previews get an empty database

There is no copy-on-write branch here. CloudNativePG's nearest equivalent is a
`pg_basebackup` of the parent, which is slow, doubles the storage, and — the
part that actually decides it — puts production data in a preview environment.
A preview's database is a fresh one with the same version, extensions and
storage as its parent and none of its data, and the claim says so:
`dataProvenance: synthetic` on that branch. That is what keeps production data
out of previews by construction rather than by policy, and it is what the
default policy bundle's `data-provenance-preview` rule reads.

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
collector       0/1     0 of 1 pods available: Error creating: pods "kitchen-collector-gc9m4" is
                        forbidden: violates PodSecurity "baseline:latest": hostPath volumes
controller      1/1
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
helm upgrade kitchen ./charts/kitchen --namespace kitchen-system --reset-then-reuse-values
```

`--reset-then-reuse-values` keeps existing overrides while adding defaults
introduced by the new chart version.

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

### Pinned policy bundles after an upgrade

An environment whose requirements pin a policy bundle pins it **by digest**,
and the built-in bundle's digest is the digest of the operator's compiled-in
rules. So a release that changes one of those rules changes the digest, and an
environment still pinned to the previous one no longer resolves: its promotions
answer *the environment's requirements could not be evaluated*, which is a
refusal to judge rather than a verdict — a bar that cannot be read is not a bar
that has been cleared.

Nothing is silently substituted, deliberately: the same rule is what stops a
ConfigMap bundle edited in place quietly changing what every environment pinned
to it demands. But it surfaces as promotions failing some time after the
upgrade, which is a long way from where the change was made.

After an upgrade, read `GET /api/v1/policy/bundles` — `kitchen api GET
/policy/bundles` reaches it from a terminal — for the `built-in` bundle's
current digest, and repin the environments whose digest has moved on each
environment's Requirements panel in the dashboard. Environments that pin
nothing are unaffected, and the release notes say when a release touched the
built-in bundle.

### Letting the platform update itself

Off by default. With it on, an upgrade is a button on the dashboard's settings
page instead of a command:

```sh
helm upgrade kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --reset-then-reuse-values \
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

### Backup and restore

Taking a backup is a button on the dashboard's Platform → Backup screen, and
needs no chart value at all: the archive is one gzipped tar carrying every
Kitchen object, every credential in `kitchen-system`, and the identity
provider's database. What it does **not** carry is telemetry — ClickHouse is
expendable and already expires on its own retention.

Restoring is the half that needs the chart, because it happens into a cluster
whose accounts database is gone: the credentials to log into the dashboard are
inside the archive, so there is nobody left to press a button. Install the
chart at the release the archive was written by, put the archive in a Secret,
and turn the Job on:

```sh
kubectl -n kitchen-system create secret generic kitchen-backup \
  --from-file=backup.tar.gz=./kitchen-backup-prod-2026-08-19T090000Z.tar.gz
helm upgrade kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --reset-then-reuse-values \
  --set restore.enabled=true --set restore.secretName=kitchen-backup
```

The Job waits for the identity provider to have migrated its schema — the
accounts dump is data only — then writes the objects, the secrets and the rows,
and rolls the identity provider so it reads the restored signing secret. Its
ServiceAccount is bound to a Role over the kinds this chart owns and the
secrets in its own namespace, not to cluster-admin: unlike an upgrade, a
restore writes an enumerable list of things. `--set restore.id=2` runs it
again. The whole procedure, and what the archive deliberately leaves out, is
[docs/BACKUP.md](../../docs/BACKUP.md).

### Upgrading to the telemetry agent

The Vector log collector was replaced by an OpenTelemetry collector that also
carries metrics and the OTLP receiver, so its values moved with it. `logs.*` is
gone; the block is `collector.*`, and an upgrade that still passes `--set
logs.enabled=false` (or a values file with a `logs:` key) is now setting nothing
at all. The mapping:

| Old | New |
|---|---|
| `logs.enabled` | `collector.enabled` — note it now governs metrics and the OTLP endpoint too |
| `logs.excludePathsGlobPatterns` | `collector.logs.exclude` |
| `logs.extraLabelSelector` / `.extraFieldSelector` | gone — the agent reads files, not the API server's pod list. Use `collector.logs.excludeNamespaces` |
| `logs.globCooldownMs` | gone — new files are found by polling, with no cooldown to tune |
| `logs.batch.*` / `logs.buffer.*` | `collector.export.batch.*` / `collector.export.queueSize` |
| `logs.hostDataPath` | `collector.hostDataPath`, and its default moved to `/var/lib/kitchen/collector` |
| everything else under `logs.` | the same name under `collector.` |

The old `logs`, `metrics` and `traces` tables are **not** dropped. Nothing
writes to them any more, and they age out on their own TTL — 30 days by default
— so an installation that wants the space back sooner can drop them by hand once
the new tables have the history it needs.

### Upgrading to a chart that owns the namespace

Every release installed before `namespace.create` existed has a `kitchen-system`
that Helm does not own, and the upgrade refuses to import it. Either adopt it
once — see [Adopting an existing
namespace](#adopting-an-existing-namespace) — or upgrade with
`--set namespace.create=false` and keep managing its Pod Security labels
yourself. Adopting is a metadata-only change: nothing is restarted, and the
labels the chart then applies are what make log collection work.

### Upgrading to a closed platform namespace

The upgrade that adds `networkPolicy.enabled` starts denying ingress to
`kitchen-system` from everywhere except the rules in [What the platform
namespace accepts](#what-the-platform-namespace-accepts). Nothing the platform
does needs anything else, and the objects appear whether or not the cluster
enforces them — but two things are worth checking before you upgrade:

- **Anything of yours that reaches into `kitchen-system`** — a Prometheus that
  scrapes something other than `metrics.port`, a job that queries ClickHouse
  directly, a sidecar talking to the identity provider's Postgres — stops
  working the moment the CNI enforces the deny. Move it into the namespace, or
  upgrade with `--set networkPolicy.enabled=false` and write your own.
- **A cluster whose CNI does not enforce NetworkPolicy gets nothing, and says
  nothing.** Cilium is a prerequisite and does enforce it. If you are running
  something else, the policies are inert and the platform's stores are still
  open to every pod on the cluster.

Rolling it back is `--set networkPolicy.enabled=false` on the next upgrade,
which deletes the policies with the release's own manifest.

### Upgrading to a telemetry store that speaks TLS

The upgrade that adds `clickhouse.tls.enabled` restarts ClickHouse once, with a
certificate. Nothing has to be done in order, but it is worth knowing what
happens in what order and why nothing is lost:

1. Helm applies the release. The operator's own Deployment rolls first in
   practice — it is a Deployment with no volume to wait for — and the new
   operator creates the CA, the two issuers and the store's certificate.
   cert-manager issues both in seconds: it is signing them itself, with no ACME
   account, no DNS and nothing outside the cluster.
2. The ClickHouse StatefulSet rolls. Its new pod does not start until the
   certificate Secret exists, which by then it does. If the operator is slower,
   the pod waits — `ContainerCreating` — and starts when the Secret appears.
3. **The telemetry agent keeps collecting throughout.** Its export queue holds
   and retries for five minutes (`retry_on_failure.max_elapsed_time`), which is
   far longer than a single-pod restart, so a rollout costs latency rather than
   data. Its own health endpoint belongs to an extension that knows nothing
   about the store, so it stays Ready and its node keeps a place to export to.
4. **The REST API's health does not depend on the store either.** Telemetry
   reads fail while it is down and the dashboard's observability screens say so;
   nothing in the request path of a deployment touches it.

The window where the store is unreachable is one pod restart, which is what any
upgrade of it already costs. If the certificate never issues — no cert-manager
on the cluster, for instance — the store stays down and `InternalCAReady` says
which of the two certificates is stuck. `--set clickhouse.tls.enabled=false` on
the upgrade puts it back the way it was, in the clear.

### Upgrading to an accounts database that speaks TLS

The upgrade that adds `postgres.tls.enabled` restarts Postgres once, with a
certificate, and rolls the identity provider onto a DSN that verifies it. In
order:

1. Helm applies the release, and the operator's Deployment rolls first in
   practice — no volume to wait for. The new operator creates the CA (or finds
   the one the telemetry store already uses) and requests the database's
   certificate. cert-manager signs it in seconds, with nothing outside the
   cluster involved.
2. The Postgres StatefulSet rolls. Its new pod does not start until the
   certificate Secret exists, so it never runs with `ssl = on` and no
   certificate, and never refuses plaintext at a moment when nothing could
   have done TLS: the refusal and the certificate arrive in the same process.
3. The identity provider's Deployment rolls onto the new DSN. Its pods wait for
   the CA bundle the same way; the old pods, which connect in the clear, are
   refused by the new database from the moment it comes up, so this is one
   rolling restart's worth of failed logins rather than a slow drift — and a
   single-node Postgres restart already costs exactly that.
4. **Nothing else in the platform notices.** Deployments, builds and routes do
   not touch this database; what pauses is signing in.

If the certificate never issues — no cert-manager on the cluster, for instance
— the database stays down and `InternalCAReady` says which certificate is
stuck. `--set postgres.tls.enabled=false` on the upgrade puts it back the way
it was, in the clear.

### Upgrading to an object store that speaks TLS

The upgrade that adds `objectStore.tls.enabled` restarts MinIO once, with a
certificate, and rewrites where every bound bucket says its store is. The
ordering is the telemetry store's, and so is the reason nothing is lost:

1. Helm applies the release. The operator rolls first in practice and requests
   the store's certificate from the CA it already has (or mints, on an
   installation where this is the first store to ask for one).
2. The object store's StatefulSet rolls. Its new pod does not start until the
   certificate Secret exists — `ContainerCreating` until it does — so at no
   point is there a MinIO answering in the clear because its certificate was
   late.
3. **Every binding is brought forward, without a new credential.** The
   `endpoint` on each bound claim's Secret becomes `https://`, and a `caCert`
   key appears holding the CA certificate. The bucket, the access key and the
   secret key are left exactly as they are: the credential is the bucket's, and
   reissuing it to carry an address would roll every pod for a change that is
   not theirs.
4. **The applications reading those bindings roll themselves.** A Secret's
   values do not reach a pod that is already running, so the operator digests
   the Secrets each workload reads onto its pod template — a binding whose
   endpoint changed is a changed digest, and the Deployment rolls. It is the
   same mechanism a rotated credential goes through, and it needs no redeploy.

What an application does have to do is *use* `caCert`: a client configured with
the endpoint alone verifies against the host's roots, which have never heard of
this CA, and refuses the connection. Its requests fail from the store's restart
until it is configured to trust the certificate — there is no window in which
they succeed in the clear. See [the claims
guide](../../docs/api/claims.md#objectstore) for what the key holds.

`--set objectStore.tls.enabled=false` on the upgrade puts it back the way it
was, in the clear, and the bindings follow it back on the next reconcile.

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
helm upgrade kitchen ./charts/kitchen --namespace kitchen-system --reset-then-reuse-values
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

**What `kitchen.applyOnUpgrade=true` costs.** Deleting and recreating the
object means every field the chart does not render is gone, and every field it
does render is set back to the value in your `--set` flags and values files. A
base domain, a retention, a build strategy changed on the settings screen is
reverted. So is the object's `status`, which is rebuilt from scratch on the
next reconcile — nothing about the installation can be remembered there.

Two things are carried across rather than dropped, because losing them changes
who may do what:

- **The operator list.** `spec.access.operators` is [who owns the
  platform](#who-owns-the-platform), and an *absent* list is how the operator
  is told that nobody has ever said — so a recreated object with no list would
  be re-seeded from every account the identity provider holds. Narrow the
  platform to two operators on the settings screen and the next upgrade would
  hand it back to all fourteen, silently, and a deliberate `operators: []`
  would be destroyed outright. The chart therefore reads the live list at
  render time and writes it back, `[]` included. Set
  `kitchen.access.operators` to override it deliberately.
- The identity provider's and the databases' generated credentials, read back
  out of their secrets the same way.

Both are lookups against the cluster, so `helm template` and `--dry-run`
cannot see them: a rendered diff shows the operator list absent while the
upgrade itself preserves it. There is a moment during the upgrade when the
singleton does not exist at all, between the delete and the create; the API
answers nothing useful for that moment, and the reconciler rebuilds the status
afterwards.

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
| `namespace.podSecurity` | `privileged` | Pod Security level on the platform namespace. Anything stricter needs `collector.enabled=false`. |
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
| `networkPolicy.enabled` | `true` | Deny ingress to `kitchen-system` except from the platform itself, on the published ports, and from project namespaces to the object store and the telemetry agent. Needs a CNI that enforces NetworkPolicy; see [What the platform namespace accepts](#what-the-platform-namespace-accepts). |
| `selfUpdate.enabled` | `false` | Let the platform upgrade its own release from the dashboard. Creates a ServiceAccount bound to **cluster-admin**; see [Letting the platform update itself](#letting-the-platform-update-itself). |
| `selfUpdate.chart` | `oci://ghcr.io/bermos/charts/kitchen` | Chart the upgrade pulls from. |
| `selfUpdate.releaseName` | `""` | Release to upgrade. Defaults to this release's own name. |
| `selfUpdate.allowMinor` | `false` | Allow an upgrade that crosses a minor version — pre-1.0, where breaking changes land. |
| `selfUpdate.timeout` | `15m` | How long helm is given to finish. |
| `selfUpdate.serviceAccountName` | `""` | Generated when empty. |
| `selfUpdate.image.repository` / `.tag` | `alpine/helm` / `3.19.0` | Image the update job runs helm from. |
| `backup.serviceAccountName` | `""` | Generated when empty. The identity a **scheduled** backup runs as. The schedule, the destination and the retention are not chart values: they are runtime configuration on the `Kitchen` object, edited on the Backup screen, because a backup that could only be reconfigured by a `helm upgrade` is one nobody reconfigures. See [docs/BACKUP.md](../../docs/BACKUP.md). |
| `backup.rbac.create` | `true` | Create the scheduled backup's ServiceAccount and its roles. Read-only, and not a privilege reduction: a backup reads every credential the platform holds. It is separate so the grant is legible in one file and gone with the release. |
| `restore.enabled` | `false` | Run the restore Job. A bootstrap step, not something an install does — see [docs/BACKUP.md](../../docs/BACKUP.md). Needs one of the two sources below. |
| `restore.secretName` / `.secretKey` | `""` / `backup.tar.gz` | Secret holding the archive. Bounded by etcd's object limit (about 1 MiB). |
| `restore.existingClaim` | `""` | A PersistentVolumeClaim holding the archive instead, for one past that. |
| `restore.path` | `/archive/backup.tar.gz` | Where the archive is read from. Its directory is the mount point, and must not be a path the image already has a binary at. |
| `restore.id` | `"1"` | Changing it runs the restore again: a Job's pod template is immutable, so the id is in its name. |
| `restore.force` | `false` | Restore an archive written by a different release. The accounts dump carries rows and not a schema. |
| `restore.skipAccounts` | `false` | Restore the objects and secrets alone. |
| `restore.waitForSchema` | `5m` | How long to wait for the identity provider to have migrated its schema. |
| `restore.serviceAccountName` | `""` | Generated when empty. |
| `restore.rbac.create` | `true` | Create the restore's ServiceAccount and roles. Not cluster-admin, unlike self-update's. |
| `restore.resources` | 100m/128Mi → 1Gi | The memory it wants tracks the archive, not the cluster. |
| `restore.ttlSecondsAfterFinished` | `86400` | Seconds the finished Job and its pod are kept. A Job is not garbage-collected on its own, and `restore.id` guarantees they accumulate. Empty keeps it until somebody deletes it. |
| `crds.install` | `true` | Install the `kitchen.bermos.dev` CRDs. |
| `crds.keep` | `true` | Keep CRDs (and custom resources) on uninstall. |
| `kitchen.create` | `true` | Create the `Kitchen` singleton. Needs `baseDomain`. |
| `kitchen.applyOnUpgrade` | `false` | Re-apply the singleton on every upgrade, reverting anything changed on the settings screen. The object is deleted and recreated; the operator list and the generated credentials are carried across, nothing else is. See [Upgrade](#upgrade). |
| `kitchen.access.operators` | `[]` | Who owns the platform, named at install time: email addresses, `sub`s, or maps of `subject` and `email`. Empty leaves it to be seeded from the accounts the bundled identity provider holds — **an installation on a federated issuer has to set this**, or nobody ever holds the operator role. See [Who owns the platform](#who-owns-the-platform). |
| `kitchen.baseDomain` | `""` | Generated URLs are `<slug>.<baseDomain>`. |
| `kitchen.clusterName` | `""` | What the dashboard's status bar calls this cluster. Defaults to the first label of `baseDomain`. |
| `kitchen.residency` | `""` | Where this installation's data is located — a region or jurisdiction in your own vocabulary. Declared, not observed; the compliance inventory's default for environments that declare none, reading `"unknown"` when empty. |
| `kitchen.api.externalURL` | `""` | Defaults to `kitchen.<baseDomain>`, under the scheme `kitchen.tls.mode` serves. |
| `kitchen.ingress.gatewayClassName` | `cilium` | GatewayClass for the shared Gateway. |
| `kitchen.ingress.publicAddresses` | `[]` | Addresses the internet reaches the platform on, when a router forwards :80/:443 to a private Gateway address. What `dns.mismatch` compares published names against. Addresses, not hostnames. |
| `kitchen.ingress.cloudflared.enabled` | `false` | Run a cloudflared tunnel as the edge. |
| `kitchen.ingress.cloudflared.tunnelSecretName` | `""` | Secret with the tunnel token under `token`. |
| `kitchen.tls.mode` | `acme` | `acme`, `cloudflared` or `none`. `acme` requires the `acme` values below. Also the scheme of every published URL: `none` serves HTTP only. |
| `kitchen.tls.acme.email` | `""` | CA contact address. Required in `acme` mode. |
| `kitchen.tls.acme.server` | Let's Encrypt production | ACME directory URL. Use the staging directory while setting up. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName` | `""` | Secret holding a Cloudflare API token (`Zone:DNS:Edit` + `Zone:Zone:Read`). Required in `acme` mode. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretKey` | `api-token` | Key inside that secret. |
| `cert-manager.enabled` | `true` | Install cert-manager with the platform. Disable if the cluster already runs one. |
| `cert-manager.crds.enabled` / `.keep` | `true` / `true` | Install cert-manager's CRDs, and keep them on uninstall. |
| `cert-manager.resources` / `.webhook.resources` / `.cainjector.resources` / `.startupapicheck.resources` | 10m/32–128Mi → 128–512Mi | cert-manager's own chart ships none, so every one of its pods would be BestEffort — first evicted when a node runs short, for the component that renews the wildcard certificate every published URL rides on. |
| `cert-manager.config.gatewayAPI.enabled` | `true` | Solve HTTP-01 challenges as HTTPRoutes on the shared Gateway — what issues custom-domain certificates. A cluster that runs its own cert-manager needs the same switch on it. |
| `kitchen.auth` | from `auth.*` / `previewGate.*` | The singleton's `auth` block mirrors `auth.enabled`, the resolved host, the secret the operator registers clients with, and the preview gate. |
| `kitchen.builds.defaultStrategy` | `auto` | `auto`, `dockerfile` or `buildpacks`. |
| `kitchen.builds.concurrency` | `2` | Builds running at once. Read with `builds.resources` below: the two together are what bound the platform's own build footprint. |
| `kitchen.builds.resources.cpu` | `"2"` | CPU one build may take, as a Kubernetes quantity, written onto every container of the build pod as its request and its limit at once. CPU is compressible: a build that wants more is throttled, never killed. Empty for no ceiling. |
| `kitchen.builds.resources.memory` | `4Gi` | Memory one build may take, reserved for the build and capped at it — so a node with no room queues the build rather than starting it on top of what is already running. A build that reaches it is killed and reported as a build failure naming the ceiling. Empty for no ceiling. |
| `kitchen.builds.timeoutMinutes` | `60` | How long one build may run before the platform ends it and the commit reports a failed build, in minutes — the ceiling in time that `builds.resources` is in capacity. An hour is far past anything the platform is meant to build; raise it where a cold-cache monorepo or a build on a small node legitimately takes longer. `0` is no deadline at all. A change reaches builds started after it, since the Job's deadline is immutable once it exists. |
| `kitchen.builds.imagePollInterval` | `10m` | How often the platform asks whether a watched tag has moved — the event that corresponds to a push, for a project whose software this platform did not build. One registry manifest HEAD per watched reference, never a pull. A reference pinned to a digest is never asked about, which is how a project opts out of moving. `0` turns the poll off; anything else has a floor of one minute. |
| `kitchen.builds.releaseRetention` | `10` | Releases each project keeps. Older ones are pruned, except any an environment still points at — a rollback target never disappears. `0` keeps every release forever. |
| `kitchen.builds.cache.enabled` | `true` | Reuse layers between builds. The cache is a manifest in the registry the project already pushes to, under the same credential — nothing extra to install. Off means every build starts from nothing. |
| `kitchen.builds.cache.mode` | `max` | How much of a BuildKit build is cached: `max` keeps intermediate layers, so a source change still reuses the dependency install above it, at the cost of registry storage; `min` keeps only the layers of the image that came out. Buildpacks builds ignore it. |
| `kitchen.builds.cache.scope` | `project` | What two builds share to reuse each other's layers: `project` is one tag per project, overwritten in place and so bounded; `branch` is one per branch, a better hit rate on a long-lived branch and a tag nothing removes. |
| `kitchen.previews.maxPerProject` | `5` | Preview environments one project may have live at once. A pull request that would exceed it gets no preview and is told so with a commit status and a comment naming this setting, rather than a preview that starts and lets the node sort it out — a project with claims materializes a backing service per claim per preview. Production environments and anything promoted are never counted, a project may set its own ceiling (`spec.previews.max`), and `0` is no ceiling at all. |
| `kitchen.appNamespaces.podSecurity` | `privileged` | Pod Security level the operator labels each project's namespace with. `privileged` is what the rootless BuildKit builder needs — anything stricter refuses its pods at admission and Dockerfile builds never start. See [Application namespaces have a level of their own](#application-namespaces-have-a-level-of-their-own). |
| `kitchen.compliance.audit.enabled` | `true` | Record every state transition into an append-only, hash-chained log in the telemetry store. Off leaves the platform with no evidence of what it did. |
| `kitchen.compliance.audit.retentionDays` | `365` | Audit retention, deliberately separate from telemetry retention: evidence must outlive logs. Minimum 90. |
| `kitchen.compliance.attestation.enabled` | `true` | Sign a build record for every artifact and attach it to the artifact's digest as a DSSE envelope over an in-toto statement, through OCI referrers. |
| `kitchen.compliance.attestation.signingKeySecretName` | `""` | Secret holding the signing keypair (`private.pem`, `public.pem`). Empty has the operator generate one into `kitchen-attestation-key`. |
| `kitchen.compliance.attestation.build.provenance` | `true` | Ask the builder for SLSA provenance: the source commit it resolved, the base images it pulled and their digests, and what it was invoked with. A stronger claim than the build record, because the process that did the work makes it. Costs the build almost nothing. |
| `kitchen.compliance.attestation.build.sbom` | `true` | Ask the builder for a bill of materials for the finished image. Not free: a scanner image is pulled on every build, because the build pod is ephemeral. |
| `kitchen.compliance.attestation.build.sbomGenerator` | `""` | Scanner image the builder runs. The format follows the generator and is recorded rather than converted: the default emits SPDX 2.3, a CycloneDX generator produces a CycloneDX attestation. Empty uses a pinned default. |
| `kitchen.compliance.attestation.vendored.sbom` | `true` | Generate a bill of materials for an image the platform did **not** build, where the vendor published none, and attest it as the platform's own observation rather than as the vendor's claim. On by default because an artifact with no bill of materials is one the rescan pass can never look inside. |
| `kitchen.compliance.attestation.vendored.sbomGenerator` | `""` | The image that generates it: a scanner pointed at the digest. Not the builder-side generator above, which speaks BuildKit's scanner protocol and cannot run standalone. Empty uses a pinned default. |
| `kitchen.compliance.attestation.vendored.timeoutSeconds` | `1800` | Seconds one generation may take. Generous because the pod pulls the whole image; a run that overruns is recorded as a generation that did not happen. |
| `kitchen.compliance.machineIdentities` | `[]` | Provider usernames exempt from a project's "require a reviewed pull request" setting — Renovate, release-please, and anything else that merges its own commits and will never have a reviewer. Matched exactly and case-insensitively; every use of the exemption is an audit record, which is what makes it auditable rather than merely configured. |
| `kitchen.compliance.exceptions.ladder` | `[]` | The break-glass escalation ladder: rungs of `{maxDuration, role}` mapping how long an exception is asked for to who may approve it — `developer` or `admin` on the project, `operator` on the platform. A duration beyond every rung always needs an operator. Empty uses the compiled-in default: up to 24h needs developer, up to 720h (30 days) needs admin, longer needs an operator. |
| `kitchen.compliance.gates` | `[]` | Quality gates run over every artifact. Each is a pod — an image you name, pointed at the artifact through `$(KITCHEN_ARTIFACT)`, writing findings to `$(KITCHEN_FINDINGS)` — whose result the platform signs and attaches to the artifact's digest. A gate records findings and **never** a verdict, so do not pass a scanner the flag that makes it exit non-zero on findings: a non-zero exit means the gate did not run. |
| `kitchen.compliance.vex.enabled` | `true` | Admit OpenVEX documents: somebody's assertion that a vulnerability found in an artifact is not exploitable in it. This is what keeps continuous rescanning readable — a daily report of four hundred findings of which four matter gets rubber-stamped. A `not_affected` statement must carry one of OpenVEX's five justifications; free text alone is refused. Whether a statement suppresses anything is still the target environment's policy's question. |
| `kitchen.compliance.vex.trustedAuthors` | `[]` | Document authors whose statements the platform will sign and attach at all — platform-wide admission, matched exactly and case-insensitively. Empty admits any authenticated caller's document and leaves the attribution as the control: every submission is an audit record naming who sent it and what it asserted. |
| `kitchen.compliance.rescan.enabled` | `false` | Re-evaluate every currently-deployed release on a schedule against a current vulnerability database, through the same policy path a promotion uses. No rebuild and no redeploy: what is matched is the bill of materials the build already attested. Off by default — it costs a scanner pod per environment per interval. |
| `kitchen.compliance.rescan.interval` | `24h` | How often each deployed (environment, release) pair is re-evaluated, counted from its own last finished scan so the estate spreads itself out. Floor one hour. |
| `kitchen.compliance.rescan.concurrency` | `4` | Scans in flight at once across the whole platform. The first sweep after an upgrade has every environment due at the same instant; this is what stops that becoming two hundred simultaneous image pulls. |
| `kitchen.compliance.rescan.scanner` | `{}` | The matcher run over each artifact's SBOM: `{name, image, args, version, format, timeoutSeconds}`, pointed at `$(KITCHEN_SBOM)` and writing to `$(KITCHEN_FINDINGS)` and, where it can, `$(KITCHEN_DATA_SNAPSHOT)`. `format` is `grype-json`, `trivy-json` or `osv-json`. No default, deliberately: enabled with no scanner the pass reports itself inert rather than picking a vendor for you. Like a gate, a scan records findings and never a verdict. |
| `kitchen.compliance.access.enabled` | `true` | Ask somebody, on a cadence, who holds what here — and watch Kitchen's own objects for changes no reconcile made. None of it can refuse a deployment. Off leaves the register readable and cycles openable by hand, and stops the platform opening or watching anything on its own. |
| `kitchen.compliance.access.intervalDays` | `90` | How often a recertification cycle opens, counted from the last one's close so a late installation does not immediately owe two. `0` opens none automatically. |
| `kitchen.compliance.access.dueDays` | `14` | How long a cycle has from opening before it is reported overdue. Overdue is a phase and a condition, never a refusal. |
| `kitchen.compliance.access.inactivityDays` | `90` | How long without a recorded action makes an identity dormant. Dormant **and** unknown to the identity provider is orphaned. The audit log records writes, so a read-only account looks dormant and is not. |
| `kitchen.compliance.access.detectOutOfBandWrites` | `true` | Read the field manager the API server recorded on Kitchen's objects and report writes the platform did not make. Detection, never prevention — anybody with cluster-admin can name their field manager anything, and docs/COMPLIANCE.md says so rather than claiming otherwise. |
| `kitchen.compliance.access.expectedManagers` | `[]` | Field-manager names whose writes are expected, beyond the platform's own and Helm's — a GitOps controller, a mutating webhook, a restore tool. Matched exactly, no patterns. |
| `kitchen.observability.clickhouse.retentionDays` | `30` | The retention every telemetry class inherits when `kitchen.retention` does not set it. |
| `kitchen.observability.hubble.relayAddress` | `""` | host:port of Hubble Relay's gRPC endpoint (e.g. `hubble-relay.kube-system.svc.cluster.local:80`). When set, the operator ships flow observations into the telemetry store for the dashboard's traffic view. Empty disables flow collection. |
| `kitchen.observability.metrics.enabled` | `true` | The operator's half of the environment history: restarts, OOM kills, configured limits and replica counts, sampled off the API server and exported to the agent over OTLP. CPU and memory come from the agent's kubelet scrape instead. |
| `kitchen.observability.metrics.intervalSeconds` | `30` | Seconds between samples. |
| `kitchen.observability.traces.enabled` | `true` | Hand every environment the agent's OTLP address through the standard OTLP environment variables. The receiver itself belongs to the agent and runs with it. |
| `kitchen.observability.traces.port` | `4318` | Port applications export to: the agent's OTLP/HTTP receiver and its Service — OTLP/HTTP's registered one. |
| `kitchen.observability.traces.endpoint` | `""` | What applications are told to export to. Empty means the agent's own in-cluster Service. |
| `kitchen.observability.traces.service.annotations` | `{}` | |
| `kitchen.observability.clockSync.enabled` | `true` | Measure how far the cluster's clocks are from the operator's own, and report drift as an unhealthy component. Every correlation in an incident report is timestamps from several machines, so clocks that disagree make the order wrong silently. |
| `kitchen.observability.clockSync.maxDriftSeconds` | `5` | Seconds a node's clock may be from the operator's before the check reports it. Chosen against the use rather than against NTP's accuracy: five seconds is roughly where "these happened in this order" stops being safe to say across machines. |
| `kitchen.retention.containerLogs` | `~` | Days to retain application, platform and cluster container logs. Empty inherits `kitchen.observability.clickhouse.retentionDays`, as every entry below does. |
| `kitchen.retention.buildLogs` | `~` | Days to retain build output. Its own class because a build log is read months later beside an artifact's provenance. It shares a table with the container logs, so setting the two apart costs that table its cheap part-drop expiry — see docs/COMPLIANCE.md §12.2. |
| `kitchen.retention.flows` | `~` | Days to retain observed network flows. |
| `kitchen.retention.metrics` | `~` | Days to retain metric series and their rollups. |
| `kitchen.retention.traces` | `~` | Days to retain spans. |
| `kitchen.retention.requests` | `~` | Days to retain HTTP request telemetry. Raw rows live a week or this window, whichever is shorter, and the hourly rollup twelve of these windows; those ratios are not configurable. |
| `kitchen.retention.clusterEvents` | `~` | Days to retain the cluster's Warning-event history, of which this is the only copy — the API server expires the originals after about an hour. |
| `kitchen.retention.activity` | `~` | Days to retain the dashboard's activity feed. Prose rather than evidence, so a short window costs only convenience. |
| `kitchen.retention.audit` | `~` | Days to retain audit records and the policy decisions they gate. Empty inherits `kitchen.compliance.audit.retentionDays`. The floor is 90 and the only way under it is the override below. |
| `kitchen.retention.auditFloorOverride.reason` | `""` | Why this installation keeps audit records for less than the 90-day floor. At least 20 characters — it is the answer somebody gets when they ask why the log does not go back far enough. Setting it is itself an audit record. |
| `kitchen.retention.auditFloorOverride.approvedBy` | `""` | Who decided it, as a name or an address. Recorded as written; the platform does not resolve it. |
| `clickhouse.enabled` | `true` | Run a single-node ClickHouse in the release. |
| `clickhouse.image.repository` / `.tag` | `clickhouse/clickhouse-server` / `26.3.17.110-alpine` | Current LTS line. |
| `clickhouse.auth.database` / `.username` | `kitchen` / `kitchen` | Created on first start. |
| `clickhouse.auth.password` | `""` | Generated on install, preserved on upgrade. |
| `clickhouse.tls.enabled` | `true` | Serve the store over TLS, with a certificate the operator requests from the platform's internal CA, and remove its plaintext listeners. Off is the one way to run it in the clear; see [How the store is reached](#how-the-store-is-reached). |
| `clickhouse.tls.httpsPort` / `.nativePort` | `8443` / `9440` | Ports the HTTP interface and the native protocol answer on once TLS is on. |
| `clickhouse.service.type` / `.httpPort` / `.nativePort` | `ClusterIP` / `8123` / `9000` | The ports apply when `clickhouse.tls.enabled` is off. |
| `clickhouse.persistence.enabled` | `true` | PVC for the data directory. |
| `clickhouse.persistence.size` / `.storageClass` / `.accessModes` | `20Gi` / cluster default / `[ReadWriteOnce]` | |
| `clickhouse.resources` | 200m/1Gi → 4Gi | |
| `clickhouse.podSecurityContext` / `.securityContext` | restricted PSS | Non-root as the image's uid 101, no capabilities, and a read-only root filesystem: the data directory, `users.d`, `/tmp` and the server's log directory are volumes, so nothing writes to the image. |
| `clickhouse.extraConfig` | `{}` | Filename → XML for `config.d`, passed through `tpl`. |
| `clickhouse.external.host` / `.httpPort` / `.nativePort` | `""` / `8123` / `9000` | Point at an existing ClickHouse. |
| `clickhouse.external.tls` | `false` | That store serves TLS, verified against the host's roots. The operator issues nothing for it. |
| `clickhouse.acknowledgeNoStore` | `false` | Install with no telemetry store at all. |
| `collector.enabled` | `true` | Run the telemetry agent. Off means no logs, no metrics and no OTLP endpoint. Skipped when there is no store. |
| `collector.image.repository` / `.tag` | `otel/opentelemetry-collector-contrib` / `0.158.0` | Pinned: the operator's DDL tracks this exporter version. |
| `collector.logs.enabled` | `true` | Tail the node's container log files. |
| `collector.logs.excludeNamespaces` | `[]` | Namespaces whose containers are not tailed. |
| `collector.logs.exclude` | `[]` | Extra log file globs to skip. |
| `collector.logs.maxStructuredFields` | `64` | Fields kept from a JSON line, queryable as `http.status:500`. Beyond it the line ships without them. |
| `collector.logs.levelFields` | `[level, severity, lvl]` | JSON fields a line's severity may be written under, first match wins. Non-JSON lines are scanned for the common spellings. |
| `collector.logs.traceIdFields` / `.spanIdFields` | `[trace_id, traceId, …]` | Fields a line's trace and span ids may be written under, first match wins. Lifting them into `TraceId`/`SpanId` is what links a log line to its trace. |
| `collector.metrics.kubelet.enabled` | `true` | Scrape the node's kubelet for per-pod and per-container CPU and memory. |
| `collector.metrics.node.enabled` | `true` | Scrape the node itself: CPU, load, memory, network, disk, filesystem. Off also removes the mount of the node's root filesystem, which is there for this scraper and nothing else — the container log files are their own read-only `/var/log` mount. |
| `collector.metrics.intervalSeconds` | `30` | Seconds between scrapes, for both. |
| `collector.otlp.grpcPort` | `4317` | OTLP/gRPC port. OTLP/HTTP's is `kitchen.observability.traces.port`. |
| `collector.export.queueSize` | `20000` | Signals held per node while the store is unreachable; beyond it the newest are dropped. |
| `collector.export.batch.minSize` / `.maxSize` / `.flushTimeoutSeconds` | `5000` / `0` / `5` | Insert batching, in the exporter's queue rather than a `batch` processor; the timeout is log latency. |
| `collector.logLevel` | `info` | Log level of the agent itself. |
| `collector.serviceAccount.create` / `.name` / `.annotations` | `true` / `""` / `{}` | |
| `collector.rbac.create` | `true` | ClusterRole to read pod and namespace metadata and the kubelet's stats and pods endpoints. |
| `collector.hostLogsPath` / `.hostDataPath` | `/var/log` / `/var/lib/kitchen/collector` | Node paths for logs and read offsets. |
| `collector.goMemLimit` | `800MiB` | `GOMEMLIMIT`; keep at ~80% of `resources.limits.memory`. |
| `collector.resources` | 100m/256Mi → 1Gi | See [Sizing the agent](#sizing-the-agent). |
| `collector.tolerations` | `[{operator: Exists}]` | Collect from tainted nodes too. |
| `collector.terminationGracePeriodSeconds` | `60` | Time to drain the export queue on shutdown. |
| `postgres.enabled` | `true` | Run a single-node Postgres for the identity provider. |
| `postgres.image.repository` / `.tag` | `postgres` / `17.6-alpine` | |
| `postgres.auth.database` / `.username` | `kitchen_auth` / `kitchen` | Created on first start. |
| `postgres.auth.password` | `""` | Generated on install, preserved on upgrade. |
| `postgres.tls.enabled` | `true` | Serve the accounts database over TLS with a certificate the operator requests from the platform's internal CA, and refuse plaintext connections. Off is the one way to run it in the clear; see [How the accounts database is reached](#how-the-accounts-database-is-reached). |
| `postgres.service.type` / `.port` | `ClusterIP` / `5432` | |
| `postgres.persistence.enabled` | `true` | PVC for the data directory. Accounts die with the pod without it. |
| `postgres.persistence.size` / `.storageClass` / `.accessModes` | `8Gi` / cluster default / `[ReadWriteOnce]` | |
| `postgres.resources` | 100m/256Mi → 1Gi | |
| `postgres.podSecurityContext` / `.securityContext` | restricted PSS | Non-root as the image's uid 70, no capabilities, and a read-only root filesystem: `PGDATA` and the socket directory are volumes and are the whole of what the image writes. |
| `postgres.external.host` / `.port` | `""` / `5432` | Point at an existing Postgres. |
| `postgres.external.sslmode` | `""` | What clients ask of an external Postgres, appended to the DSN. `verify-full` is the only value both drivers read the same way; empty is a connection in the clear, and the platform says so. |
| `auth.enabled` | `true` | Deploy the identity provider. Needs a Postgres. |
| `auth.image.repository` / `.tag` / `.digest` | `ghcr.io/bermos/kitchen-auth` / `""` / `""` | Tag defaults to `appVersion`. |
| `auth.replicaCount` | `1` | Stateless; state lives in Postgres. |
| `auth.secret` | `""` | Signing secret. Generated on install, preserved on upgrade. |
| `auth.serviceKey` | `""` | Operator API key for client registration, ≥64 characters. |
| `auth.serviceAccountEmail` | `operator@kitchen.local` | Machine account owning that key. |
| `auth.bootstrap.enabled` / `.token` | `true` / `""` | One-time first-administrator link. |
| `auth.ui.enabled` | `true` | Register the Kitchen UI as an OAuth client (PKCE, no secret). |
| `auth.ui.clientId` | `kitchen-ui` | Client id the UI authenticates with, and the one OAuth client the operator API accepts a token from. |
| `auth.ui.redirectURIs` | `[]` | Defaults to `<kitchen.api.externalURL>/auth/callback`. |
| `auth.github.clientId` / `.clientSecret` | `""` | Upstream GitHub OAuth app. |
| `auth.github.existingSecret` / `.existingSecretKey` | `""` / `clientSecret` | Read the client secret from an existing secret. |
| `auth.allowSocialSignUp` | `false` | Let an unknown GitHub account create a Kitchen account. |
| `auth.trustedOrigins` | `[]` | Extra browser origins allowed to make signed-in calls to the identity provider. The dashboard's own is derived from `api.externalURL`. |
| `auth.port` | `8080` | Container port for the published listener: OIDC, the hosted pages, the bootstrap link. |
| `auth.internalPort` | `8081` | Container port for the private listener, which serves the operator's `/kitchen` prefix. The published one answers 404 there. |
| `auth.internalRateLimit` | `300` | Requests into `/kitchen` per minute per source address; `0` for none. |
| `auth.service.type` / `.port` | `ClusterIP` / `80` | The Service the HTTPRoute names, and the only one published. |
| `auth.internalService.port` / `.annotations` | `80` / `{}` | The Service in front of the private listener, `<release>-auth-internal`. No HTTPRoute references it. |
| `auth.route.enabled` | `true` | Publish the issuer on the shared Gateway. |
| `auth.route.host` | `""` | Defaults to `auth.<baseDomain>`. This is the OIDC issuer. |
| `auth.route.gateway.name` / `.namespace` | `kitchen` / `kitchen-system` | Must match the operator's constants. |
| `auth.resources` | 50m/128Mi → 512Mi | |
| `auth.logLevel` | `info` | `debug`, `info`, `warn`, `error`. |
| `previewGate.enabled` | `true` | Gate protected previews behind platform login. Needs `auth.enabled`. |
| `previewGate.host` | `""` | Where logins come back to. Defaults to `previews.<baseDomain>`. |
| `previewGate.replicas` | `2` | The gate is in the request path of every protected preview. |
| `previewGate.sessionTTL` | `8h` | How long a visitor stays signed in to a preview. |
| `registry.enabled` | `true` | Run the bundled image registry. See [The bundled registry](#the-bundled-registry). |
| `registry.image.repository` / `.tag` | `ghcr.io/project-zot/zot` / `v2.1.20` | |
| `registry.auth.username` | `kitchen` | The registry's one account. |
| `registry.auth.password` | `""` | Generated on install, preserved on upgrade. |
| `registry.host` | `""` | Defaults to `registry.<baseDomain>`. Also the prefix images are pushed under. |
| `registry.service.type` / `.port` | `ClusterIP` / `5000` | |
| `registry.persistence.enabled` | `true` | PVC for the image store. Every image dies with the pod without it. |
| `registry.persistence.size` / `.storageClass` / `.accessModes` | `20Gi` / cluster default / `[ReadWriteOnce]` | |
| `registry.retention.enabled` | `true` | Reclaim disk while the registry runs. Off means it grows without bound. |
| `registry.retention.keepTags` | `20` | Tagged images kept per repository, newest push first. |
| `registry.retention.keepPushedWithin` | `720h` | Keep anything pushed within this window whatever its rank. |
| `registry.retention.delay` | `24h` | Grace period before a manifest policy stopped keeping is removed. |
| `registry.retention.gcInterval` / `.gcDelay` | `24h` / `2h` | How often the collector runs, and how long a fresh blob is left alone. |
| `registry.resources` | 50m/128Mi → 1Gi | |
| `registry.logLevel` | `info` | |
| `objectStore.enabled` | `false` | Run the bundled object store and seed the `s3` Connection pointing at it. See [The bundled object store](#the-bundled-object-store). |
| `objectStore.image.repository` / `.tag` | `quay.io/minio/minio` / `RELEASE.2025-04-22T22-12-26Z` | A single MinIO server on one volume. |
| `objectStore.auth.accessKeyId` | `kitchen` | The root user; mints every bucket's own credential and is never handed to an application. |
| `objectStore.auth.secretAccessKey` | `""` | Generated on install, preserved on upgrade. |
| `objectStore.region` | `us-east-1` | What every bucket reports, and what the seeded Connection is told. |
| `objectStore.tls.enabled` | `true` | Serve the store over HTTPS, with a certificate the operator requests from the platform's internal CA, and hand every binding the CA certificate to verify it against. Off is the one way to run it in the clear; see [How applications reach it](#how-applications-reach-it). |
| `objectStore.service.port` | `9000` | |
| `objectStore.persistence.enabled` | `true` | PVC for the store. Every object dies with the pod without it. |
| `objectStore.persistence.size` / `.storageClass` / `.accessModes` | `50Gi` / cluster default / `[ReadWriteOnce]` | |
| `objectStore.resources` | 100m/256Mi → 2Gi | |
| `scaleToZero.enabled` | `false` | Idle environments down to no pods. An idle environment stops doing *everything*, not only serving — a background loop stops with it, so a project whose workload is not request-driven sets `spec.runtime.notRequestDriven` and keeps its pods. Needs KEDA and the HTTP add-on in the cluster — installed by the operator, or by you; see [Scale to zero](#scale-to-zero). |
| `scaleToZero.install.enabled` | `false` | Let the operator install KEDA and its HTTP add-on itself. Creates a ServiceAccount bound to cluster-admin; does nothing on a cluster that already runs KEDA. |
| `scaleToZero.install.chartRepository` | `https://kedacore.github.io/charts` | Helm repository the two charts are pulled from. |
| `scaleToZero.install.version` | `""` | KEDA chart version to install. Empty takes the operator's own pin. |
| `scaleToZero.install.addOnVersion` | `""` | HTTP add-on chart version to install. Empty takes the operator's own pin. |
| `scaleToZero.install.timeout` | `10m` | How long helm is given for each of the two installs. Both wait for their workloads. |
| `scaleToZero.install.serviceAccountName` | `""` | Name of the install job's ServiceAccount. Generated when empty. |
| `scaleToZero.install.image.repository` | `alpine/helm` | Image the install job runs helm from. |
| `scaleToZero.install.image.tag` | `3.19.0` | Tag of that image. |
| `scaleToZero.interceptor.service` | `keda-add-ons-http-interceptor-proxy` | Interceptor Service idling environments are routed through. The add-on names it after its own chart, so this is a constant. |
| `scaleToZero.interceptor.namespace` | `keda` | Namespace the HTTP add-on was installed into. |
| `scaleToZero.interceptor.port` | `8080` | Port the interceptor accepts traffic on. |
| `databases.namespace` | `kitchen-databases` | Namespace provisioned databases live in. Not a project's own, so a `Retain`ed database survives the project's deletion. |
| `databases.install.enabled` | `false` | Let the operator install CloudNativePG itself. Creates a ServiceAccount bound to cluster-admin; does nothing on a cluster that already runs it. |
| `databases.install.chartRepository` | `https://cloudnative-pg.github.io/charts` | Helm repository the chart is pulled from. |
| `databases.install.version` | `""` | CloudNativePG chart version to install. Empty takes the operator's own pin. |
| `databases.install.timeout` | `10m` | How long helm is given for the install. It waits for the operator's workloads. |
| `databases.install.serviceAccountName` | `""` | Name of the install job's ServiceAccount. Generated when empty. |
| `databases.install.image.repository` | `alpine/helm` | Image the install job runs helm from. |
| `databases.install.image.tag` | `3.19.0` | Tag of that image. |
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
