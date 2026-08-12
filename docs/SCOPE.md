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
| **ClickHouse** | Storage for metrics, logs, traces, build logs | Observability store — *not* the system of record |
| **Collectors** | Cluster + application monitoring | OTel Collector (or Vector) shipping into ClickHouse — must ship **logs** too, not just metrics (see gaps) |
| **Service mesh + ingress** | Traffic routing, mTLS, ingress into apps | Choice is an open decision (see below) |
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
   - Build isolation: builds run arbitrary user code — dedicated node pool and/or gVisor/Kata, resource quotas per build.

2. **Git integration.** The `git push` in the one-liner: GitHub/GitLab/Gitea app + webhook receiver in the operator, commit-triggered builds, deploy status checks posted back on commits/PRs. Fits the connection-plugin model (git providers as a plugin family alongside registry/db).

3. **Preview deployments.** The killer Vercel feature: every PR gets an ephemeral environment with a unique URL, torn down on merge/close. Requires wildcard DNS + wildcard TLS (or cloudflared, which sidesteps both) — this constrains the ingress/domain design and should be in from day one, not retrofitted.

4. **Domains & DNS.** Custom domain attachment with validation, a wildcard base domain for generated URLs (`*.apps.example.com`), optionally ExternalDNS for clusters that manage their own zones.

5. **Log pipeline.** ClickHouse is listed for "monitoring" but Vercel's DX depends on *logs*: live build logs and runtime log streaming/search in the UI. Collectors ship container logs + build logs into ClickHouse; operator API exposes tail/search.

6. **Auth & tenancy.** The UI and API need login: OIDC (bring your own IdP) + local admin fallback, teams/projects, RBAC, API tokens for CI and the CLI.

7. **Rollbacks & revision history.** Deployments as immutable revisions (image digest + config snapshot) → instant rollback comes for free. Vercel-table-stakes.

8. **Runtime scaling.** Decide the story: plain Deployments + HPA (simple) vs. scale-to-zero via Knative/KEDA (serverless feel, big dependency). See open decisions.

9. **Operator state.** Source of truth should be CRDs (etcd) — no extra database for the control plane. ClickHouse stays analytics-only. Anything that doesn't fit CRDs well (users/sessions/tokens) may need a small embedded store — decide early.

10. **Self-hosting hygiene.** Helm upgrade path incl. CRD migrations, backup/restore of platform state, NetworkPolicies isolating tenant apps from each other and from the platform namespace.

### Nice-to-haves (later)

- CLI (`kitchen deploy`, `kitchen logs`, `kitchen link`)
- Deploy notifications/webhooks (Slack, generic)
- Cron jobs / background workers per project
- Edge config / feature flags
- Additional plugins: S3-compatible object storage, Redis/Valkey, CloudNativePG as a self-hosted alternative to Neon

---

## Open decisions

1. **Mesh choice — or no mesh at all.** "Service mesh with ingress" is the heaviest line in the list. Options:
   - **Cilium**: CNI + network policy + Hubble observability + Gateway API ingress + optional mesh — one dependency, but assumes it can own the CNI (invasive on a BYO cluster).
   - **Istio (ambient)**: full mesh + ingress, well-trodden, heavy.
   - **No mesh**: Envoy Gateway (Gateway API) for ingress only. Honest question: what does Kitchen actually need a *mesh* for? mTLS between tenant apps is nice; Gateway API + NetworkPolicies may cover 90% at a fraction of the weight.
2. **Buildpacks vs. nixpacks vs. Dockerfile-first** for zero-config builds.
3. **Scale-to-zero** (Knative/KEDA) in v1, or plain Deployments + HPA with scale-to-zero later.
4. **Neon plugin scope**: Neon is a managed cloud service — fine as a first-party plugin, but the plugin interface should be a generic "database provisioner" so CloudNativePG (fully self-hosted) can slot in.
5. **cert-manager optionality**: keep it always-installed (operator webhook certs + internal CA), with only the *public ACME issuer* part optional when cloudflared handles edge TLS.

---

## Rough phasing

- **MVP**: operator + CRDs, Helm chart, git webhook → BuildKit build → registry → Deployment + Gateway route, generated URLs on a wildcard domain, build/runtime logs in ClickHouse, minimal Vue UI, local-admin auth.
- **v1**: preview deployments, custom domains + cert-manager/cloudflared, Infisical integration, Neon + registry plugins as a proper plugin interface, metrics dashboards, rollbacks, OIDC + teams.
- **Later**: CLI, scale-to-zero, more plugins, notifications, cron jobs.
