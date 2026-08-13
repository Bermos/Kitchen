# Kitchen 🍳

*Cause we be cooking.*

A self-hosted Vercel alternative for people who bring their own Kubernetes cluster.
`git push` → build → deploy → URL, on your own cluster, with batteries included.

Deploys as a single Helm chart. Assumes Cilium as the cluster CNI (Hubble for traffic
observability, Cilium Gateway API for ingress). All telemetry — logs, metrics, traces,
flow data — lands in ClickHouse.

## Docs

- [Project scope](docs/SCOPE.md) — components, decisions, phasing
- [CRD schema](docs/CRDS.md) — the operator's data model and reconcile flows

## Layout

- `api/v1alpha1/` — CRD types (`kitchen.bermos.dev/v1alpha1`): Kitchen, Connection,
  Project, Build, Release, Environment, Domain, ResourceClaim
- `internal/controller/` — one reconciler per CRD (stubs for now)
- `config/crd/bases/` — generated CRD manifests
- `cmd/` — operator entrypoint
- `charts/kitchen/` — the Helm chart that deploys all of it

## Install

```sh
helm install kitchen ./charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

Then point `*.apps.example.com` at the shared Gateway:

```sh
kubectl get kitchen default -o jsonpath='{.status.gatewayAddress}'
```

Cluster prerequisites (Cilium with Gateway API, wildcard DNS, cert-manager or
cloudflared), every value, and the upgrade/uninstall semantics are documented
in [the chart's README](charts/kitchen/README.md).

## Development

Standard kubebuilder project:

```sh
make generate manifests   # regenerate deepcopy + CRDs after editing api/
make helm-manifests       # sync the chart's CRD + RBAC templates with config/
make test                 # unit + envtest suite
make run                  # run the operator against the current kubecontext
make install              # install CRDs into the cluster
make helm-lint            # lint the chart
make helm-install BASE_DOMAIN=apps.example.com IMG=ghcr.io/bermos/kitchen:dev
```

## Status

The core pipeline is implemented end to end:
`git push` → webhook receiver → Build (BuildKit job) → Release → Environment
→ Deployment + Service + HTTPRoute on the shared Gateway, with per-PR preview
environments and instant rollbacks by design.

Reconcilers: Kitchen (shared Gateway + optional cloudflared), Project
(webhook registration, namespace, connection validation), Build, Environment.
The Helm chart ships the operator, its CRDs and the git webhook route.
Still missing: Connection/Domain/ResourceClaim reconcilers, ClickHouse +
collectors, Infisical sync, the operator REST API and the Vue UI — none of
which the chart deploys yet.
