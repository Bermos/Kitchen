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
The platform namespace is **not** in that list — the chart creates and labels
`kitchen-system` itself; see [Install](#install).

cert-manager is **not** in that list either: the chart ships it as a sub-chart
(`cert-manager.enabled`, on by default), because Kitchen owns the cluster it is
installed into. Set `cert-manager.enabled=false` for a cluster that already
runs one.

One optional feature does add a prerequisite: **KEDA and its HTTP add-on**, if
you want `scaleToZero.enabled`. They cannot be sub-charts of anything — see
[Scale to zero](#scale-to-zero) — so they are two `helm install` commands of
their own. Without them the feature simply stays off.

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
ClickHouse runs here or elsewhere, so the agent and the operator have one place
to look.

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
`username`, `password`, `dsn`). Point at an existing Postgres instead:

```sh
--set postgres.enabled=false \
--set postgres.external.host=postgres.databases.svc \
--set postgres.auth.password=<password>
```

Install without an identity provider — no login for the UI, no issuer for apps
— with `--set auth.enabled=false --set postgres.enabled=false`.

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
  --namespace kitchen-system --reuse-values \
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

## Scale to zero

An environment nobody is using can drop to no pods at all, and the next request
to its URL starts it again. That is what makes a dozen open pull requests
nearly free: a preview costs a URL and a Deployment record until someone opens
it.

It is off by default, and it is the one platform feature with a prerequisite
this chart does not install: [KEDA](https://keda.sh) and its
[HTTP add-on](https://github.com/kedacore/http-add-on) go in as their own Helm
releases, which is how upstream ships them and — see
[why they are not sub-charts](#why-keda-is-not-a-sub-chart) — the only way Helm
can install them at all.

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

Turning `scaleToZero.enabled` back off returns every environment to plain
Deployment routing on the next reconcile and deletes the scaled objects it
created. KEDA and the add-on are yours to uninstall separately, whenever.

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
  --namespace kitchen-system --reuse-values \
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
| `selfUpdate.enabled` | `false` | Let the platform upgrade its own release from the dashboard. Creates a ServiceAccount bound to **cluster-admin**; see [Letting the platform update itself](#letting-the-platform-update-itself). |
| `selfUpdate.chart` | `oci://ghcr.io/bermos/charts/kitchen` | Chart the upgrade pulls from. |
| `selfUpdate.releaseName` | `""` | Release to upgrade. Defaults to this release's own name. |
| `selfUpdate.allowMinor` | `false` | Allow an upgrade that crosses a minor version — pre-1.0, where breaking changes land. |
| `selfUpdate.timeout` | `15m` | How long helm is given to finish. |
| `selfUpdate.serviceAccountName` | `""` | Generated when empty. |
| `selfUpdate.image.repository` / `.tag` | `alpine/helm` / `3.19.0` | Image the update job runs helm from. |
| `restore.enabled` | `false` | Run the restore Job. A bootstrap step, not something an install does — see [docs/BACKUP.md](../../docs/BACKUP.md). Needs one of the two sources below. |
| `restore.secretName` / `.secretKey` | `""` / `backup.tar.gz` | Secret holding the archive. Bounded by etcd's object limit (about 1 MiB). |
| `restore.existingClaim` | `""` | A PersistentVolumeClaim holding the archive instead, for one past that. |
| `restore.path` | `/backup/backup.tar.gz` | Where the archive is read from. Its directory is the mount point. |
| `restore.id` | `"1"` | Changing it runs the restore again: a Job's pod template is immutable, so the id is in its name. |
| `restore.force` | `false` | Restore an archive written by a different release. The accounts dump carries rows and not a schema. |
| `restore.skipAccounts` | `false` | Restore the objects and secrets alone. |
| `restore.waitForSchema` | `5m` | How long to wait for the identity provider to have migrated its schema. |
| `restore.serviceAccountName` | `""` | Generated when empty. |
| `restore.rbac.create` | `true` | Create the restore's ServiceAccount and roles. Not cluster-admin, unlike self-update's. |
| `restore.resources` | `{}` | |
| `crds.install` | `true` | Install the `kitchen.bermos.dev` CRDs. |
| `crds.keep` | `true` | Keep CRDs (and custom resources) on uninstall. |
| `kitchen.create` | `true` | Create the `Kitchen` singleton. Needs `baseDomain`. |
| `kitchen.applyOnUpgrade` | `false` | Re-apply the singleton on every upgrade, reverting anything changed on the settings screen. The object is deleted and recreated; the operator list and the generated credentials are carried across, nothing else is. See [Upgrade](#upgrade). |
| `kitchen.access.operators` | `[]` | Who owns the platform, named at install time: email addresses, `sub`s, or maps of `subject` and `email`. Empty leaves it to be seeded from the accounts the bundled identity provider holds — **an installation on a federated issuer has to set this**, or nobody ever holds the operator role. See [Who owns the platform](#who-owns-the-platform). |
| `kitchen.baseDomain` | `""` | Generated URLs are `<slug>.<baseDomain>`. |
| `kitchen.clusterName` | `""` | What the dashboard's status bar calls this cluster. Defaults to the first label of `baseDomain`. |
| `kitchen.api.externalURL` | `""` | Defaults to `kitchen.<baseDomain>`, under the scheme `kitchen.tls.mode` serves. |
| `kitchen.ingress.gatewayClassName` | `cilium` | GatewayClass for the shared Gateway. |
| `kitchen.ingress.cloudflared.enabled` | `false` | Run a cloudflared tunnel as the edge. |
| `kitchen.ingress.cloudflared.tunnelSecretName` | `""` | Secret with the tunnel token under `token`. |
| `kitchen.tls.mode` | `acme` | `acme`, `cloudflared` or `none`. `acme` requires the `acme` values below. Also the scheme of every published URL: `none` serves HTTP only. |
| `kitchen.tls.acme.email` | `""` | CA contact address. Required in `acme` mode. |
| `kitchen.tls.acme.server` | Let's Encrypt production | ACME directory URL. Use the staging directory while setting up. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName` | `""` | Secret holding a Cloudflare API token (`Zone:DNS:Edit` + `Zone:Zone:Read`). Required in `acme` mode. |
| `kitchen.tls.acme.dns01.cloudflare.apiTokenSecretKey` | `api-token` | Key inside that secret. |
| `cert-manager.enabled` | `true` | Install cert-manager with the platform. Disable if the cluster already runs one. |
| `cert-manager.crds.enabled` / `.keep` | `true` / `true` | Install cert-manager's CRDs, and keep them on uninstall. |
| `cert-manager.config.gatewayAPI.enabled` | `true` | Solve HTTP-01 challenges as HTTPRoutes on the shared Gateway — what issues custom-domain certificates. A cluster that runs its own cert-manager needs the same switch on it. |
| `kitchen.auth` | from `auth.*` / `previewGate.*` | The singleton's `auth` block mirrors `auth.enabled`, the resolved host, the secret the operator registers clients with, and the preview gate. |
| `kitchen.builds.defaultStrategy` | `auto` | `auto`, `dockerfile` or `buildpacks`. |
| `kitchen.builds.concurrency` | `2` | Builds running at once. |
| `kitchen.builds.releaseRetention` | `10` | Releases each project keeps. Older ones are pruned, except any an environment still points at — a rollback target never disappears. `0` keeps every release forever. |
| `kitchen.builds.cache.enabled` | `true` | Reuse layers between builds. The cache is a manifest in the registry the project already pushes to, under the same credential — nothing extra to install. Off means every build starts from nothing. |
| `kitchen.builds.cache.mode` | `max` | How much of a BuildKit build is cached: `max` keeps intermediate layers, so a source change still reuses the dependency install above it, at the cost of registry storage; `min` keeps only the layers of the image that came out. Buildpacks builds ignore it. |
| `kitchen.builds.cache.scope` | `project` | What two builds share to reuse each other's layers: `project` is one tag per project, overwritten in place and so bounded; `branch` is one per branch, a better hit rate on a long-lived branch and a tag nothing removes. |
| `kitchen.compliance.audit.enabled` | `true` | Record every state transition into an append-only, hash-chained log in the telemetry store. Off leaves the platform with no evidence of what it did. |
| `kitchen.compliance.audit.retentionDays` | `365` | Audit retention, deliberately separate from telemetry retention: evidence must outlive logs. Minimum 90. |
| `kitchen.compliance.attestation.enabled` | `true` | Sign a build record for every artifact and attach it to the artifact's digest as a DSSE envelope over an in-toto statement, through OCI referrers. |
| `kitchen.compliance.attestation.signingKeySecretName` | `""` | Secret holding the signing keypair (`private.pem`, `public.pem`). Empty has the operator generate one into `kitchen-attestation-key`. |
| `kitchen.observability.clickhouse.retentionDays` | `30` | Telemetry retention. |
| `kitchen.observability.hubble.relayAddress` | `""` | host:port of Hubble Relay's gRPC endpoint (e.g. `hubble-relay.kube-system.svc.cluster.local:80`). When set, the operator ships flow observations into the telemetry store for the dashboard's traffic view. Empty disables flow collection. |
| `kitchen.observability.metrics.enabled` | `true` | The operator's half of the environment history: restarts, OOM kills, configured limits and replica counts, sampled off the API server and exported to the agent over OTLP. CPU and memory come from the agent's kubelet scrape instead. |
| `kitchen.observability.metrics.intervalSeconds` | `30` | Seconds between samples. |
| `kitchen.observability.traces.enabled` | `true` | Hand every environment the agent's OTLP address through the standard OTLP environment variables. The receiver itself belongs to the agent and runs with it. |
| `kitchen.observability.traces.port` | `4318` | Port applications export to: the agent's OTLP/HTTP receiver and its Service — OTLP/HTTP's registered one. |
| `kitchen.observability.traces.endpoint` | `""` | What applications are told to export to. Empty means the agent's own in-cluster Service. |
| `kitchen.observability.traces.service.annotations` | `{}` | |
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
| `collector.enabled` | `true` | Run the telemetry agent. Off means no logs, no metrics and no OTLP endpoint. Skipped when there is no store. |
| `collector.image.repository` / `.tag` | `otel/opentelemetry-collector-contrib` / `0.158.0` | Pinned: the operator's DDL tracks this exporter version. |
| `collector.logs.enabled` | `true` | Tail the node's container log files. |
| `collector.logs.excludeNamespaces` | `[]` | Namespaces whose containers are not tailed. |
| `collector.logs.exclude` | `[]` | Extra log file globs to skip. |
| `collector.logs.maxStructuredFields` | `64` | Fields kept from a JSON line, queryable as `http.status:500`. Beyond it the line ships without them. |
| `collector.logs.levelFields` | `[level, severity, lvl]` | JSON fields a line's severity may be written under, first match wins. Non-JSON lines are scanned for the common spellings. |
| `collector.logs.traceIdFields` / `.spanIdFields` | `[trace_id, traceId, …]` | Fields a line's trace and span ids may be written under, first match wins. Lifting them into `TraceId`/`SpanId` is what links a log line to its trace. |
| `collector.metrics.kubelet.enabled` | `true` | Scrape the node's kubelet for per-pod and per-container CPU and memory. |
| `collector.metrics.node.enabled` | `true` | Scrape the node itself: CPU, load, memory, network, disk, filesystem. |
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
| `scaleToZero.enabled` | `false` | Idle environments down to no pods. Needs KEDA and the HTTP add-on in the cluster, installed separately — see [Scale to zero](#scale-to-zero). |
| `scaleToZero.interceptor.service` | `keda-add-ons-http-interceptor-proxy` | Interceptor Service idling environments are routed through. The add-on names it after its own chart, so this is a constant. |
| `scaleToZero.interceptor.namespace` | `keda` | Namespace the HTTP add-on was installed into. |
| `scaleToZero.interceptor.port` | `8080` | Port the interceptor accepts traffic on. |
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
