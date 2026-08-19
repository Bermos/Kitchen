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

## Nothing needs kubectl

**Abstracting the cluster away is the product** (issue #60). Anything a
developer or an operator does in the normal running of the platform has a
route and a screen: a feature is not done when its reconciler works, it is
done when the operation exists in the REST API and the dashboard. This holds
for every future feature — a new capability that only works via `kubectl
apply` is an unfinished feature, and the docs must never point at `kubectl`
for something the dashboard can do.

The corollaries that shape how such writes are built:

- **The API never reads credentials back.** Writing a credential means the
  operator creates the Secret from the request body; no response ever echoes
  it. Secrets the API wrote carry `app.kubernetes.io/managed-by: kitchen` and
  are deleted with their connection — secrets anything else wrote are not.
- **A write surface waits for its reconciler.** Domain and ResourceClaim
  writes are deliberately absent while their reconcilers are stubs: an API
  over objects nothing reconciles only looks like it works.
- **Destructive writes are honest about blast radius.** Project deletion is a
  finalizer that tears down environments, builds, releases, domains, claims
  and the app namespace (nothing owner-references them — the finalizer is the
  garbage collector); the UI confirms by typing the name. Deletions the
  operator finishes asynchronously answer `202`.
- The allowed exceptions are cluster bootstrap (install, DNS, the Cloudflare
  token secret, the bootstrap link) and deploy-time chart values — and a
  setting that stays chart-only should be a deliberate decision, not a gap.

## Commits

**Every commit message is a
[Conventional Commit](https://www.conventionalcommits.org/en/v1.0.0/)**, no
exceptions: `<type>[(scope)][!]: <description>`. Types are `build`, `chore`,
`ci`, `docs`, `feat`, `fix`, `perf`, `refactor`, `revert`, `style`, `test`;
scopes are free-form and usually name the piece (`chart`, `operator`, `api`,
`ui`, `auth`, `deps`). No full stop on the description, blank line before any
body, subject under 100 characters.

This is machine-read, not decorative. release-please derives the next version
and the change notes from these messages, so the wrong type is the wrong
version number and a non-conforming message is a change missing from the
release notes. Choose the type by what it does to the version — `feat` bumps
the minor, `fix`/`perf`/`revert` the patch, everything else nothing — rather
than by which word describes the diff most flatteringly.

`hack/check-commit-message.sh` is the one implementation of these rules; the
Commits workflow runs it over every commit in a pull request **and over the
pull request title**, because squash-merging makes the title the subject that
lands on main. When opening a pull request, write the title as a Conventional
Commit. `make hooks` installs the same check as a local `commit-msg` hook, and
`make check-commits` runs it over what the branch already has.

Breaking changes take `!` and a `BREAKING CHANGE:` footer. **While Kitchen is
on 0.x that bumps the minor, not the major** — that is
`bump-minor-pre-major` in `release-please-config.json`, and it is the correct
pre-1.0 reading of SemVer, not an oversight.

## Releases

Never edit a version number, and never write a changelog entry. Both are
release-please's, driven by the commits on `main`: it keeps a release pull
request open, and merging it creates the GitHub release as a **draft** and
calls the publish workflow, which ships both images and the chart under that
one number and only then publishes the draft — which is also what creates the
`vX.Y.Z` tag.

- **The release is a draft until every artifact exists.** `"draft": true` in
  `release-please-config.json` plus the `finalize` job in `publish.yml`, which
  needs all three artifact jobs, attaches the chart, appends the resolved
  digests to the notes and flips it live. The release object used to be written
  before the artifacts, which is how `0.5.1` and `0.6.0` came to be live
  releases with two images and no chart behind them. A publish that fails now
  leaves a draft and no tag; re-running the Publish workflow by hand with the
  same version finds the draft and finishes it.
- **No release pull request is not a broken workflow.** release-please opens
  one only when the commits since the last tag contain something that bumps a
  version: `feat`, `fix`, `perf`, `revert`, or a breaking change. A batch of
  nothing but `ci`, `docs`, `chore`, `refactor`, `test`, `style` and `build`
  produces no pull request at all, because there is no version to move to.
  That is correct, and it looks exactly like a jammed workflow — check the
  types on the commits before going looking for a fault. It also means a
  change worth shipping that is typed `chore` will not ship.
- **Adding a file that spells the version out means adding it to
  `release-please-config.json`.** `charts/kitchen/Chart.yaml` (annotated
  `# x-release-please-version` on both `version` and `appVersion`) and the
  `package.json`/`package-lock.json` pairs under `ui/` and `auth/` are there
  already. A file that is not listed silently keeps the old number forever.
- **Every `extra-files` entry names its updater; never list a path as a bare
  string.** A bare string is resolved by file extension, so a `.yaml` file goes
  to the YAML updater: it rewrites the document through a parser, which strips
  every comment in the file — the `# x-release-please-version` annotations
  included, so the next release cannot find the version at all — and it sets
  only `$.version`, leaving `appVersion` behind. `Chart.yaml` is listed as
  `{"type": "generic", ...}` for that reason, which edits the annotated lines
  in place and touches nothing else. This is not what the config schema's
  description of string entries implies; it is what release-please 17 does.
- **release-please owns `CHANGELOG.md`** and rewrites the top of it. Nothing
  else writes to that file — prose about the release process goes in
  [CONTRIBUTING.md](CONTRIBUTING.md).
- **The baseline is `.release-please-manifest.json`, and it has to match the
  newest tag that actually exists.** It says `0.1.4` because `v0.1.4` is the
  last release that was published. Setting it lower does not merely produce an
  odd number: the next release would compute a version whose tag is already
  taken, and creating it fails. Tags published before release-please
  (`v0.1.0`–`v0.1.4`) were cut by hand and have no GitHub release object, so
  `bootstrap-sha` pins the commit range explicitly rather than trusting tag
  discovery. The one moment the manifest legitimately runs ahead of the tags is
  between a merged release pull request and a finished publish, because the tag
  arrives with the draft going live.
- **The version reaches the running platform through the linker, not through a
  source file.** `internal/version.Version` defaults to `dev` and is set by
  `-ldflags` (`LDFLAGS` in the Makefile, `ARG VERSION` in the Dockerfile). It
  is served on `/config.json` and shown in the dashboard's sidebar and settings
  page. Anything else that wants to report the version reads that package.
- **`release.yml` calls `publish.yml` through `workflow_call` rather than
  letting the tag trigger it.** A tag pushed with a workflow's own
  `GITHUB_TOKEN` does not start another workflow run, so the `push: tags`
  trigger would never fire for an automated release. It stays only for a tag a
  person pushes.

## Regeneration

Anything under `api/` or any `+kubebuilder:rbac` marker feeds generated files.
After editing either:

```sh
make manifests helm-manifests   # CRDs, RBAC, and the chart templates derived from them
make test                       # also runs go fmt and regenerates deepcopy
```

CI fails if the checked-in output differs from a fresh run. `make test` needs
envtest binaries; it downloads them on first use.

## Before every push

```sh
make lint                       # golangci-lint — CI runs it as its own job
```

`make test` passing does not imply `make lint` does: the linter also checks
test files, and goconst has already caught a release name repeated across
assertions. Run it before every push, not only after touching Go code you
consider "real".

## Checking chart behaviour without waiting for CI

**In a dev container the Docker daemon is installed but not running — start it
with `dockerd &` and wait for `docker info`.** With it up, `kind` reproduces the
whole `helm.yml` install job locally, which is the only way to find out what
Helm actually does before pushing.

Failing that, a question about *Helm itself* can be answered against the
envtest binaries `make setup-envtest` already downloads: `envtest.Environment`
plus `AddUser(...).KubeConfig()` is a real API server, and `helm install`
against it is faithful for anything short of pods running. That is how the
"you cannot bundle a chart that ships a custom resource of another chart's
CRD" rule below was established rather than guessed — four candidate
workarounds, each disproved in about a minute. Note that cert-manager's
`startupapicheck` hook Job can never finish there (no scheduler), so pass
`--set cert-manager.enabled=false` to any full-chart install.

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
the registry's route and its seeded Connection, the ACME `ClusterIssuer` and
the wildcard `Certificate` are all created by `KitchenReconciler`, not by
templates. (The registry itself — StatefulSet, Service, PVC — is a plain
template; only the route needs the Gateway to exist first, and only the
Connection needs a credential the API never reads back.) For the cert-manager objects the reason
is hard — cert-manager's own webhook admits them, so on a first install they
cannot exist until it is serving, and a reconcile loop can requeue where a Helm
release simply fails.

When adding a dependency that ships an admission webhook for its own CRs,
assume its CRs belong in the operator.

**A chart cannot bundle a dependency that ships a custom resource of another
chart's CRD.** Helm builds and validates a release's whole manifest against the
API server before it applies any of it, so a CR whose CRD arrives in the same
release never resolves. That is why KEDA and its HTTP add-on are the one
platform dependency Kitchen does *not* bundle, despite owning its cluster: the
add-on autoscales its own interceptor with a `keda.sh` ScaledObject, whose CRD
comes from the KEDA chart. A `pre-install` hook does not help (the main
manifest is built first), `crds/` fixes install but is never applied on
upgrade, and a bundled copy also collides with a cluster that already runs
KEDA, because Helm will not adopt CRDs another release owns. `scaleToZero`
therefore carries the interceptor's address instead of installing it, and
`EnvironmentReconciler` writes the per-environment `HTTPScaledObject` — one per
Environment, so that was never a template either.

cert-manager's kinds are addressed as `unstructured` objects rather than
through its Go types, to avoid tying the build to its release cadence.

## Things that are true and easy to get wrong

- **Wildcard certificates are DNS-01 only.** ACME cannot issue `*.example.com`
  over HTTP-01, no matter how reachable the cluster is. Every generated URL is
  a subdomain, so the platform always needs a wildcard.
- **`tls.mode: acme` without `tls.acme` is refused at admission**, by a CEL rule
  on the CRD (`x-kubernetes-validations` on `TLSSpec`), as is an `acme` block
  naming no solver. Since `acme` is also the *default* mode, the chart cannot
  render a Kitchen from base values alone: `kitchen.validate` fails unless
  `kitchen.tls.acme.email` and the Cloudflare token secret are set, or the mode
  is something else. Every `helm template` in CI passes them for that reason —
  `CHART_BASE` plus `CHART_ACME` in `.github/workflows/helm.yml`, kept apart
  because an `acme` block in another mode is refused too.
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
  node's `/var/log` and `baseline` forbids `hostPath` outright. The chart owns
  `kitchen-system` (`namespace.create`) so the level is set rather than
  inherited — kind defaults to `privileged` and CI never notices, Talos to
  `baseline`, where the collector silently never starts. A DaemonSet whose pods
  are refused at admission has **no pods at all** — `kubectl get pods` looks
  clean, and the rejection is a `FailedCreate` event on the DaemonSet. That
  failure mode is what `status.components` on the Kitchen singleton exists to
  surface.
- **A chart cannot bootstrap the namespace it installs into**, so
  `--create-namespace` is still required: Helm writes its release record into
  the target namespace *before* applying any manifest, and without the flag the
  install dies with `namespaces "kitchen-system" not found`. The flag creates a
  bare namespace which the template then adopts. Anything created another way
  has no Helm ownership metadata and **fails** the install rather than being
  adopted, which makes this a breaking change for pre-existing installs — they
  need the three ownership keys applied by hand once. Verified against Helm
  4.2.2; Helm 3 does not adopt at all.
- **The namespace is annotated `helm.sh/resource-policy: keep`.** Without it,
  `helm uninstall` deletes the namespace and every PVC in it, so removing the
  release would destroy the accounts database and the telemetry store. Any
  change to that template must keep the annotation.
- **The two build strategies are two different jobs, and only one of them
  fetches its own source.** BuildKit takes the commit as a git context and
  clones it itself; the Cloud Native Buildpacks lifecycle only ever builds a
  directory, so `buildpacks` clones in an init container and hands `creator` a
  path. They agree on the two things the reconciler needs: credentials come
  from `DOCKER_CONFIG`, and the pushed image's digest is left in the pod's
  termination message — BuildKit's as JSON, the lifecycle's as its TOML report,
  which is why `digestFromTerminationMessage` reads both. The buildpacks pod
  asks for none of the seccomp and AppArmor exemptions the BuildKit one does:
  it enters as the builder image's own user, which is what makes the
  lifecycle's chown-and-drop a no-op.
- **Two things the CNB lifecycle will not infer, and both fail the build
  outright.** It has no default platform API — without `CNB_PLATFORM_API` it
  exits before it does anything, saying so. And it drops to `CNB_USER_ID`:`CNB_GROUP_ID`
  from the builder image, which for the pinned Paketo jammy builder is
  **1001:1000**, not the 1000:1000 everything else in this repository runs as;
  a pod entering as a different unprivileged user cannot `setuid` to it and
  dies in `Privileges()`. `cnbUID`/`cnbGID` sit next to `BuildpacksBuilderImage`
  for that reason — bumping the builder means checking them. The clone runs as
  the same user too, because buildpacks write into the application directory
  (npm's modules, the Node buildpack's start script), so source owned by anyone
  else fails the build halfway through.
- **A private registry needs the credential twice: to push and to pull.** The
  build syncs the registry Connection's docker config into the application
  namespace, and the Environment's Deployment names that same Secret as its
  `imagePullSecrets` — `registrySecretName()` is the one place the name is
  spelled. Without it the pods sit in `ImagePullBackOff` while the build, the
  release and the route all read as healthy, which is a long way to walk back
  from.
- **The bundled registry is published on the internet on purpose.** The node's
  container runtime is what pulls an image, and it trusts neither a Service
  address with an in-cluster CA nor plain HTTP unless the node is configured
  to. Every other in-cluster registry solves that at the node — an
  insecure-registry entry, a CA in CRI-O's trust store, a DaemonSet writing
  `certs.d` — and Kitchen is a chart installed into someone else's cluster,
  where Cilium and a StorageClass are the only prerequisites. A route on the
  shared Gateway rides the platform's own publicly trusted wildcard
  certificate, which is the one address that asks the node for nothing. It
  follows that the feature does not exist in `tls.mode: none`, and that the
  registry admits nobody anonymously.
- **The seeded registry Connection is a seed, not a fixture.** It is created
  once and the fact is recorded in `status.registry.connection`; a Connection
  someone deletes stays deleted, because an installation that would rather use
  Harbor or GHCR has to be able to end up with only its own. It is still kept
  in step — URL and credential — for as long as it exists and carries
  `app.kubernetes.io/managed-by: kitchen`.
- **`PORT` is the platform's, and every environment gets it.** A
  buildpacks-built image starts whatever process the buildpack chose, and every
  buildpack's answer to which port that process listens on is `$PORT` — so an
  application that never had a Dockerfile has no other way to be told. It is
  injected ahead of the project's own variables, like the telemetry ones, so a
  project that sets `PORT` still wins.
- **An idling Deployment's replica count belongs to KEDA, not to the
  reconciler.** While an Environment is allowed to scale to zero,
  `applyDeployment` does not touch `spec.replicas` at all: the number it would
  write is the one the autoscaler has just moved, so writing it back would undo
  every scale decision — including the zero the feature exists for. The
  fallback path (idling off, or the add-on's API unavailable) sets it again,
  which is what brings a parked environment back.
- **Routing an idling environment means replacing the application's address,
  wherever it appears.** For an open environment that is the HTTPRoute's
  backend; for a *protected* preview the route still points at the forward-auth
  gate, and it is the gate's upstream header that has to name the interceptor
  instead — pointing the route at the interceptor there would take the gate out
  of the path and publish the preview. Both work because everything in front
  keeps the visitor's `Host` header, which is the only thing the interceptor
  routes on.
- **Anything that should appear in the component survey needs
  `app.kubernetes.io/part-of: kitchen`** and, to be readable,
  `app.kubernetes.io/component`. The survey selects on the former rather than on
  names, since every chart-generated name is release-name prefixed. Use
  `platformLabels()` for operator-created workloads — and note the selector name
  is passed separately, because a Deployment's selector is immutable and must
  keep whatever value it already had.
