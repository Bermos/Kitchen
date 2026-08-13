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
- [Auth architecture](docs/AUTH.md) — the platform's identity provider

## Layout

- `api/v1alpha1/` — CRD types (`kitchen.bermos.dev/v1alpha1`): Kitchen, Connection,
  Project, Build, Release, Environment, Domain, ResourceClaim
- `internal/controller/` — one reconciler per CRD (stubs for now)
- `config/crd/bases/` — generated CRD manifests
- `cmd/` — operator entrypoint
- `auth/` — the identity provider (better-auth) served at `auth.<baseDomain>`
- `charts/kitchen/` — the Helm chart that deploys all of it

## Install

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

That brings up the operator, its CRDs, the git webhook receiver, a single-node
ClickHouse for telemetry, and the identity provider at `auth.apps.example.com`
with its Postgres.

Then point `*.apps.example.com` at the shared Gateway:

```sh
kubectl get kitchen default -o jsonpath='{.status.gatewayAddress}'
```

Create the first administrator with the one-time link `helm install` prints:

```sh
echo "https://auth.apps.example.com/bootstrap?token=$(kubectl -n kitchen-system \
  get secret kitchen-auth -o jsonpath='{.data.bootstrapToken}' | base64 -d)"
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

The auth service is a Node project of its own:

```sh
cd auth
npm install
npm run typecheck
npm test                  # integration tests, need a Postgres
```

## Status

The core pipeline is implemented end to end:
`git push` → webhook receiver → Build (BuildKit job) → Release → Environment
→ Deployment + Service + HTTPRoute on the shared Gateway, with per-PR preview
environments and instant rollbacks by design.

Reconcilers: Kitchen (shared Gateway + optional cloudflared), Project
(webhook registration, namespace, connection validation), Build, Environment.
The Helm chart ships the operator, its CRDs, the git webhook route, ClickHouse
and the identity provider with its Postgres; tagged releases publish both
images and the chart to GHCR.

The identity provider serves OIDC discovery, JWKS and dynamic client
registration, and hands the operator a service credential to register clients
with. Still missing: Connection/Domain/ResourceClaim reconcilers (including
`oidcClient` claims), the collectors that fill ClickHouse, Infisical sync, the
operator REST API and the Vue UI — none of which the chart deploys yet.
