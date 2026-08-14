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
- [REST API](docs/API.md) — the endpoints, and how to get a token for them

## Layout

- `api/v1alpha1/` — CRD types (`kitchen.bermos.dev/v1alpha1`): Kitchen, Connection,
  Project, Build, Release, Environment, Domain, ResourceClaim
- `internal/controller/` — one reconciler per CRD (stubs for now)
- `internal/api/` — the REST API, behind the platform's identity provider
- `config/crd/bases/` — generated CRD manifests
- `cmd/` — operator entrypoint
- `auth/` — the identity provider (better-auth) served at `auth.<baseDomain>`
- `ui/` — the dashboard: a Vue SPA (Nuxt UI on Reka components) the operator
  embeds and serves at `kitchen.<baseDomain>`, talking only to the REST API
- `cmd/gate/`, `internal/previewgate/` — the forward-auth gate protected preview
  URLs are served through
- `charts/kitchen/` — the Helm chart that deploys all of it

## Install

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com
```

That brings up the operator, its CRDs, the git webhook receiver, the REST API
at `kitchen.apps.example.com/api/v1/`, a single-node ClickHouse for telemetry
with the collector that fills it, and the identity provider at
`auth.apps.example.com` with its Postgres.

Then point `*.apps.example.com` at the shared Gateway:

```sh
kubectl get kitchen default -o jsonpath='{.status.gatewayAddress}'
```

Create the first administrator with the one-time link `helm install` prints:

```sh
echo "https://auth.apps.example.com/bootstrap?token=$(kubectl -n kitchen-system \
  get secret kitchen-auth -o jsonpath='{.data.bootstrapToken}' | base64 -d)"
```

Everything the API serves is behind that login. With a token:

```sh
curl -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects
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

So is the dashboard. `npm run dev` proxies `/api` to a locally running
operator (`make run`), and `make ui-build` stages the built SPA for embedding
into the manager binary — the image build does the same on its own:

```sh
cd ui
npm install
npm run dev               # vite dev server against a local operator
npm run build && npm run typecheck && npm test
```

## Status

The core pipeline is implemented end to end:
`git push` → webhook receiver → Build (BuildKit job) → Release → Environment
→ Deployment + Service + HTTPRoute on the shared Gateway, with per-PR preview
environments and instant rollbacks by design.

Preview URLs are gated behind platform login by default: a protected preview's
route goes through the operator-run forward-auth gate, which sends anonymous
visitors to the identity provider and proxies the ones that come back — the
deployed app needs no changes. Turn it off per project with
`spec.previews.protected: false`.

Reconcilers: Kitchen (shared Gateway, optional cloudflared, telemetry schema,
the preview gate and its OAuth client), Project (webhook registration,
namespace, connection validation), Build, Environment. The Helm chart ships the
operator, its CRDs, the git webhook route, the REST API, ClickHouse with a
Vector DaemonSet shipping container and build logs into it, and the identity
provider with its Postgres; tagged releases publish both images and the chart
to GHCR.

The identity provider serves OIDC discovery, JWKS and dynamic client
registration, and hands the operator a service credential to register clients
with — which is how the preview gate gets its own OAuth client. Logs are
queryable by project, environment and build as soon as a build runs or an app
deploys.

The Vue dashboard is served by the operator at `kitchen.<baseDomain>`, behind
the platform login (OIDC Authorization Code + PKCE): projects, builds with
their logs, environments with one-click rollback, connections and the editable
platform settings, with an operator mode that surfaces `status.conditions` on
everything. Still missing: Connection/Domain/ResourceClaim reconcilers
(including `oidcClient` claims), metrics/traces/flow collection, Infisical
sync, and create flows in the UI (projects and connections are still
`kubectl apply`).
