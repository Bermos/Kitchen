# Kitchen auth service

The platform's identity provider: one login for the Kitchen UI, the operator
API and — for projects that opt in — the apps Kitchen deploys. It is a small
Node service around [better-auth](https://better-auth.com) and is deployed by
the chart at `auth.<baseDomain>`. The architecture and the reasoning behind it
live in [docs/AUTH.md](../docs/AUTH.md).

What matters to everything else is the contract, not this implementation:
**an OIDC issuer that supports dynamic client registration.**

## What it serves

Everything is mounted at the root, so the issuer is the origin itself. It is
served on **two listeners**: the public one below, which the chart publishes on
the shared Gateway, and a private one on `KITCHEN_AUTH_INTERNAL_PORT` that
serves the operator's `/kitchen` prefix and nothing else — see
[The operator's own prefix](#the-operators-own-prefix).

| Path | Purpose |
|---|---|
| `/.well-known/openid-configuration` | OIDC discovery document |
| `/jwks` | Signing keys; the operator API validates bearer tokens against these |
| `/oauth2/authorize`, `/oauth2/token`, `/oauth2/userinfo` | Authorization Code + PKCE |
| `/oauth2/register` | Dynamic client registration — the operator's service credential only, see below |
| `/login`, `/consent` | The hosted pages the provider redirects to |
| `/get-session`, `/list-accounts`, `/list-sessions` | What the dashboard's account screen reads |
| `/update-user`, `/change-password`, `/revoke-session` | What it writes — see [docs/AUTH.md](../docs/AUTH.md), "Managing an account" |
| `/bootstrap` | First-administrator flow, see below |
| `/kitchen/*` | **404 here.** Kitchen's own prefix is served on the private listener only — see below |
| `/healthz`, `/readyz` | Probes; readiness additionally requires Postgres. Both listeners answer them |

Plugins: OAuth/OIDC provider, SSO (upstream OIDC and SAML providers), social
login (GitHub), organizations, passkeys, two-factor and API keys.

## Accounts

Public sign-up is off, and an account comes from one of exactly two places:

- the **bootstrap** flow, for the very first administrator,
- an upstream provider, when `KITCHEN_AUTH_ALLOW_SOCIAL_SIGNUP` is on and a
  GitHub OAuth app is configured.

There is no third, whatever this file used to say. The organizations plugin
is mounted, but an organization invitation **cannot create an account**:
accepting one is behind the plugin's session middleware, so the invitee has to
be signed in already. On an installation with no upstream provider the
bootstrap account is therefore the only account there can be, and the platform
has no way to make another — creating an account and resetting a password both
need mail it cannot send. [docs/AUTH.md](../docs/AUTH.md) has the whole of what
that leaves missing, and how a locked-out password is reset by hand until it is
decided.

An account **manages itself** from the dashboard's `/account` screen, which
calls the endpoints in the table above with the session cookie this service
set: the display name, the password, and the browsers signed in as it. Those
calls come from the dashboard's origin, which is why `trustedOrigins` is
derived from the same list the CORS headers are (`src/config.ts`) rather than
naming this service alone.

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

It is the **only** key that may do that. `clientPrivileges` on the OAuth
provider admits the service account for every client action and refuses
everyone else, a signed-in administrator included, and `src/keyscope.ts` keeps
every other key to the two endpoints a CI credential needs. See "What a key may
do at the issuer" below.

## The operator's own prefix

`/kitchen/*` is Kitchen's, not better-auth's. It answers to the operator's
service credential and to nothing else — not to a signed-in administrator, and
not to a CI key, which is an ordinary account's credential. It exists because
the platform needs two things OpenID Connect has no answer to.

**It is served on the private listener alone** (`KITCHEN_AUTH_INTERNAL_PORT`,
8081 by default), and the public one answers `404` under the prefix, in the
words an issuer that never had it would use. The chart fronts the private port
with a second Service, `<release>-auth-internal`, that no HTTPRoute names —
because the auth route sends `PathPrefix /` of the issuer's hostname at the
public Service, and this prefix enumerates accounts, mints CI keys for any
project and rewrites an OAuth client's redirect list on the strength of one
header. The operator is told where to find it through `directoryURL` in the
`<release>-auth` secret.

The prefix is rate-limited per source address by
`KITCHEN_AUTH_INTERNAL_RATE_LIMIT` (requests per minute, `300` by default, `0`
for none), because it is mounted ahead of the better-auth catch-all and so
better-auth's own limiter never sees it. Over the limit is a `429` with
`Retry-After`; every refusal under the prefix names the address it came from.

| | |
|---|---|
| `GET /kitchen/accounts` | Every account that belongs to a **person**, or the one holding `?email=`. It is what the operator list is seeded from and what the dashboard's people picker resolves an address with |
| `GET /kitchen/keys?project=` | A project's CI keys: name, subject, address, prefix, created, last used. Never a key value |
| `POST /kitchen/keys` | Create a machine account and the one key it owns. The key value is in this response and in no other |
| `DELETE /kitchen/keys?project=&name=` | Revoke the key and delete the machine account, answering the subject so the caller can take the grant off the project |

### Machine accounts, and why keys need them

The api-key plugin runs with `enableSessionForAPIKeys`, which is easy to read
as "a key is an identity". It is not: the session the plugin mints for a key is
a session for the account the key's `referenceId` points at, so the `sub` in the
token a key is exchanged for at `/token` is **its owner's**. Granting "the key's
subject" a project role would therefore grant it to whoever created the key, on
their own account.

So every CI key gets an owner of its own — a machine account holding that one
key — and it is that account's `sub` the project grants a role to
([docs/AUTH.md](../docs/AUTH.md#machine-accounts)). The convention lives in
`src/identity.ts`:

- A machine account's address is `<project>.<key>@machines.kitchen.local`. Both
  halves are DNS labels, so the split is unambiguous, and the user table's
  unique address is what enforces one key per name per project. The `.local`
  domain is reserved (RFC 6762), so no real mailbox can collide with one.
- The address is **never verified**. An access entry naming an address is
  honoured only for a verified one, so a hand-written grant can never resolve
  to a machine account by address — the only way to grant a key anything is its
  `sub`.
- Neither a machine account nor the service account is a **person**. That line
  is drawn in one place, `isPerson`, and both `isBootstrapped` and the account
  directory ask it: an installation whose only accounts are credentials still
  has nobody who can sign in, and a people picker must not offer a robot.

Nothing outside this service parses the address to make a decision. The operator
is handed the `sub`; it reads the domain only to render a key's grant as a key
rather than as a stranger with an odd address.

### What a key may do at the issuer

The same reading of `enableSessionForAPIKeys` has a second half, and it is the
one that mattered: the session the plugin mints is a session on **every**
endpoint this service serves, not only on the one Kitchen sends a key to. A CI
key was therefore a signed-in administrator here — able to register OAuth
clients the operator's `/kitchen/clients` cannot see, and to mint further keys
for its own machine account, which `GET /kitchen/keys` cannot see either
because it lists one row per account.

`src/keyscope.ts` states the reach instead of inheriting it:

| Key | Reaches |
|---|---|
| A project's CI key | `GET /token` (the exchange CI and `kitchen login` make) and `/get-session`. `/kitchen/*` is answered ahead of better-auth and refuses it a 403 of its own |
| The operator's service credential | Everything |

Anything else is a 403 saying what a key is for. The two are told apart by the
presented value rather than by a lookup, because the chart seeds
`KITCHEN_AUTH_SERVICE_KEY` and the account that owns it as a pair — so the
guard costs a comparison and no query. A wider credential (issue #349) is a
third row in that table, not a second mechanism.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `KITCHEN_AUTH_BASE_URL` | yes | Public issuer URL, e.g. `https://auth.apps.example.com` |
| `BETTER_AUTH_SECRET` | yes | Signing secret (≥16 characters) |
| `DATABASE_URL` | yes | Postgres connection string |
| `PORT` | no | Listen port for the published listener, default `8080` |
| `KITCHEN_AUTH_INTERNAL_PORT` | no | Listen port for the private `/kitchen` listener, default `8081`. Must differ from `PORT` |
| `KITCHEN_AUTH_INTERNAL_RATE_LIMIT` | no | Requests into `/kitchen` per minute per source address, default `300`, `0` for none |
| `KITCHEN_AUTH_SERVICE_KEY` | no | Operator API key, exactly the key the operator sends (≥64 characters) |
| `KITCHEN_AUTH_SERVICE_ACCOUNT_EMAIL` | no | Machine account owning that key, default `operator@kitchen.local` |
| `KITCHEN_AUTH_BOOTSTRAP_TOKEN` | no | Token for the first-administrator link |
| `KITCHEN_AUTH_API_URL` | no | Public URL of the operator API, accepted as a token audience |
| `KITCHEN_AUTH_UI_CLIENT_ID` | no | Client id for the Kitchen UI, default `kitchen-ui`. The one client allowed to name a `resource` at the token endpoint |
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
