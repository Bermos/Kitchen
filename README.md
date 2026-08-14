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
- `cmd/gate/`, `internal/previewgate/` — the forward-auth gate protected preview
  URLs are served through
- `charts/kitchen/` — the Helm chart that deploys all of it

## First-time setup

Kitchen expects to be the only thing in its cluster, so it brings most of its
own dependencies — including cert-manager. What it does *not* bring is the
things a cluster needs before it can run anything at all.

### 1. Cluster prerequisites

Four things, none of which the chart installs:

- **Cilium as the CNI**, with `gatewayAPI.enabled=true` and kube-proxy
  replacement. Its Gateway API implementation *is* the ingress; there is no
  separate ingress controller.
- **Gateway API CRDs**, at the version your Cilium requires — this moves faster
  than you would expect, so check Cilium's docs rather than guessing.
- **A default StorageClass.** ClickHouse and the identity provider's Postgres
  are StatefulSets with `volumeClaimTemplates` and no `storageClass` set, so
  they take the cluster default. Without one they sit `Pending` forever, and
  that is the most common way a first install appears to hang.
- **A reachable address for the Gateway** — on bare metal, Cilium LB IPAM plus
  L2 announcements or BGP. cloudflared sidesteps needing a routable address,
  but not needing an address: Cilium will not mark a Gateway `Programmed`
  without one.

### 2. Wildcard DNS and a Cloudflare token

Every generated URL is `<slug>.<baseDomain>`, so you need a wildcard record and
a wildcard certificate. Kitchen obtains the certificate over ACME **DNS-01**,
which is not a preference — ACME issues wildcards no other way, however
reachable your cluster is.

```sh
kubectl create namespace kitchen-system
kubectl -n kitchen-system create secret generic cloudflare-api-token \
  --from-literal=api-token=<token>
```

The token needs `Zone:DNS:Edit` on the zone and `Zone:Zone:Read` to find it.

> Note: Cloudflare's Universal SSL only covers one level of subdomain. If your
> base domain is itself a subdomain — `apps.example.com`, so app URLs are
> `<slug>.apps.example.com` — an edge certificate needs Advanced Certificate
> Manager. This does not affect DNS-01 issuance, only proxied Cloudflare edge
> TLS.

### 3. Install

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.acme.email=you@example.com \
  --set kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName=cloudflare-api-token
```

That brings up cert-manager, the operator and its CRDs, the git webhook
receiver, the REST API at `kitchen.apps.example.com/api/v1/`, a single-node
ClickHouse for telemetry with the collector that fills it, and the identity
provider at `auth.apps.example.com` with its Postgres.

Set `kitchen.tls.mode=none` to start without TLS, or `cert-manager.enabled=false`
if your cluster already runs one.

### 4. Point DNS at the Gateway

```sh
kubectl get kitchen default -o jsonpath='{.status.gatewayAddress}'
```

Point `*.apps.example.com` at that address.

### 5. Wait for the certificate

The wildcard certificate is requested by the operator rather than by Helm:
cert-manager's webhook has to be serving before a `Certificate` is admitted,
so it appears a reconcile after the release, not with it.

```sh
kubectl get kitchen default \
  -o jsonpath='{.status.conditions[?(@.type=="CertificateReady")].message}'
```

A DNS-01 order takes a minute or two. Failures — a token without the right
scopes, a zone it cannot see — are reported in that message.

### 6. Create the first administrator

With the one-time link `helm install` prints:

```sh
echo "https://auth.apps.example.com/bootstrap?token=$(kubectl -n kitchen-system \
  get secret kitchen-auth -o jsonpath='{.data.bootstrapToken}' | base64 -d)"
```

Everything the API serves is behind that login. With a token:

```sh
curl -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects
```

Every value, and the upgrade/uninstall semantics, are documented in
[the chart's README](charts/kitchen/README.md).

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
deploys. Still missing: Connection/Domain/ResourceClaim reconcilers (including
`oidcClient` claims), metrics/traces/flow collection, Infisical sync and the
Vue UI — none of which the chart deploys yet.
