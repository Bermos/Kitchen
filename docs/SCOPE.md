# Kitchen — Project Scope

> Self-hosted Vercel alternative for people who bring their own Kubernetes cluster.
> Ships as a single Helm chart. You point it at your cluster, connect a git repo, and push.

## One-liner

`git push` → build → deploy → URL, on your own cluster, with batteries included.

---

## Who it is for

Two people. Most design arguments here resolve by asking which of them is in the room.

**The developer** thinks *project → branch → URL*, and should never need the words "namespace" or "Deployment" — abstracting the cluster away *is* the product, not a convenience laid over it. They want a URL, a preview link they can paste to someone who does not write code, logs when it breaks, a rollback when it breaks badly, an environment variable, and above all to know whether a failure is theirs or the platform's. Node pressure, cert-manager conditions and the state of the tunnel are not merely useless to them; they suggest the developer is unqualified to use the tool. This is why a capability that only works through `kubectl` is an unfinished capability: a feature is done when it has a REST route and a screen, not when its reconciler works.

**The operator** owns the cluster and the platform on it, is fluent in Kubernetes, and *wants* the objects. Theirs are the base domain, TLS, the ACME token, connections, the telemetry store, upgrades, and "why is nothing deploying" — a question that needs the platform to be legible from underneath, which is what `status.components`, the platform screens and the diagnostics surfaces are for. Their strongest motivation is not being a bottleneck: every developer question that reaches them is a product gap, and the fix is a surface the developer could have used rather than a wider grant.

They are **hats, not people.** In a single-person installation they are the same human ten minutes apart, so nothing may require two accounts, two logins or two browsers in order to be both — see [Who may do what](AUTH.md#who-may-do-what), where the operator role contains the developer one for exactly that reason.

A third identity is not a persona but needs a place in any model that grants powers: **CI**, which must be able to trigger a build for one project and must not be able to change the base domain.

---

## Component inventory

### Core (the original list)

| Component | Role | Notes |
|---|---|---|
| **Operator (Go)** | Control plane: reconciles all Kitchen resources, exposes the public API | CRD-driven; the API the UI/CLI talk to |
| **Management UI (Vue)** | Dashboard: projects, deployments, logs, metrics, settings | Talks only to the operator API |
| **`kitchen` CLI (Go)** | `link`, `deploy`, `logs`, `env`, `rollback` from a terminal | ✅ Shipped, in this repository so one tag versions it with the chart and both images. A client of the same REST API the dashboard uses, and built to be driven by a machine: `--json` everywhere, no prompt that has no flag, fixed exit codes, and `kitchen schema` publishing the whole surface. See [CLI.md](CLI.md) |
| **ClickHouse** | Storage for **all** telemetry: metrics, logs, traces, build logs, mesh flow data | Observability store — *not* the system of record (that's CRDs/etcd) |
| **Collectors** | Cluster + application monitoring | ✅ Settled and shipped, and there is one of them: an `otelcol-contrib` DaemonSet takes container logs, pod and node metrics and the OTLP applications export, and writes ClickHouse itself. What it cannot produce stays the operator's — Hubble flows, and the restarts, OOM kills, limits and replica counts that are facts about API objects. See the telemetry-pipeline row below |
| **Cilium (assumed present)** | Traffic observability (Hubble) + ingress (Gateway API) | Kitchen does **not** install Cilium — it assumes the cluster runs it as its CNI (missing-Cilium handling ignored for now). No separate ingress controller: Cilium's built-in Gateway API implementation is the ingress |
| **cloudflared** (optional) | Tunnel-based ingress, no public IP / LB needed | Configured via the operator |
| **cert-manager** (semi-optional) | TLS for public ingress | Still wanted even with cloudflared — the operator's admission webhooks need certs (see gaps) |
| **Infisical** | Secret management | Synced into app namespaces as k8s Secrets |
| **Connection plugins** | First-party: Docker registry (image storage), Neon (hosted Postgres), CloudNativePG (Postgres in your own cluster) | Plugin interface is generic — matched on capabilities, never on provider names |

### What was missing

These aren't nice-to-haves — the first three are the product.

1. **Build system.** Nothing in the list actually *builds* anything. Needed:
   - In-cluster build execution: BuildKit (or Kaniko/buildah) build pods with a build queue managed by the operator.
   - Framework auto-detection for zero-config deploys: Cloud Native Buildpacks or nixpacks (Next.js, Nuxt, Vite, static, plain Dockerfile fallback). ✅ Shipped, as Buildpacks beside the Dockerfile build rather than instead of it. `strategy: auto` reads the repository at the commit through the project's git Connection, records what it found in `Build.status.detectedFramework`, and fails with *"no Dockerfile and no framework detected"* rather than with a builder's error about a file the repository never had. The framework also supplies the runtime port when the project names none, and tells the buildpacks lifecycle how to serve a framework that builds to static files.
   - Build caching: BuildKit cache exported to the connected registry, so rebuilds are fast. ✅ Shipped, for both builders and with no infrastructure added: BuildKit exports a cache manifest to `<image repository>:buildcache` beside the image it just pushed, under the same credential, and imports it on the next build; the buildpacks lifecycle exports a cache image to `:buildcache-cnb`. `Kitchen.spec.builds.cache` sets the mode (`max` keeps intermediate layers, so a source change still reuses the dependency install above it) and the scope (one cache per project by default — one tag, overwritten in place, so it is bounded without anything pruning it — or one per branch). A registry that will not keep a cache manifest degrades rather than failing: the export is told to warn, and the next build notices the cache never landed and runs without one. Every build says on `status.cache`, on its commit status and in the dashboard whether it started warm or cold, because a build that was slow for having nothing to reuse should not read as a regression. A persistent BuildKit with a PVC would be faster still and was deliberately not taken: it makes the platform stateful, needs a garbage-collection policy of its own, and pins builds to one node.
   - Build isolation: **deprioritized.** Kitchen is fully open source and self-hosted by a team that trusts its own code — no multi-tenant SaaS threat model. Resource quotas per build are enough for now; hard isolation (gVisor/Kata, dedicated node pools) can come later if someone runs it for untrusted users.

2. **Git integration.** The `git push` in the one-liner: GitHub/GitLab/Gitea app + webhook receiver in the operator, commit-triggered builds, deploy status checks posted back on commits/PRs. Fits the connection-plugin model (git providers as a plugin family alongside registry/db). ✅ Shipped for GitHub, all three parts: webhook registration and receipt, builds from commits, and status back on the commit — a check per build, a deployment per environment, and one preview comment per PR rewritten in place. Reporting is a separate interface from being a source (`gitprovider.StatusReporter`), and GitLab and Gitea now do both (#72): they register webhooks, build from verified push and pull/merge-request deliveries, and post the check and the preview comment back. The deployment record is the one piece the forges do not agree on — GitHub and GitLab keep one, Gitea has no such API — so it is its own interface (`gitprovider.DeploymentPublisher`) and a forge without it loses nothing else.

3. **Preview deployments.** The killer Vercel feature: every PR gets an ephemeral environment with a unique URL, torn down on merge/close. Requires wildcard DNS + wildcard TLS (or cloudflared, which sidesteps the wildcard *certificate* but not the wildcard DNS) — this constrains the ingress/domain design and should be in from day one, not retrofitted. ✅ Shipped, and gated: preview URLs go through a forward-auth proxy so only signed-in platform users see unreleased work (`spec.previews.protected`, on by default).

4. **Domains & DNS.** Custom domain attachment with validation, a wildcard base domain for generated URLs (`*.apps.example.com`), optionally ExternalDNS for clusters that manage their own zones.

5. **Telemetry pipeline.** ✅ Shipped, all four kinds, into the one store under the one retention (`Kitchen.spec.observability.clickhouse.retentionDays`), whose schema the operator owns.

   Three of the four arrive by one route: an `otelcol-contrib` DaemonSet on every node tails the container log files, reads the kubelet and the node itself for usage, receives what applications export over OTLP, and writes ClickHouse directly with the stock `clickhouseexporter`. It replaced three separate paths — a log-shipping DaemonSet, an OTLP receiver inside the operator, and the operator polling every kubelet in the cluster — and the gain is not tidiness: one process per node means one `k8sattributes` lookup, so a log line, a span and a memory reading about the same container agree about which project and environment they belong to, because the same enrichment stamped all three.

   **The operator kept what no collector can produce.** No OTel receiver models a network flow, so flows are still followed off Hubble Relay and written by the operator. The activity feed is not collected at all — the controllers and the API write it as they act. And restarts, OOM kills, resource limits and replica counts are facts about API objects rather than about a running process, so the operator still samples those; it exports them to the collector over OTLP as six `kitchen.*` metrics instead of inserting rows, which makes it an ordinary client of the same endpoint every instrumented application is handed. The restart differencing stays on the operator's side for a reason worth keeping written down: a restart count is a lifetime counter, and a counter bucketed for a chart loses every transition that lands on a bucket boundary, so the change has to be taken where the previous sample is remembered.

   **The operator also kept the DDL and the TTLs**, for every table including the ones it never inserts into — the exporter runs with `create_schema: false` and creates nothing. The write path moved; schema ownership did not, and the reason is the ordering key. Logs, traces and metrics carry the exporter's own column set, so the store is readable by anything that knows the OTel ClickHouse schema, ClickStack and HyperDX included, with Kitchen's semantics added as `MATERIALIZED` columns over `ResourceAttributes` — `project`, `environment`, `build`, `source` and the `k8s.*` names — and the ordering key built on those. Upstream orders logs by `(toStartOfFiveMinutes(Timestamp), ServiceName, Timestamp)`; every query Kitchen makes is scoped to a project, so ours is `(project, environment, Timestamp)`. Standard shape, our sort order: that is the whole trade, and its price is a schema transcribed from one exporter version, which is why the collector's image tag is pinned rather than floating.

   The tables are `otel_logs`, `otel_traces` (with `otel_traces_trace_id_ts` and its materialized view, so a trace id that arrived out of a log line is a point read rather than a scan of the retention), the five `otel_metrics_*` tables, a `metrics_5m` rollup derived from two of them, and Kitchen's own `flows` and `events`. The pre-collector `logs`, `traces` and `metrics` tables are deliberately not migrated: they hold real history this schema has no reshaping for, they age out on their own TTL, and an operator who wants the disk back sooner can drop them by hand.

   **Node and system logs are deliberately not collected.** The stock contrib image is `FROM scratch`, and the journald receiver shells out to a `journalctl` that is not in it; a receiver that errors out of `Start()` aborts collector startup, so switching it on would take the log, metric and OTLP pipelines down with it. Talos, which Kitchen is built to run on, has no journald to read in the first place.

   - **Logs**: every container on the node, with `project`, `environment` and `build` lifted out of the Kitchen labels into columns, a JSON line's own fields flattened into the line's attributes so `http.status:500` is a map lookup, and its trace and span ids in columns of their own. On top sits log *analytics* rather than a log grep: one selection — Kitchen's query language, or raw ClickHouse as the escape hatch — asked four ways, as lines, as a histogram over the window, as facets with counts, and as message patterns, and saveable under a name.
   - **Flows**: the operator follows Hubble Relay and writes flow observations — the traffic view's service map. This is the pipeline that never moved, because a flow is not a signal OTLP has a shape for.
   - **Resource metrics**: `kubeletstats` gives CPU and memory per pod and container; the operator supplies the rest, and both halves land in the same tables under the same resource attributes. What was ruled out stays ruled out: a cAdvisor scrape is keyed by namespace/pod/container and cannot make the join back to the project and environment that own the pod, which lives in the pod's labels; and kube-state-metrics is a workload whose whole job is turning API objects into metrics for a system that can only read metrics, which the operator is not. A five-minute rollup behind materialised views answers the wide windows.
   - **Traces**: spans come from the applications themselves — nothing the platform sees from outside one is a substitute, and Hubble's L7 data reshaped into trace-like rows would answer none of the questions tracing exists for. What the platform does is remove every other obstacle: every environment is handed the collector's OTLP address through OTLP's own environment variables, so instrumenting an app is adding its language's SDK and nothing else. Nothing changed for an instrumented application when the receiver moved out of the operator — same Service name, same port — beyond the Service becoming `internalTrafficPolicy: Local`, so the name is stable but the agent it reaches is the one on the caller's own node, and an export from a node running no collector is dropped rather than sent elsewhere.

   The correlation between them is closed: a log line carries its trace id, so a line offers the request it came out of and a span offers its lines.

6. **Auth & tenancy.** ✅ Decided and shipped as the platform IdP: the chart runs better-auth at `auth.<baseDomain>` — OIDC issuer with dynamic client registration, upstream SSO/social login, organizations, passkeys, 2FA and API keys, on its own Postgres. See [AUTH.md](AUTH.md). The operator's REST API landed behind it (never in front of it): stateless JWT validation against the issuer's JWKS, the UI registered as the first client, CI keys exchanged for tokens at the issuer — see [API.md](API.md). Preview URLs are gated by it too: the operator registers an OAuth client for a forward-auth gate and routes protected previews through it. RBAC is decided and built: two platform roles and three project roles, membership held in Kitchen's own custom resources rather than in the IdP so the issuer contract stays at OIDC + DCR, and a token that carries identity and no permissions — enforced by one route → role table in the API, by the preview gate for protected previews, and followed by a dashboard whose copy of the table is generated from it. See [Who may do what](AUTH.md#who-may-do-what). App-level auth builds on the same IdP and has landed: a `ResourceClaim` of type `oidcClient` registers a client per project and the operator keeps its redirect list level with the project's environment URLs — see [App auth](AUTH.md#app-auth-a-claim-for-single-sign-on).

7. **Rollbacks & revision history.** Deployments as immutable revisions (image digest + config snapshot) → instant rollback comes for free. Vercel-table-stakes.

8. **Runtime scaling.** ✅ Decided and shipped: plain Deployments by default, with optional scale-to-zero through KEDA's HTTP add-on rather than Knative — replica counts on the Deployments the operator already creates, not a second serving model. An idle preview drops to no pods and cold-starts on the next request; production stays on its replica count unless a Project opts in. Scaling *up* under load beyond that ceiling is still to come: an HTTP-driven autoscaler exists for the environments that idle, and nothing yet scales one that never does.

9. **Operator state.** ✅ Decided: CRDs (etcd) are the source of truth for all config and management state — no extra database for the control plane. ClickHouse stays analytics-only. The things that genuinely don't fit CRDs — accounts, sessions, OAuth clients — belong to the identity provider and live in its Postgres, not in the operator.

10. **Self-hosting hygiene.** Helm upgrade path incl. CRD migrations, backup/restore of platform state. The upgrade path itself is shipped, and optionally self-serve: `selfUpdate.enabled` grants a Job cluster-admin and the platform upgrades its own release from the dashboard (`PlatformUpdate`). Off by default — the grant is real — and it runs helm in a Job rather than in the operator, which does not survive applying its own new Deployment. ✅ Backup/restore is shipped: one archive carrying the CRs, every Secret in `kitchen-system` and a data dump of the identity provider's Postgres, taken from the dashboard and put back by a Job the chart renders — a Job rather than a screen because a restore happens into a cluster whose accounts are gone, so there is nobody left to log in. Telemetry is explicitly *not* in it and is not expected to survive; PVC snapshots are offered where the cluster actually has a snapshot class, checked rather than assumed (#64). CRD migrations are still the open half — no conversion webhook exists, the API is `v1alpha1`, and an archive is bound to the release that wrote it. See [BACKUP.md](BACKUP.md). NetworkPolicies between app namespaces are lower priority given the trusted-team model, but keep the platform namespace protected.

11. **Processes beyond the web one.** ✅ Decided and shipped: a Project declares
   workers and scheduled jobs alongside its web process, and they are the same
   Release — the same image and the same environment, started with another
   command — so they are a modelling change rather than a build one. A worker
   is a Deployment with no Service and no route; a scheduled job is a
   `batch/v1` CronJob and one firing is a run. The list is snapshotted into
   every Release, so a rollback runs the processes that release declared. The
   two decisions worth having written down are that a preview runs none of them
   unless a process opts in (a preview that emails customers nightly is a bad
   afternoon), and that a failed run is carried out of the cluster rather than
   left in `kubectl get jobs`: into the activity feed, onto the environment's
   status, and onto the environment screen. The log pipeline keys on the
   process and the run, so one firing's output is one query for as long as the
   lines are kept — which outlasts the Job. See
   [Workers and scheduled jobs](api/processes.md).

### Nice-to-haves (later)

- Deploy notifications/webhooks (Slack, generic)
- Edge config / feature flags
- Additional plugins: S3-compatible object storage, Redis/Valkey. (CloudNativePG shipped — see decision 2.)

---

## Decisions made

- **Not a SaaS.** Fully open source, self-hosted by teams that trust their own code. No multi-tenant threat model → build isolation and inter-app NetworkPolicies are deprioritized.
- **Git integration is in.** Webhook-triggered builds, status checks back on commits — core to the product.
- **Preview deployments are in.** Designed for from day one.
- **CRDs are the system of record** for all operator config/management. ClickHouse is analytics-only.
- **ClickHouse gets everything analytical**: logs, metrics, traces, build logs, mesh traffic data — one store, one query surface for the UI.
- **Mesh purpose: traffic observability only.** No mTLS or policy-enforcement goals. The mesh exists so you can see per-service traffic (rates, errors, latency, who-talks-to-whom) for troubleshooting, without touching app code — and export it all to ClickHouse.
- **Mesh implementation: Cilium.** Assumed to already be the cluster's CNI (e.g. a Talos cluster configured with Cilium) — Kitchen does not install it, and handling its absence is out of scope for now. Hubble provides the traffic observability (eBPF flow logs, service map, L7 visibility), exported into ClickHouse.
- **Scale-to-zero via KEDA.** ✅ Shipped, off by default. KEDA's HTTP add-on parks an
  interceptor in front of idle apps, scales them to zero, and cold-starts on the next
  request — which makes open preview environments nearly free. Chosen over Knative
  because it only manages replica counts on our existing Deployments instead of taking
  over the serving model. The operator writes one `HTTPScaledObject` per idling
  environment and addresses the application through the interceptor instead of directly
  — as the Gateway's backend where the environment is open, as the preview gate's
  upstream where it is protected. Which environments idle is each Project's own
  `spec.scaleToZero`: previews by default, production only when asked. Turning it on
  costs the first visitor a cold start, which is why the interceptor's readiness
  timeout is a documented value rather than a hidden one. An idle environment
  stops doing *everything*, not only serving — there are no pods, so a background
  loop stops with it — which is why a project whose workload is not request-driven
  says so in `spec.runtime.notRequestDriven` and keeps its pods everywhere,
  previews included.
  - **KEDA is the one platform dependency Kitchen's *chart* does not bundle**, against
    the usual rule. The HTTP add-on ships a `ScaledObject` of KEDA's own CRD, and Helm
    builds and validates a release's entire manifest before applying any of it, so a
    chart containing both never installs — nor does a `pre-install` hook (built after the
    main manifest) or a `crds/` directory (never applied on upgrade), and a bundled copy
    collides with a cluster that already runs KEDA, since Helm will not adopt CRDs
    another release owns. Two releases, as upstream ships them.
  - **The operator installs them anyway** (`scaleToZero.install`), ✅ shipped — because
    every constraint above is Helm's and none of them is Kubernetes'. A controller
    installs one release, waits for its CRDs, and installs the next, which is the same
    reason the cert-manager `ClusterIssuer` and the wildcard `Certificate` are the
    operator's rather than the chart's. It runs as a job under an account the chart
    creates only when asked, bound to cluster-admin because installing KEDA applies CRDs
    and ClusterRoles — the `selfUpdate` shape, for the same reason. The two chart
    versions are pinned as a pair rather than floated, and an operator upgrade carries
    the dependency forward with it.
    - **It never writes to a release it did not create.** A cluster already serving the
      add-on's API is recorded (`status.scaleToZero.managed: false`) and left alone for
      good; KEDA present without its add-on is refused with a message rather than
      installed over. An installation that would rather run its own KEDA has to be able
      to, and the seed-not-a-fixture rule that governs the seeded registry Connection
      governs this too.
    - Without either, `scaleToZero.enabled` carries only the interceptor's address, and
      an environment on a platform that lacks the add-on stays on plain Deployment
      routing and says so.
- **Preview protection is an in-path proxy, not a Gateway filter.** Gateway API has no external-authorization filter and Cilium's implementation exposes none of Envoy's `ext_authz`; injecting one through `CiliumEnvoyConfig` would tie routing to Cilium. Instead a protected preview's `HTTPRoute` simply points at the gate, with the application's address in a header the Gateway sets — something every Gateway API implementation can do. See [AUTH.md](AUTH.md).
- **No separate ingress controller.** The operator programs **Gateway API** resources (`Gateway`/`HTTPRoute`), and Cilium's built-in Gateway API implementation (embedded Envoy) serves them. Gateway API is the abstraction, so another implementation (Envoy Gateway, Istio) could slot in later without touching the operator's routing logic. When cloudflared is enabled, the tunnel points at the Gateway service — cloudflared is the edge, but all traffic still flows through one routing layer with uniform telemetry.
  - Documented cluster prerequisites this implies: Cilium with `gatewayAPI.enabled=true` + kube-proxy replacement, Gateway API CRDs at the version Cilium pins, a default StorageClass (ClickHouse and the auth Postgres take the cluster default and stay `Pending` without one), and a LoadBalancer address for the Gateway.
  - **cloudflared does not remove the address requirement**, only the routability requirement. Verified on bare metal: Cilium reports `Programmed=False` / `AddressNotAssigned` on a Gateway with no LoadBalancer IP, and `observeGateway` mirrors that into the Kitchen object, so the platform never goes ready. LB IPAM is needed either way; L2 announcements or BGP are what cloudflared lets you skip, since the tunnel dials out from inside the cluster.

## Open decisions

1. ~~**Buildpacks vs. nixpacks vs. Dockerfile-first**~~ ✅ Decided: **Cloud Native Buildpacks**, as a second strategy beside the Dockerfile one rather than instead of it. Buildpacks are a specification with several implementations rather than one vendor's tool, the Paketo builders already cover the languages a project here is likely to be written in, and an image the lifecycle built can be rebased onto a patched base image without rebuilding — which is the answer to a base-image CVE across every project at once. The cost is a lifecycle and a builder image to learn when a build goes wrong, and it is paid only by projects that choose it: a repository with a Dockerfile keeps building the way it did.
   - The builder image is pinned (`BuildpacksBuilderImage`), not floating: it decides what the image contains, so a moving tag would mean the same commit rebuilt tomorrow produces something else.
   - Detection then picks between the two, and ships with them (issue #69): `strategy: auto` reads the repository through the project's git Connection, a Dockerfile wins over every other signal, and everything the platform recognises otherwise goes to buildpacks. The framework it decides on is recorded on the Build, supplies the runtime port where the project names none, and configures the web-server buildpack for the frameworks that build to a directory of files instead of a server.
2. ~~**Neon plugin scope**~~ ✅ Decided and now shipped twice over: the interface is a generic database provisioner (`internal/provider/database`: Provision/Deprovision plus branch operations against opaque IDs), and both consumers it was shaped against exist — Neon for a hosted Postgres, and CloudNativePG for one in the cluster Kitchen was installed into, so that a database needs no SaaS account at all (#238). The shape held: the second provider is a new value in the Connection enum and a new implementation, and the claim, the binding Secret and the environment wiring did not move. Three things it taught the interface, all of them honest rather than incidental. **A claim can ask for capabilities** — a Postgres major version and the extensions its first migration will call for — which are resolved to an image before anything is created, so a claim that cannot be satisfied fails *as a claim*, with a message, rather than as a `CREATE EXTENSION` crash loop later. **A database the platform runs itself takes minutes to come up**, where a SaaS API answers in one request, so "not ready yet" is a state the interface distinguishes from "refused". And **previews are empty**: cnpg has no copy-on-write branch, its nearest equivalent is a `pg_basebackup` of production, so a preview gets a fresh database and declares `dataProvenance: synthetic` — which keeps production data out of previews by construction rather than by policy. Whether the operator installs CloudNativePG is the KEDA question again and has the KEDA answer: off by default, a cluster-admin job under an account the chart only creates when asked, a pinned chart version, and a refusal to take over an installation somebody else owns.
3. ~~**cert-manager optionality**~~ ✅ Decided: cert-manager ships *with* the platform, as a sub-chart (`cert-manager.enabled`, on by default). Kitchen owns the cluster it is installed into, so the usual objection to bundling — a cluster-scoped singleton colliding with an existing installation — does not apply, and `cert-manager.enabled=false` covers the cluster that already has one. Bundling is safe specifically because cert-manager ships its CRDs as ordinary templates rather than `crds/` files, so `helm upgrade` applies schema changes; that is the same reason Kitchen ships its own CRDs as templates. The reasoning that it was needed for *operator webhook certs* no longer applies — the chart has no admission webhooks — so its one job is edge TLS.
   - The `ClusterIssuer` and the wildcard `Certificate` are created by the **operator**, not the chart, from `spec.tls.acme`. cert-manager's webhook admits both, so on a first install neither can exist until it is serving: a reconcile loop waits and retries where a Helm release would simply fail. Progress is the `CertificateReady` condition.
   - The solver is DNS-01 only. Every generated URL is a subdomain of the base domain, so the platform needs a wildcard, and ACME issues wildcards over DNS-01 alone — inbound reachability does not change that. Cloudflare is the first provider modelled.
   - `tls.mode: acme` without a `tls.acme` block, and a `tls.acme` block naming no solver, are both refused at **admission**, by CEL rules on the CRD. The reconcile-time conditions stay for what admission cannot see — cert-manager not being installed, an order that fails — but a platform that could never get a certificate is a rejected `kubectl apply` rather than a `.status` nobody reads. The Kitchen object is meant to be edited from the UI, which is the case that rules out relying on the chart's own guards.

## The future corner

Ideas we like, deliberately parked — with the reasoning preserved so future-us doesn't
have to rediscover it:

- **Kargo-style promotion pipelines.** Kargo (Akuity) models artifacts ("Freight" ≈ our
  Release) flowing through Stages (≈ our Environments) with verification gates and
  automatic promotion policies. Our one-field promotion is deliberately simpler and we
  keep it — but multi-stage pipelines (dev → staging → prod), pre-promotion validation
  (smoke tests, SLO checks against ClickHouse data), and auto-promotion-on-green are the
  natural SRE-grade evolution of our Release/Environment model. Steal the model, or
  integrate Kargo outright, when the demand shows up.

---

## Rough phasing

- **MVP**: operator + CRDs, Helm chart, git webhook → BuildKit build → registry → Deployment + Gateway route, generated URLs on a wildcard domain, build/runtime logs in ClickHouse, minimal Vue UI, local-admin auth.
- **v1**: preview deployments, custom domains + cert-manager/cloudflared, Infisical integration, Neon + registry plugins as a proper plugin interface, metrics dashboards, rollbacks, OIDC + teams.
- **Later**: more plugins, notifications. The CLI landed here ([CLI.md](CLI.md)) — it needed no platform surface of its own, being a client of an API that already existed. So did cron jobs and background workers, which needed one field on the Project and one on the Release rather than a platform surface of their own ([processes](api/processes.md)).
