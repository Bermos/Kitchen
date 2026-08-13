# Kitchen Helm chart

Deploys the Kitchen operator — CRDs, RBAC, the controller manager, the git
webhook receiver and its route on the shared Gateway — plus the `Kitchen`
singleton that holds platform configuration.

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
helm install kitchen ./charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

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
| `kitchen.builds.defaultStrategy` | `auto` | `auto`, `dockerfile` or `buildpacks`. |
| `kitchen.builds.concurrency` | `2` | Builds running at once. |
| `kitchen.observability.clickhouse.retentionDays` | `30` | Telemetry retention. |
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
