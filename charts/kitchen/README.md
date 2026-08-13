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
- **Gateway API CRDs** (`gateway.networking.k8s.io/v1`).
- **A reachable address for the Gateway**: Cilium L2 announcements or BGP for a
  LoadBalancer IP — or cloudflared, which sidesteps both.
- **Wildcard DNS** for `*.<baseDomain>`, pointed at that address.
- **cert-manager** if `kitchen.tls.mode=acme`, to issue the wildcard
  certificate into the secret `kitchen-wildcard-tls` in `kitchen-system`.

## Install

The operator resolves its platform namespace from a compiled-in constant, so
the chart must be installed into `kitchen-system`:

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

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
kubectl -n kitchen-system create secret generic kitchen-tunnel --from-literal=token=<tunnel-token>

helm install kitchen ./charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.mode=cloudflared \
  --set kitchen.ingress.cloudflared.enabled=true \
  --set kitchen.ingress.cloudflared.tunnelSecretName=kitchen-tunnel
```

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
| `message` | the line |
| `labels` | every pod label, for anything the columns miss |

The table (`logs`) is created by the operator, not the chart — it is applied
from `Kitchen.spec.observability.clickhouse`, and its TTL follows
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

## Upgrade

```sh
helm upgrade kitchen ./charts/kitchen --namespace kitchen-system --reuse-values
```

CRDs ship as ordinary templates rather than in the chart's `crds/` directory,
so `helm upgrade` applies schema changes — that is the CRD migration path.
Removing a CRD field still needs care: the API server rejects a stored object
that no longer validates, so land conversion work before shipping a breaking
schema.

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
every custom resource they hold, and the `Kitchen` singleton — uninstalling the
control plane should not delete every project and its running environments. To
tear it all down:

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
| `crds.install` | `true` | Install the `kitchen.bermos.dev` CRDs. |
| `crds.keep` | `true` | Keep CRDs (and custom resources) on uninstall. |
| `kitchen.create` | `true` | Create the `Kitchen` singleton. Needs `baseDomain`. |
| `kitchen.applyOnUpgrade` | `false` | Re-apply the singleton on every upgrade. |
| `kitchen.baseDomain` | `""` | Generated URLs are `<slug>.<baseDomain>`. |
| `kitchen.api.externalURL` | `""` | Defaults to `https://kitchen.<baseDomain>`. |
| `kitchen.ingress.gatewayClassName` | `cilium` | GatewayClass for the shared Gateway. |
| `kitchen.ingress.cloudflared.enabled` | `false` | Run a cloudflared tunnel as the edge. |
| `kitchen.ingress.cloudflared.tunnelSecretName` | `""` | Secret with the tunnel token under `token`. |
| `kitchen.tls.mode` | `acme` | `acme`, `cloudflared` or `none`. |
| `kitchen.auth` | from `auth.*` | The singleton's `auth` block mirrors `auth.enabled` and the resolved host. |
| `kitchen.builds.defaultStrategy` | `auto` | `auto`, `dockerfile` or `buildpacks`. |
| `kitchen.builds.concurrency` | `2` | Builds running at once. |
| `kitchen.observability.clickhouse.retentionDays` | `30` | Telemetry retention. |
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
| `logs.resources` | 100m/128Mi → 512Mi | |
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
