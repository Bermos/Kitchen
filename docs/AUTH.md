# Kitchen — Auth Architecture

Central SSO for the platform *and* for the applications it deploys, served from
`auth.<baseDomain>`. One login for the Kitchen UI, the operator API, and — for
projects that opt in — every app running on the platform.

## The contract (the load-bearing decision)

Kitchen integrates with **"an OIDC issuer that supports dynamic client
registration"** — not with a specific auth product. Everything below speaks
standard OpenID Connect:

- the Vue UI is an OIDC client (Authorization Code + PKCE),
- the operator API validates JWTs statelessly against the issuer's JWKS,
- deployed apps become OIDC clients provisioned by the operator.

The default implementation the chart ships is **better-auth** with its OIDC
Provider plugin. If it ever disappoints, the implementation swaps out without
touching the operator, the CRDs, or any deployed app. This is the same
capability-over-provider principle used for Connections.

## Components

```
                      ┌─────────────────────────────┐
   dev browser ──────▶│  auth.<baseDomain>          │◀────── app users
                      │  better-auth (Node)         │
                      │  · OIDC Provider plugin     │
                      │  · social/SSO login (GitHub)│──▶ Postgres
                      │  · organizations, passkeys, │    (chart-managed or
                      │    2FA, API keys            │     external)
                      └──────────┬──────────────────┘
                 OIDC (JWKS)     │     dynamic client registration
        ┌────────────────────────┼──────────────────────┐
        ▼                        ▼                      ▼
  Kitchen Vue UI          operator REST API      deployed apps
  (PKCE client)           (JWT middleware)       (clients via ResourceClaim)
```

- **Auth service**: a small Node service running better-auth, deployed by the
  Helm chart at `auth.<baseDomain>` (HTTPRoute on the shared Gateway, same
  pattern as the webhook receiver). Plugins: OIDC Provider (the piece that
  makes it an IdP), SSO/social login upstream — logging into Kitchen with the
  same GitHub account you push from — organizations (future teams/RBAC),
  passkeys, 2FA, API keys.
- **Storage**: Postgres. Platform-critical OLTP — ClickHouse and CRDs are both
  wrong for it. The chart ships a single-node Postgres StatefulSet (same
  pattern and secret conventions as ClickHouse) with an `external.host`
  override for clusters that already run Postgres. SQLite-on-PVC was rejected:
  it caps the auth service at one replica forever.
- **Kitchen UI/API**: the first OIDC client. The Go API verifies bearer JWTs
  via the issuer's JWKS endpoint — no session state in the operator.
- **Apps**: opt in via a `ResourceClaim` of type `oidcClient`. The operator
  registers a client with the auth service (dynamic client registration) and
  writes the binding secret (`OIDC_ISSUER`, `CLIENT_ID`, `CLIENT_SECRET`),
  which reaches the app through the existing `fromResourceClaim` env
  mechanism. Same mental model as a database claim, zero new concepts.

## What falls out of this

1. **Redirect URI automation.** The operator owns every environment's URL, so
   it keeps each OIDC client's redirect list in sync as previews come and go —
   the OAuth chore nobody wants to do by hand, automated by the component that
   owns the URLs.
2. **Preview protection (forward-auth).** Preview URLs are currently public.
   With a central IdP, a forward-auth middleware at the Gateway can gate
   previews behind platform login Cloudflare-Access-style — the app doesn't
   change at all. Planned as a follow-up, not part of the first cut.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| IdP integration | OIDC + dynamic client registration as the contract | Swappable implementation, standard protocol everywhere |
| Default IdP | better-auth + OIDC Provider plugin | Excellent DX; the provider plugin is its youngest part — accepted risk, mitigated by the contract above |
| Auth storage | Chart-managed single-node Postgres (external override) | OLTP; SQLite would cap replicas at 1; mirrors the ClickHouse pattern |
| App auth surface | `ResourceClaim` type `oidcClient` | Reuses the existing claim → binding-secret → env flow |
| Sequencing | auth service → REST API behind it → UI → app claims → forward-auth | The API should never exist without auth; the provider is the hard part, claims are known plumbing |

## Open items

- **Bootstrap**: first admin user on `helm install` — likely a post-install
  hook printing a one-time signup link. To be settled when the service lands.
- **CI/API tokens**: better-auth's api-key plugin vs. platform-issued tokens.
  Leaning api-key plugin so all credentials live in one place.
- **Token shape for the operator API**: scopes/claims for teams and RBAC are
  deferred until the organizations plugin is wired up.
