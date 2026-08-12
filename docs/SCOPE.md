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
| **Service mesh + ingress** | Ingress into apps + **traffic observability** | Mesh is for monitoring traffic only — no mTLS/policy enforcement goals. Implementation choice still open (see below) |
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

6. **Auth & tenancy.** The UI and API need login: OIDC (bring your own IdP) + local admin fallback, teams/projects, RBAC, API tokens for CI and the CLI.

7. **Rollbacks & revision history.** Deployments as immutable revisions (image digest + config snapshot) → instant rollback comes for free. Vercel-table-stakes.

8. **Runtime scaling.** Decide the story: plain Deployments + HPA (simple) vs. scale-to-zero via Knative/KEDA (serverless feel, big dependency). See open decisions.

9. **Operator state.** ✅ Decided: CRDs (etcd) are the source of truth for all config and management state — no extra database for the control plane. ClickHouse stays analytics-only. Anything that genuinely doesn't fit CRDs (sessions, tokens) gets a small embedded store, not a new dependency.

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

## Open decisions

1. **Mesh implementation.** With the goal narrowed to observability-only, the candidates are:
   - **Istio ambient**: no sidecars, per-node ztunnel gives L4 telemetry by default; optional waypoints add L7 metrics only where wanted. Doesn't replace the CNI — friendliest for BYO clusters. Pairs with its own ingress gateway. Likely default.
   - **Cilium + Hubble**: the best traffic-visibility story (eBPF flow logs, service map, L7 visibility) with zero proxies — but it must own the CNI, which is invasive on a bring-your-own cluster. Great *optional* mode for clusters already running Cilium.
   - **Linkerd**: lightest full mesh, excellent golden metrics, but sidecar-per-pod for what is here an observability-only goal.
2. **Buildpacks vs. nixpacks vs. Dockerfile-first** for zero-config builds.
3. **Scale-to-zero** (Knative/KEDA) in v1, or plain Deployments + HPA with scale-to-zero later.
4. **Neon plugin scope**: Neon is a managed cloud service — fine as a first-party plugin, but the plugin interface should be a generic "database provisioner" so CloudNativePG (fully self-hosted) can slot in.
5. **cert-manager optionality**: keep it always-installed (operator webhook certs + internal CA), with only the *public ACME issuer* part optional when cloudflared handles edge TLS.

---

## Rough phasing

- **MVP**: operator + CRDs, Helm chart, git webhook → BuildKit build → registry → Deployment + Gateway route, generated URLs on a wildcard domain, build/runtime logs in ClickHouse, minimal Vue UI, local-admin auth.
- **v1**: preview deployments, custom domains + cert-manager/cloudflared, Infisical integration, Neon + registry plugins as a proper plugin interface, metrics dashboards, rollbacks, OIDC + teams.
- **Later**: CLI, scale-to-zero, more plugins, notifications, cron jobs.
