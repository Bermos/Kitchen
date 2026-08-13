# Kitchen auth service

The platform's identity provider: one login for the Kitchen UI, the operator
API and — for projects that opt in — the apps Kitchen deploys. It is a small
Node service around [better-auth](https://better-auth.com) and is deployed by
the chart at `auth.<baseDomain>`. The architecture and the reasoning behind it
live in [docs/AUTH.md](../docs/AUTH.md).

What matters to everything else is the contract, not this implementation:
**an OIDC issuer that supports dynamic client registration.**

## What it serves

Everything is mounted at the root, so the issuer is the origin itself:

| Path | Purpose |
|---|---|
| `/.well-known/openid-configuration` | OIDC discovery document |
| `/jwks` | Signing keys; the operator API validates bearer tokens against these |
| `/oauth2/authorize`, `/oauth2/token`, `/oauth2/userinfo` | Authorization Code + PKCE |
| `/oauth2/register` | Dynamic client registration (authenticated) |
| `/login`, `/consent` | The hosted pages the provider redirects to |
| `/bootstrap` | First-administrator flow, see below |
| `/healthz`, `/readyz` | Probes; readiness additionally requires Postgres |

Plugins: OAuth/OIDC provider, SSO (upstream OIDC and SAML providers), social
login (GitHub), organizations, passkeys, two-factor and API keys.

## Accounts

Public sign-up is off. Accounts come from one of three places:

- the **bootstrap** flow, for the very first administrator,
- an upstream provider, when `KITCHEN_AUTH_ALLOW_SOCIAL_SIGNUP` is on,
- an invitation from an organization (the organizations plugin).

### Bootstrap

`helm install` generates a token into the release's auth Secret and prints the
link that carries it. `GET /bootstrap?token=…` serves a form; posting it
creates the first administrator. The endpoint then closes: it works only while
the installation has no account, so the link is one-time without any state of
its own. The operator's own service account does not count as an account.

### The Kitchen UI's OAuth client

The UI is the platform's own front end, so its client is seeded on start rather
than registered at runtime: the client id is a constant the UI is built with,
where a generated one would have to be discovered from somewhere. It is a
public client — a browser application keeps no secret — which makes PKCE
mandatory, and consent is skipped, because asking someone to authorise the
dashboard of the platform they just signed in to has one sensible answer.
Clients registered later, including apps that claim one, still get the consent
screen.

### The operator's service credential

The chart generates an API key into the same Secret and the service seeds it on
start, owned by a machine account. The operator sends it as `x-api-key` and can
then register OAuth clients — the mechanism behind `ResourceClaim` type
`oidcClient`. Rotating the value in the Secret and restarting replaces the key;
the previous one is dropped.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `KITCHEN_AUTH_BASE_URL` | yes | Public issuer URL, e.g. `https://auth.apps.example.com` |
| `BETTER_AUTH_SECRET` | yes | Signing secret (≥16 characters) |
| `DATABASE_URL` | yes | Postgres connection string |
| `PORT` | no | Listen port, default `8080` |
| `KITCHEN_AUTH_SERVICE_KEY` | no | Operator API key, exactly the key the operator sends (≥64 characters) |
| `KITCHEN_AUTH_SERVICE_ACCOUNT_EMAIL` | no | Machine account owning that key, default `operator@kitchen.local` |
| `KITCHEN_AUTH_BOOTSTRAP_TOKEN` | no | Token for the first-administrator link |
| `KITCHEN_AUTH_API_URL` | no | Public URL of the operator API, accepted as a token audience |
| `KITCHEN_AUTH_UI_CLIENT_ID` | no | Client id for the Kitchen UI, default `kitchen-ui` |
| `KITCHEN_AUTH_UI_REDIRECT_URIS` | no | The UI's redirect URIs, comma separated. Without them no UI client is seeded |
| `GITHUB_CLIENT_ID` / `GITHUB_CLIENT_SECRET` | no | Upstream GitHub OAuth app; set both or neither |
| `KITCHEN_AUTH_ALLOW_SOCIAL_SIGNUP` | no | Let an unknown GitHub account create an account, default `false` |
| `KITCHEN_AUTH_TRUSTED_ORIGINS` | no | Extra browser origins, comma separated |
| `KITCHEN_AUTH_DATABASE_WAIT_SECONDS` | no | How long to wait for Postgres on start, default `120` |
| `LOG_LEVEL` | no | `debug`, `info`, `warn`, `error` |

The schema is migrated on start, under a Postgres advisory lock so several
replicas can start at once.

## Development

```sh
npm install
npm run typecheck
npm test          # integration tests, needs a Postgres
npm start
```

The tests run against a real Postgres because better-auth generates the schema
from the plugin set; override the connection with
`KITCHEN_AUTH_TEST_DATABASE_URL` (default
`postgres://postgres@127.0.0.1:5433/kitchen_auth_test`). They drop and recreate
the `public` schema, so point them at a throwaway database.

Build the image from this directory:

```sh
docker build -t ghcr.io/bermos/kitchen-auth:dev .
```

## Notes on better-auth

- The OIDC provider is `@better-auth/oauth-provider`. It supersedes the
  `oidc-provider` plugin, which better-auth deprecated in 1.6 and drops in the
  next major; both implement the same OpenID Connect surface.
- `validAudiences` is an explicit, closed list: this issuer, plus the operator
  API when `KITCHEN_AUTH_API_URL` is set. Leaving it open is the
  audience-confusion problem GHSA-p2fr-6hmx-4528 describes, which is only fixed
  in the 1.7 prereleases; a two-entry allow-list is the same mitigation with
  the API able to be an audience of its own.
- Access tokens are opaque unless the client asks for one with a `resource`
  parameter, in which case they are JWTs signed with the JWKS. That is why the
  operator API's callers request `resource=<api url>`, and why a token minted
  from a session at `GET /token` — what an API key is exchanged for — carries
  the issuer as its audience.
