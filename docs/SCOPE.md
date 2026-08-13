# Kitchen — Project Scope

> Self-hosted Vercel alternative for people who bring their own Kubernetes cluster.
> Ships as a single Helm chart. You point it at your cluster, connect a git repo, and push.

## One-liner

`git push` → build → deploy → URL, on your own cluster, with batteries included.

---

## Component inventory

### Core (the original list)

| Component | Role | Notes |
|---|---|---|
| **Operator (Go)** | Control plane: reconciles all Kitchen resources, exposes the public API | CRD-driven; the API the UI/CLI talk to |
| **Management UI (Vue)** | Dashboard: projects, deployments, logs, metrics, settings | Talks only to the operator API |
| **ClickHouse** | Storage for **all** telemetry: metrics, logs, traces, build logs, mesh flow data | Observability store — *not* the system of record (that's CRDs/etcd) |
| **Collectors** | Cluster + application monitoring | OTel Collector (or Vector) shipping into ClickHouse — logs, metrics, and traces, all of it |
| **Cilium (assumed present)** | Traffic observability (Hubble) + ingress (Gateway API) | Kitchen does **not** install Cilium — it assumes the cluster runs it as its CNI (missing-Cilium handling ignored for now). No separate ingress controller: Cilium's built-in Gateway API implementation is the ingress |
| **cloudflared** (optional) | Tunnel-based ingress, no public IP / LB needed | Configured via the operator |
| **cert-manager** (semi-optional) | TLS for public ingress | Still wanted even with cloudflared — the operator's admission webhooks need certs (see gaps) |
| **Infisical** | Secret management | Synced into app namespaces as k8s Secrets |
| **Connection plugins** | First-party: Docker registry (image storage), Neon (Postgres) | Plugin interface should be generic (see gaps) |

### What was missing

These aren't nice-to-haves — the first three are the product.

1. **Build system.** Nothing in the list actually *builds* anything. Needed:
   - In-cluster build execution: BuildKit (or Kaniko/buildah) build pods with a build queue managed by the operator.
   - Framework auto-detection for zero-config deploys: Cloud Native Buildpacks or nixpacks (Next.js, Nuxt, Vite, static, plain Dockerfile fallback).
   - Build caching: BuildKit cache exported to the connected registry, so rebuilds are fast.
   - Build isolation: **deprioritized.** Kitchen is fully open source and self-hosted by a team that trusts its own code — no multi-tenant SaaS threat model. Resource quotas per build are enough for now; hard isolation (gVisor/Kata, dedicated node pools) can come later if someone runs it for untrusted users.

2. **Git integration.** The `git push` in the one-liner: GitHub/GitLab/Gitea app + webhook receiver in the operator, commit-triggered builds, deploy status checks posted back on commits/PRs. Fits the connection-plugin model (git providers as a plugin family alongside registry/db).

3. **Preview deployments.** The killer Vercel feature: every PR gets an ephemeral environment with a unique URL, torn down on merge/close. Requires wildcard DNS + wildcard TLS (or cloudflared, which sidesteps both) — this constrains the ingress/domain design and should be in from day one, not retrofitted.

4. **Domains & DNS.** Custom domain attachment with validation, a wildcard base domain for generated URLs (`*.apps.example.com`), optionally ExternalDNS for clusters that manage their own zones.

5. **Log pipeline.** ClickHouse is listed for "monitoring" but Vercel's DX depends on *logs*: live build logs and runtime log streaming/search in the UI. Collectors ship container logs + build logs into ClickHouse; operator API exposes tail/search.

6. **Auth & tenancy.** ✅ Decided and shipped as the platform IdP: the chart runs better-auth at `auth.<baseDomain>` — OIDC issuer with dynamic client registration, upstream SSO/social login, organizations, passkeys, 2FA and API keys, on its own Postgres. See [AUTH.md](AUTH.md). Teams/RBAC and app-level claims build on it.

7. **Rollbacks & revision history.** Deployments as immutable revisions (image digest + config snapshot) → instant rollback comes for free. Vercel-table-stakes.

8. **Runtime scaling.** Decide the story: plain Deployments + HPA (simple) vs. scale-to-zero via Knative/KEDA (serverless feel, big dependency). See open decisions.

9. **Operator state.** ✅ Decided: CRDs (etcd) are the source of truth for all config and management state — no extra database for the control plane. ClickHouse stays analytics-only. The things that genuinely don't fit CRDs — accounts, sessions, OAuth clients — belong to the identity provider and live in its Postgres, not in the operator.

10. **Self-hosting hygiene.** Helm upgrade path incl. CRD migrations, backup/restore of platform state. NetworkPolicies between app namespaces are lower priority given the trusted-team model, but keep the platform namespace protected.

### Nice-to-haves (later)

- CLI (`kitchen deploy`, `kitchen logs`, `kitchen link`)
- Deploy notifications/webhooks (Slack, generic)
- Cron jobs / background workers per project
- Edge config / feature flags
- Additional plugins: S3-compatible object storage, Redis/Valkey, CloudNativePG as a self-hosted alternative to Neon

---

## Decisions made

- **Not a SaaS.** Fully open source, self-hosted by teams that trust their own code. No multi-tenant threat model → build isolation and inter-app NetworkPolicies are deprioritized.
- **Git integration is in.** Webhook-triggered builds, status checks back on commits — core to the product.
- **Preview deployments are in.** Designed for from day one.
- **CRDs are the system of record** for all operator config/management. ClickHouse is analytics-only.
- **ClickHouse gets everything analytical**: logs, metrics, traces, build logs, mesh traffic data — one store, one query surface for the UI.
- **Mesh purpose: traffic observability only.** No mTLS or policy-enforcement goals. The mesh exists so you can see per-service traffic (rates, errors, latency, who-talks-to-whom) for troubleshooting, without touching app code — and export it all to ClickHouse.
- **Mesh implementation: Cilium.** Assumed to already be the cluster's CNI (e.g. a Talos cluster configured with Cilium) — Kitchen does not install it, and handling its absence is out of scope for now. Hubble provides the traffic observability (eBPF flow logs, service map, L7 visibility), exported into ClickHouse.
- **Scale-to-zero via KEDA** (when it lands): KEDA's HTTP add-on parks an interceptor
  in front of idle apps, scales them to zero, and cold-starts on the next request —
  which makes open preview environments nearly free. Chosen over Knative because it
  only manages replica counts on our existing Deployments instead of taking over the
  serving model. Touches the Environment reconciler's routing (interceptor sits
  between Gateway and Service).
- **No separate ingress controller.** The operator programs **Gateway API** resources (`Gateway`/`HTTPRoute`), and Cilium's built-in Gateway API implementation (embedded Envoy) serves them. Gateway API is the abstraction, so another implementation (Envoy Gateway, Istio) could slot in later without touching the operator's routing logic. When cloudflared is enabled, the tunnel points at the Gateway service — cloudflared is the edge, but all traffic still flows through one routing layer with uniform telemetry.
  - Documented cluster prerequisites this implies: Cilium with `gatewayAPI.enabled=true` + kube-proxy replacement, Gateway API CRDs installed, and a way to give the Gateway a reachable address on bare metal — Cilium L2 announcements or BGP for a LoadBalancer IP, or skip that entirely by fronting with cloudflared.

## Open decisions

1. **Buildpacks vs. nixpacks vs. Dockerfile-first** for zero-config builds.
2. **Neon plugin scope**: Neon is a managed cloud service — fine as a first-party plugin, but the plugin interface should be a generic "database provisioner" so CloudNativePG (fully self-hosted) can slot in.
3. **cert-manager optionality**: keep it always-installed (operator webhook certs + internal CA), with only the *public ACME issuer* part optional when cloudflared handles edge TLS.

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
- **Later**: CLI, scale-to-zero, more plugins, notifications, cron jobs.
