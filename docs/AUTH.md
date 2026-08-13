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

The default implementation the chart ships is **better-auth** with its OAuth/
OIDC provider plugin (`@better-auth/oauth-provider`, the successor to the
`oidc-provider` plugin better-auth deprecated in 1.6). If it ever disappoints,
the implementation swaps out without touching the operator, the CRDs, or any
deployed app. This is the same capability-over-provider principle used for
Connections.

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
| Default IdP | better-auth + OAuth/OIDC provider plugin | Excellent DX; the provider plugin is its youngest part — accepted risk, mitigated by the contract above |
| Auth storage | Chart-managed single-node Postgres (external override) | OLTP; SQLite would cap replicas at 1; mirrors the ClickHouse pattern |
| App auth surface | `ResourceClaim` type `oidcClient` | Reuses the existing claim → binding-secret → env flow |
| API tokens for CI | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's credential; the operator stays stateless and revocation stays in one place |
| Sequencing | auth service → REST API behind it → UI → app claims → forward-auth | The API should never exist without auth; the provider is the hard part, claims are known plumbing |

## What the chart deploys today

The auth service lives in [`auth/`](../auth) and the chart runs it at
`auth.<baseDomain>`, next to a single-node Postgres StatefulSet with the same
conventions as ClickHouse (generated password in `<release>-postgres`, an
`external.host` override for clusters that already run Postgres). The
`Kitchen` singleton carries the platform's view of it:

```yaml
spec:
  auth:
    enabled: true
    host: auth.apps.example.com   # defaults to auth.<baseDomain>
```

Because better-auth is mounted at the root of that hostname, the issuer is the
origin: `https://auth.<baseDomain>/.well-known/openid-configuration`.

Two credentials are generated into the secret `<release>-auth` on install and
preserved across upgrades:

- `serviceKey` — the operator's API key. Sent as `x-api-key`, it authenticates
  dynamic client registration, which is how `ResourceClaim` type `oidcClient`
  will mint a client per app.
- `bootstrapToken` — the first-administrator link, below.

### Bootstrap (settled)

`helm install` prints a one-time link, `https://auth.<baseDomain>/bootstrap?token=…`.
It serves a form that creates the first administrator, and the endpoint closes
as soon as the installation has an account — the account *is* the state, so
nothing has to remember whether the token was used. Public sign-up stays off;
later accounts arrive through an upstream provider or an organization
invitation.

### The operator API (settled)

The REST API is served by the operator behind this issuer — it has never
existed without it. It validates bearer JWTs against the JWKS and keeps no
session; the issuer and the audiences it accepts both come off the `Kitchen`
object, so there is nothing to configure twice. The endpoints, the token flows
and the decisions behind them are in [API.md](API.md).

Two things fall out of the API being a **resource server of its own**:

- `validAudiences` on the provider is an explicit two-entry list — the issuer
  and `spec.api.externalURL` — so a client can ask for a token *for the API*
  (`resource=https://kitchen.<baseDomain>`) rather than one for everything.
  That is also what makes the access token a signed JWT: the provider issues
  opaque tokens when no resource is named.
- **CI tokens are the api-key plugin** (the open item below is closed). A key
  is a credential at the issuer, exchanged there for a short-lived JWT
  (`GET /token` with `x-api-key`); the operator only ever sees the JWT. Keeping
  key lookup at the identity provider is what keeps the operator's request path
  stateless, and revocation in one place.

The **Kitchen UI is the first OIDC client**, seeded on start with the client id
`kitchen-ui` and the redirect URI `<spec.api.externalURL>/auth/callback`. It is
public — a browser application keeps no secret — so PKCE is mandatory, and
consent is skipped for the platform's own dashboard. It is seeded rather than
dynamically registered because the UI is built with its client id; apps, whose
clients nobody has to know the id of, still get theirs through dynamic
registration.

## Open items

- **Token shape for the operator API**: scopes and claims for teams and RBAC
  are deferred until the organizations plugin is wired up. Tokens carry their
  scopes today and the API records who asked for what, but nothing is enforced
  beyond the issuer vouching for the caller.
- **Sign-in pages**: the service ships its own minimal login and consent
  pages. The Vue UI takes them over when it lands.
