# CLAUDE.md — working on Kitchen

Kitchen is a self-hosted Vercel alternative: a kubebuilder operator plus a Helm
chart that deploys the whole platform. See [README.md](README.md) for what it
is and [docs/SCOPE.md](docs/SCOPE.md) for why it is shaped that way.

## The premise that drives most design decisions

**Kitchen owns the cluster it is installed into.** Nothing else is meant to run
there. That is why it bundles platform dependencies (cert-manager) rather than
listing them as prerequisites, and why cluster-scoped singletons are acceptable
where they normally would not be.

Two exceptions stay prerequisites, because they must exist before the cluster
can run anything: **Cilium** (it is the CNI) and a **default StorageClass**.

## Regeneration

Anything under `api/` or any `+kubebuilder:rbac` marker feeds generated files.
After editing either:

```sh
make manifests helm-manifests   # CRDs, RBAC, and the chart templates derived from them
make test                       # also runs go fmt and regenerates deepcopy
```

CI fails if the checked-in output differs from a fresh run. `make test` needs
envtest binaries; it downloads them on first use.

## Chart conventions

- **CRDs are ordinary templates, not `crds/` files.** Helm never upgrades files
  in a `crds/` directory, so schema changes would silently never apply. Both
  Kitchen's CRDs and the bundled cert-manager's work this way — the latter is
  precisely what made bundling it safe. `hack/gen-helm-manifests.sh` generates
  Kitchen's from `config/crd/bases`; do not hand-edit `templates/crds.yaml`.
- **Every template starts with `{{- include "kitchen.validate" . -}}`.** All
  configuration guards live in that one helper in `_helpers.tpl`, and each
  `fail` message says both what is wrong and how to fix it.
- **Values carry `# --` doc comments** (helm-docs style) and are mirrored in the
  values table in `charts/kitchen/README.md`.
- **CI renders the chart with default values** in many combinations. A guard
  that fails on defaults — including anything using
  `.Capabilities.APIVersions.Has`, which is empty under `helm template` — breaks
  all of them. Gate on *configuration* the user supplied, not on cluster
  capabilities.
- **`.helmignore` patterns without a slash match at any depth.** A bare `*.tgz`
  also matches vendored sub-charts in `charts/`, and the loader then cannot see
  them: `helm dependency list` reports `ok` while `helm template` fails with
  "missing in charts/ directory". It is anchored as `/*.tgz` for this reason.

## Chart vs. operator: who creates what

The chart creates what Helm can create safely. The operator creates what needs
to wait for something.

Concretely: the shared Gateway, the cloudflared Deployment, the preview gate,
the ACME `ClusterIssuer` and the wildcard `Certificate` are all created by
`KitchenReconciler`, not by templates. For the cert-manager objects the reason
is hard — cert-manager's own webhook admits them, so on a first install they
cannot exist until it is serving, and a reconcile loop can requeue where a Helm
release simply fails.

When adding a dependency that ships an admission webhook for its own CRs,
assume its CRs belong in the operator.

cert-manager's kinds are addressed as `unstructured` objects rather than
through its Go types, to avoid tying the build to its release cadence.

## Things that are true and easy to get wrong

- **Wildcard certificates are DNS-01 only.** ACME cannot issue `*.example.com`
  over HTTP-01, no matter how reachable the cluster is. Every generated URL is
  a subdomain, so the platform always needs a wildcard.
- **cloudflared does not remove the need for a LoadBalancer address**, only for
  it to be routable. Cilium reports `Programmed=False` / `AddressNotAssigned`
  without one, and the platform never goes ready. Verified on bare metal.
- **The Gateway API CRD version is pinned by Cilium**, not by us, and it moves
  quickly. Check Cilium's docs for the release in use. CI does exactly that:
  `.github/workflows/helm.yml` pins `CILIUM_VERSION` and reads the CRD version
  out of that release's `Documentation/conf.py` at job time, so the two cannot
  drift apart. `GATEWAY_API_VERSION` there is only the fallback for when the
  lookup cannot reach GitHub; the job warns when the two disagree. Bump
  `CILIUM_VERSION` to move CI forward — the CRD version follows.
- **`kitchen.tls.mode` decides the scheme of every published URL**, not just
  whether a certificate is managed. Mode `none` gives the shared Gateway an
  HTTP listener alone, so the OIDC issuer, the API's external URL and generated
  app URLs are `http://` there. The chart derives it in `kitchen.scheme`, the
  operator in `TLSMode.Scheme()`; anything new that builds a public URL goes
  through one of them rather than writing `https://`.
- **`kitchen-wildcard-tls` is a compiled-in constant**
  (`WildcardTLSSecretName`), because the Gateway's HTTPS listener references it
  directly. It is deliberately not configurable.
- **The platform namespace is `kitchen-system`**, also compiled in. The chart
  refuses to render elsewhere unless `namespaceCheck=false`.
- **The log collector needs Pod Security `privileged`**, because it mounts the
  node's `/var/log` and `baseline` forbids `hostPath` outright. The chart does
  not create the namespace, so `--create-namespace` inherits the cluster
  default: kind is `privileged` and CI never notices, Talos is `baseline` and
  the collector silently never starts. A DaemonSet whose pods are refused at
  admission has **no pods at all** — `kubectl get pods` looks clean, and the
  rejection is a `FailedCreate` event on the DaemonSet. That failure mode is
  what `status.components` on the Kitchen singleton exists to surface.
- **Anything that should appear in the component survey needs
  `app.kubernetes.io/part-of: kitchen`** and, to be readable,
  `app.kubernetes.io/component`. The survey selects on the former rather than on
  names, since every chart-generated name is release-name prefixed. Use
  `platformLabels()` for operator-created workloads — and note the selector name
  is passed separately, because a Deployment's selector is immutable and must
  keep whatever value it already had.
