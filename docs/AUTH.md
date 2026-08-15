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
2. **Preview protection (forward-auth).** ✅ Shipped: previews are gated
   behind platform login Cloudflare-Access-style, and the app doesn't change
   at all. See [Preview protection](#preview-protection-forward-auth) below.

## Preview protection (forward-auth)

Preview URLs used to be public: anyone who guessed `shop-pr-42.apps.example.com`
saw unreleased work. Now a preview is only useful to someone signed in to the
platform, and the deployed application is not involved in that at all — it is
not told to authenticate anything, and it never sees the platform's session.

### Why an in-path proxy and not a Gateway filter

The Cloudflare-Access shape people picture is an *external authorization*
filter: the proxy asks an auth service about each request and forwards it if
the answer is yes. Envoy implements exactly that (`ext_authz`), but nothing in
Gateway API exposes it: the specification has core filters (header rewriting,
redirects, mirroring) plus `ExtensionRef` for implementation-specific ones, and
Cilium's Gateway API implementation ships no such extension — its "mutual
authentication" is workload-to-workload mTLS, a different thing entirely.
Reaching for `CiliumEnvoyConfig` to inject an `ext_authz` filter by hand would
tie Kitchen's routing to Cilium, which is precisely what
[choosing Gateway API](SCOPE.md) was meant to avoid.

So the gate sits **in** the request path rather than beside it. A protected
Environment's `HTTPRoute` keeps its hostname and its place on the shared
Gateway; only its backend changes, from the application's Service to the gate's.
Every Gateway API implementation can do that, so preview protection works the
same on Cilium, Envoy Gateway or Istio.

### The component

`kitchen-preview-gate` is a small Go reverse proxy — a second binary in the
operator's own image, so there is no third image to build or pin. The
**operator** deploys it, not the chart: it cannot start before an OAuth client
has been registered for it, and only the operator can register one (the chart
would be waiting on a reconcile it has no way to wait for). Same reasoning, and
the same shape, as the cloudflared tunnel.

The `Kitchen` singleton carries the switch:

```yaml
spec:
  auth:
    secretRef: { name: kitchen-auth }    # issuer + the operator's registration credential
    previewGate:
      enabled: true
      host: previews.apps.example.com    # defaults to previews.<baseDomain>
      sessionTTL: 8h
```

### How a request goes through it

```
  GET https://shop-pr-42.apps.example.com/orders          (no session)
        │
        ▼  Gateway → gate (backend of the preview's HTTPRoute,
        │             carrying X-Kitchen-Upstream: <app service>)
  302 → https://previews.<baseDomain>/_kitchen/gate/start?rd=<signed return URL>
        │
        ▼  the gate's own host: sets a short-lived flow cookie (state + PKCE verifier)
  302 → https://auth.<baseDomain>/oauth2/authorize?…  (platform login)
        │
        ▼  the visitor signs in — or is already signed in, and never sees a form
  302 → https://previews.<baseDomain>/_kitchen/gate/callback?code=…&state=…
        │
        ▼  code → ID token, over the back channel
  302 → https://shop-pr-42.apps.example.com/_kitchen/gate/session?token=<hand-off>
        │
        ▼  sets the session cookie for *that* hostname
  302 → https://shop-pr-42.apps.example.com/orders        (and now it proxies)
```

Three details carry most of the design:

- **One redirect URI, forever.** The login finishes on the platform's own
  `previews.<baseDomain>`, so the gate's OAuth client is registered once with
  one redirect URI. Previews appear and disappear all day without touching it.
  (The alternative — a redirect URI per preview host — would mean rewriting the
  client's redirect list on every pull request.)
- **Host-scoped sessions.** The cookie is set on the preview's own hostname with
  no `Domain` attribute, which is why the last hop exists: only a request to
  that host can set a cookie for it. A cookie scoped to `.<baseDomain>` would
  be sent to every application the platform hosts, handing each of them a
  platform session — so the gate does not use one, and strips its own cookie
  before proxying anyway.
- **The routing header is not trusted.** The Gateway sets `X-Kitchen-Upstream`
  with a `RequestHeaderModifier` filter, which overwrites whatever the client
  sent. The gate still checks it is an in-cluster Service address before
  forwarding, because a proxy that forwards wherever it is told is a way out of
  the cluster.

The application receives the request with `X-Kitchen-User` and
`X-Kitchen-User-Email` and nothing else new. `/_kitchen/gate/*` is reserved on
protected hostnames, and `/_kitchen/gate/signout` drops the session on one.

### Per-project, and fail-closed

Protection is a Project field, on by default:

```yaml
spec:
  previews:
    protected: true      # the default; production environments are never gated
```

If a Project asks for protection on a platform that runs no gate — no identity
provider, or `previewGate.enabled: false` — the Environment gets **no route at
all** rather than a public one. The workload still deploys; it is the URL that
is withheld, with the way out stated in the `PreviewProtected` condition
(`spec.previews.protected: false` serves previews openly, on purpose). Publishing
unreleased work to whoever guesses the URL is the one outcome the Project
explicitly did not ask for, so it is not the failure mode.

### What the operator keeps

| Object (in `kitchen-system`) | What it is |
|---|---|
| `kitchen-preview-gate` (Deployment, Service, HTTPRoute) | The gate and its own hostname |
| `kitchen-preview-gate-oidc` (Secret) | The registered OAuth client: issuer, client id and secret, callback |
| `kitchen-preview-gate` (Secret) | The key sessions are signed with — delete it to sign everyone out |
| `kitchen-preview-gate-<app namespace>` (ReferenceGrant) | Lets that namespace's routes point at the gate |

The client is registered through dynamic client registration with the service
credential from `<release>-auth`, so it is the same contract the rest of this
document rests on — a different issuer that supports DCR works without the
operator learning anything new. The stored Secret is the source of truth: a
client is registered again only when it is missing, or was registered for a
different issuer or callback.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| IdP integration | OIDC + dynamic client registration as the contract | Swappable implementation, standard protocol everywhere |
| Default IdP | better-auth + OAuth/OIDC provider plugin | Excellent DX; the provider plugin is its youngest part — accepted risk, mitigated by the contract above |
| Auth storage | Chart-managed single-node Postgres (external override) | OLTP; SQLite would cap replicas at 1; mirrors the ClickHouse pattern |
| App auth surface | `ResourceClaim` type `oidcClient` | Reuses the existing claim → binding-secret → env flow |
| API tokens for CI | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's credential; the operator stays stateless and revocation stays in one place |
| Dashboard sessions | Rotating refresh tokens (`offline_access`), one per browser in `localStorage` | Renewal that needs no redirect and no framing of the login page; rotation is what makes browser storage defensible |
| Preview protection | An in-path gate the routes pass through | Gateway API has no external-auth filter, and Cilium exposes none of Envoy's |
| Gate's OAuth client | One client, one redirect URI, registered by the operator | Previews come and go without touching the client |
| Sequencing | auth service → REST API behind it → forward-auth for previews → UI → app claims | The API should never exist without auth; the provider is the hard part, claims are known plumbing |

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
- **The access token carries the account's name and address**, following the
  granted scopes (`profile` → `name`, `email` → `email`). Neither the UI nor
  the operator calls `/oauth2/userinfo`, and the ID token stops at the UI's
  token exchange, so a token that named its account with `sub` alone would
  leave both showing an opaque id — in the account menu, and as the author of
  everything the API writes.
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

### Dashboard sessions (settled)

An access token is good for an hour. The dashboard used to answer the 401 that
follows by starting the sign-in again — no interaction while the identity
provider still had a session, but a full page load that dropped whatever the
tab was in the middle of, once per hour, forever.

It now **renews in the background** instead. The sign-in asks for
`offline_access`; the refresh token that comes back is traded for a new access
token a minute before the old one expires, and a 401 renews and retries the
request once before anyone is sent back to the login page. Both numbers are
set explicitly on the provider rather than left to its defaults, because
together they *are* the session: `accessTokenExpiresIn` an hour,
`refreshTokenExpiresIn` a week.

The alternative was silent re-authentication — `prompt=none` in a hidden
iframe against the provider's own session. Refresh tokens win on two counts.
The iframe would need the identity provider to permit being framed by the
dashboard, which is the one thing a login page should refuse to anyone; and it
would need the browser to send the provider's session cookie from inside that
frame, which holds only while the dashboard and the issuer are subdomains of a
single site. `auth.host` is configurable, so that is a property of one
deployment, not of the design.

Two consequences worth stating plainly, because both look like bugs from the
outside:

- **A refresh token is single-use.** The provider rotates on every renewal and
  tears down every token it issued for that account and client when a spent one
  is replayed (RFC 9700 §4.14). Two tabs renewing at the same moment would
  replay one and sign the account out of all of them, so the dashboard
  serialises renewal on a Web Lock and re-reads storage inside it: the tab that
  loses the race adopts the winner's token instead of spending its own.
- **The session lives in `localStorage`, deliberately.** It is one session per
  browser rather than one per tab: opening a second tab is no longer a second
  trip through the identity provider, and signing out of one signs out of all
  of them through the `storage` event. The cost is a refresh token that
  outlives the tab that fetched it, which is a longer-lived thing for an XSS in
  the dashboard to steal than the old per-tab access token was. What bounds it:
  rotation makes a stolen copy detectable the moment both are used, the
  dashboard revokes the token on sign-out rather than leaving it valid, and a
  week is the longest it is worth anything.

## Open items

- **Token shape for the operator API**: scopes and claims for teams and RBAC
  are deferred until the organizations plugin is wired up. Tokens carry their
  scopes today and the API records who asked for what, but nothing is enforced
  beyond the issuer vouching for the caller.
- **Sign-in pages**: the service ships its own minimal login and consent
  pages. The Vue UI takes them over when it lands.
