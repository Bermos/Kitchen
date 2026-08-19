# Kitchen 🍳

*Cause we be cooking.*

A self-hosted Vercel alternative for people who bring their own Kubernetes cluster.
`git push` → build → deploy → URL, on your own cluster, with batteries included.

Deploys as a single Helm chart. Assumes Cilium as the cluster CNI (Hubble for traffic
observability, Cilium Gateway API for ingress). All telemetry — logs, metrics, traces,
flow data — lands in ClickHouse: one OpenTelemetry collector per node writes the first
three, and the operator writes the flows and owns the schema under all of it.

## Docs

- [Contributing](CONTRIBUTING.md) — commit conventions and how a release is cut
- [Project scope](docs/SCOPE.md) — who it is for, components, decisions, phasing
- [CRD schema](docs/CRDS.md) — the operator's data model and reconcile flows
- [Auth architecture](docs/AUTH.md) — the platform's identity provider
- [REST API](docs/API.md) — the endpoints, and how to get a token for them
- [Compliance](docs/COMPLIANCE.md) — evidence as a byproduct of deployment: the audit
  log, and the attestations attached to every artifact

## Layout

- `api/v1alpha1/` — CRD types (`kitchen.bermos.dev/v1alpha1`): Kitchen, Connection,
  Project, Build, Release, Environment, Domain, ResourceClaim, PlatformUpdate,
  SavedQuery
- `internal/controller/` — one reconciler per CRD
- `internal/api/` — the REST API, behind the platform's identity provider
- `internal/audit/`, `internal/attestation/` — the evidence layer: the hash-chained
  record of every state transition, and the DSSE/in-toto attestations attached to
  built artifacts through OCI referrers
- `internal/flows/`, `internal/usage/` — the telemetry no collector produces:
  Hubble flow observations, and the restarts, OOM kills, resource limits and
  replica counts the operator samples off the API server and exports to the
  node collector over OTLP
- `internal/clickhouse/` — the telemetry store's schema, which the operator
  owns for every table including the ones only the collector writes, and every
  query the API answers out of it
- `config/crd/bases/` — generated CRD manifests
- `cmd/` — operator entrypoint
- `auth/` — the identity provider (better-auth) served at `auth.<baseDomain>`
- `ui/` — the dashboard: a Vue SPA (Nuxt UI on Reka components) the operator
  embeds and serves at `kitchen.<baseDomain>`, talking only to the REST API
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
  separate ingress controller. Turn **Hubble and Hubble Relay** on with it:
  every request to every application crosses the Gateway's Envoy, and Relay is
  the only place the platform can observe them from. The chart README's
  [prerequisites](charts/kitchen/README.md#prerequisites) name the two settings
  worth choosing rather than inheriting — query-string redaction, and the
  per-node event buffer that decides whether traffic numbers are complete.
- **Gateway API CRDs**, at the version your Cilium requires — this moves faster
  than you would expect, so check Cilium's docs rather than guessing. CI does
  the same, resolving the version from the Cilium release it targets.
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

Create a Cloudflare API token with `Zone:DNS:Edit` on the zone and
`Zone:Zone:Read` to find it. You will hand it to the cluster in step 3, once
the namespace exists.

> Note: Cloudflare's Universal SSL only covers one level of subdomain. If your
> base domain is itself a subdomain — `apps.example.com`, so app URLs are
> `<slug>.apps.example.com` — an edge certificate needs Advanced Certificate
> Manager. This does not affect DNS-01 issuance, only proxied Cloudflare edge
> TLS.

### 3. Install

```sh
helm install kitchen oci://ghcr.io/bermos/charts/kitchen \
  --namespace kitchen-system --create-namespace \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.acme.email=you@example.com \
  --set kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName=cloudflare-api-token

kubectl -n kitchen-system create secret generic cloudflare-api-token \
  --from-literal=api-token=<token>
```

That brings up cert-manager, the operator and its CRDs, the git webhook
receiver, the REST API at `kitchen.apps.example.com/api/v1/`, a single-node
ClickHouse for telemetry with the collector that fills it, the identity
provider at `auth.apps.example.com` with its Postgres, and an image registry at
`registry.apps.example.com` for builds to push to.

**Use `--create-namespace`, and do not create the namespace yourself.** The
chart manages `kitchen-system` so its Pod Security level is set rather than
inherited: the collector mounts the node's `/var/log`, and `hostPath` is
admitted at the `privileged` level alone, so on a cluster defaulting to
`baseline` — Talos is one — an unlabelled namespace means nothing is collected
at all. Helm writes its release record into the namespace before applying
any manifest, so the chart cannot bootstrap the namespace it installs into; it
can only adopt the empty one `--create-namespace` just made. A namespace
created with `kubectl create namespace` carries no Helm ownership metadata and
**fails the install** rather than being adopted. If you have one already, see
[adopting an existing namespace](charts/kitchen/README.md#adopting-an-existing-namespace).

The secret comes after the install because it lives in that namespace. That
ordering is fine: the operator creates the `ClusterIssuer` and `Certificate`
either way, cert-manager cannot solve the DNS-01 challenge until the token is
there, and both it and the operator keep retrying. Progress shows up in
`CertificateReady` (step 5), so a late secret costs a reconcile, not a
reinstall.

Set `kitchen.tls.mode=none` to start without TLS — leave the two
`kitchen.tls.acme` values off with it; they are only read in `acme` mode, which
in turn is only accepted with them. Or `cert-manager.enabled=false`
if your cluster already runs one. In `none` mode the Gateway listens on HTTP
alone, and every URL the platform publishes — the OIDC issuer, the API's
external URL, preview and app URLs — is `http://` to match, so logins and the
API still work before DNS and certificates exist.

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

In `none` mode there is also **no bundled registry**: it is published on the
base domain because the node's container runtime is what pulls an image, and
the platform's own wildcard certificate is the only address every node already
trusts. Without TLS there is nothing to trust, so nothing is published and the
`RegistryReady` condition says so — projects then need a registry connection of
their own. See [the bundled registry](charts/kitchen/README.md#the-bundled-registry).

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

Signing in lands on the dashboard, and from there nothing needs `kubectl`:
connections, projects, builds, environments, domains — creating, changing and
deleting them all have a screen, and everything a screen does goes through the
same REST API a script can call.

Creating a project asks for two connections: a git one, which is yours to add,
and a registry, which the platform has already seeded pointing at the one it
runs itself. Push, and the first build pushes an image and deploys it.

### Checking what is running

The dashboard's settings page and status bar show the operator's component
survey — which platform workloads are short of pods, and why. The same data is
on the singleton for a terminal:

```sh
kubectl get kitchen default
```

```
NAME      BASEDOMAIN         GATEWAY        COMPONENTS   AGE
default   apps.example.com   203.0.113.10   6/6 healthy  5h
```

For the breakdown, including why anything is short of pods:

```sh
kubectl get kitchen default -o jsonpath='{range .status.components[*]}{.name}{"\t"}{.available}/{.desired}{"\t"}{.message}{"\n"}{end}'
```

This is worth checking on a first install, because the interesting failures are
invisible in the obvious places. A workload whose pods are refused at admission
has no pods at all — `kubectl get pods` shows nothing wrong, because there is
nothing there to be wrong — so it is the counts, and the warning event the
survey attaches to them, that tell you.

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

## Versioning and releases

Kitchen is versioned with [SemVer](https://semver.org/spec/v2.0.0.html), and
one number covers the whole platform: a release publishes the operator image,
the auth image and the Helm chart together, and that number is what the
dashboard shows in its sidebar and on the settings page. While the major
version is 0, **a breaking change bumps the minor** — read the release notes
before upgrading across one.

Commit messages are
[Conventional Commits](https://www.conventionalcommits.org/en/v1.0.0/), which
is what makes the rest automatic: CI rejects a message (and a pull request
title) outside the spec, release-please turns the ones that land on `main` into
the next version number and the entry in `CHANGELOG.md`, and merging the
release pull request it opens tags `vX.Y.Z` and publishes everything.

```sh
make hooks                # reject a bad commit message before CI does
make check-commits        # check what this branch already has
```

[CONTRIBUTING.md](CONTRIBUTING.md) has the type table and the whole release
path.

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

Idle environments can cost nothing: with KEDA and its HTTP add-on in the
cluster (two `helm install`s of their own) and `scaleToZero.enabled`, an
environment nobody is using drops to no pods at all until the next request to
its URL starts it again. Previews idle by default, production only when a
project asks — see [the chart README](charts/kitchen/README.md#scale-to-zero).

Reconcilers: Kitchen (shared Gateway, optional cloudflared, telemetry schema,
the preview gate and its OAuth client), Project (webhook registration,
namespace, connection validation), Connection (credential probes against the
live provider, capabilities), Build, Environment. The Helm chart ships the
operator, its CRDs, the git webhook route, the REST API, ClickHouse with the
OpenTelemetry collector DaemonSet that fills it — container and build logs,
pod and node metrics, and the OTLP applications export — and the identity
provider with its Postgres; tagged releases publish both images and the chart
to GHCR.

The identity provider serves OIDC discovery, JWKS and dynamic client
registration, and hands the operator a service credential to register clients
with — which is how the preview gate gets its own OAuth client. Logs are
queryable by project, environment and build as soon as a build runs or an app
deploys.

The Vue dashboard is served by the operator at `kitchen.<baseDomain>`, behind
the platform login (OIDC Authorization Code + PKCE): projects with editable
settings and a full create/delete lifecycle, builds with their logs and
cancellation, environments with one-click rollback and preview teardown,
connections with create/rotate/delete — credentials go to the operator and are
never read back — and the editable platform settings, with an operator mode
that surfaces `status.conditions` on everything and the Kubernetes objects the
operator materialized for an environment. Resource claims provision through
their connection (Neon Postgres first, a DB branch per preview with
`previewBranching`) with create/delete in the dashboard, and custom domains
attach from the environment screen — the dashboard shows the DNS record to
create and tracks verification, certificate and routing live. Still missing:
`oidcClient` claims and Infisical sync.
