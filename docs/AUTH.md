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
- **Apps**: ✅ Shipped. Opt in via a `ResourceClaim` of type `oidcClient`. The
  operator registers a client with the auth service (dynamic client
  registration) and writes the binding secret (`OIDC_ISSUER`, `CLIENT_ID`,
  `CLIENT_SECRET`), which reaches the app through the existing
  `fromResourceClaim` env mechanism. Same mental model as a database claim,
  zero new concepts. See [App auth](#app-auth-a-claim-for-single-sign-on).

## What falls out of this

1. **Redirect URI automation.** ✅ Shipped with the claim above: the operator
   owns every environment's URL, so it keeps each OIDC client's redirect list
   in sync as previews come and go — the OAuth chore nobody wants to do by
   hand, automated by the component that owns the URLs.
2. **Preview protection (forward-auth).** ✅ Shipped: previews are gated
   behind platform login Cloudflare-Access-style, and the app doesn't change
   at all. See [Preview protection](#preview-protection-forward-auth) below.

## App auth: a claim for single sign-on

A project asks for one thing and gets sign-in on every environment it has:

```yaml
apiVersion: kitchen.bermos.dev/v1alpha1
kind: ResourceClaim
metadata:
  name: shop-auth
spec:
  projectRef: { name: my-shop }
  type: oidcClient
```

and reads it the way it reads a database:

```yaml
# Project.spec.env
- name: OIDC_ISSUER
  fromResourceClaim: { name: shop-auth, key: OIDC_ISSUER }
- name: OIDC_CLIENT_ID
  fromResourceClaim: { name: shop-auth, key: CLIENT_ID }
- name: OIDC_CLIENT_SECRET
  fromResourceClaim: { name: shop-auth, key: CLIENT_SECRET }
```

There is **no `connectionRef`**, and it is refused rather than ignored: every
other claim type names the Connection that provisions it, and this one's
provider is the identity provider the platform is already configured with. The
operator registers the client with the `serviceKey` credential from
`<release>-auth` — dynamic client registration, the contract this whole
document rests on, so a federated issuer that supports it works without the
operator learning anything new.

The application also gets **`KITCHEN_URL`**, the address this environment is
published at, injected alongside `PORT` for every environment. It is the other
half of what an OIDC client needs and the half the application cannot work out
for itself: a preview's hostname carries a pull request number nothing in the
repository has heard of.

### The redirect list is the point

An OAuth client only accepts the callback URLs it was registered with, and a
preview's URL does not exist until somebody opens a pull request. So the
operator maintains the list, out of what it already knows:

| Source | Why it is in the list |
|---|---|
| The **production URL**, computed from the project name and the base domain | Knowable before the project has ever been deployed — which it has to be, since the first deployment is waiting on this claim's binding |
| Every **Environment's `status.url`** | The only thing that knows a preview's URL is the preview |
| Every **verified custom `Domain`** pointing at one of them | Production sign-in happens at the address the visitor typed. An unverified one is left out: a callback on a hostname nobody has proven they own is the one entry here worth having wrong |
| The claim's own **`config.redirectURIs`** | The addresses the platform does not own — a developer's `http://localhost:3000/auth/callback` |

Each of the first three is crossed with `config.callbackPaths`, which defaults
to `/auth/callback` and `/api/auth/callback/kitchen` — the plain convention,
and what Auth.js builds for a provider called `kitchen`. Registering a path the
application does not serve costs nothing: every generated URI is on the
application's own origin, so the worst an unused one can do is land a code on a
page that does not exist.

The result is sorted and compared against `status.redirectURIs`, so a reconcile
that agrees with the world sends the issuer nothing, and **a redirect list
changing never costs the application a new client id**.

### What the operator keeps, and what it takes away

| Object | Where | What it is |
|---|---|---|
| `<claim>-oidc-client` (Secret) | `kitchen-system` | The operator's record: issuer, client id and secret, and the management handle. Never in the application's namespace |
| `<claim>-binding` (Secret) | `kitchen-<project>` | What the application reads: `OIDC_ISSUER`, `CLIENT_ID`, `CLIENT_SECRET` |

The record is the source of truth, for the reason the preview gate's is: an
issuer hands out a client secret once and never again, so a binding Secret
somebody deleted is rewritten from the record rather than costing the
application a new client. Deleting the claim deregisters the client —
`deletionPolicy` has no say, because it exists to protect *data* from a
deletion nobody meant and an OAuth client holds none. What it holds is
permission to sign people in, which is the thing that must not outlive the
claim.

### Changing a client is not standard, and where that leaves a federated issuer

Registration is RFC 7591. *Changing* a registered client is RFC 7592, a
separate specification an issuer may implement or not — and the OAuth provider
plugin this chart ships does not: its registration answer names no client
configuration endpoint. So the operator prefers the standard route when the
issuer offers one, and otherwise maintains the client at `/kitchen/clients`,
the same private prefix the account directory lives on, authenticated by the
same service credential and refusing to touch any client the operator did not
itself register — the dashboard's own client included.

An issuer that offers neither is not a fault to fix, and the claim says so
rather than looking bound and behaving otherwise: it stays `Bound`, the client
keeps working everywhere it was registered for, and `RedirectURIsInSync` goes
`False` naming the URIs somebody has to add by hand. It is not retried on a
timer — an endpoint that is not there does not appear by being asked again.

## Preview protection (forward-auth)

Preview URLs used to be public: anyone who guessed `shop-pr-42.apps.example.com`
saw unreleased work. Now a preview is only useful to someone on the project it
belongs to, and the deployed application is not involved in that at all — it is
not told to authenticate anything, and it never sees the platform's session.
Who counts as "on the project" is [Preview admission](#preview-admission): any
role, `viewer` included, plus the platform's operators. A signed-in visitor who
holds none of them gets a page saying so rather than another trip through the
identity provider, which would only sign them in again and land them back on
the same wall.

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
- **The routing headers are not trusted.** The Gateway sets
  `X-Kitchen-Upstream` and `X-Kitchen-Project` with one `RequestHeaderModifier`
  filter, which overwrites whatever the client sent. The gate checks both
  anyway. The upstream has to be an in-cluster Service address, because a proxy
  that forwards wherever it is told is a way out of the cluster. The project
  has to be one the route demonstrably belongs to: either the upstream sits in
  that project's application namespace — `kitchen-<project>`, derived from the
  name — or the hostname is one the platform generates for it,
  `<project>-pr-<n>.<baseDomain>`. The second is what covers an environment
  that idles to zero, where the upstream is the shared KEDA interceptor and
  names no project at all. Neither derivation reaches an idling environment
  behind a *custom* domain, and the gate refuses that rather than believing the
  header; closing that gap would mean letting the gate read Domains, which is
  more than the read-only identity it has.

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

## Who may do what

Kitchen has two users, and until recently one of them was pretending to be the
other: the dashboard's operator mode changed what was *rendered*, not what was
*permitted*, and every valid token could call every route.

**This section is now enforced.** The REST API registers every route out of one
route → role table (`internal/api/policy.go`), the preview gate and the API
both resolve membership through `internal/access`, and the dashboard's copy of
the table is generated from the API's so the two cannot disagree.

The other half of that sentence — what operator mode *renders*, as against what
the role permits — is [the dashboard's design guide](UI.md#the-mode-rule), and
it is enforced too: `ui/src/lib/design.test.ts` refuses a developer screen that
prints a Kubernetes noun outside an operator gate. It was
written down before any of it was built on purpose — enforcement without a
written model is how a permission system ends up meaning whatever the first
three `if` statements happened to mean — and it remains the authority: where
the code and this section disagree, the code is what moves.

### The two people it is for

The developer and the operator, described in full under
[Who it is for](SCOPE.md#who-it-is-for). Three things from there are what the
roles below are built out of:

- **The developer should never need the words "namespace" or "Deployment".** So
  a role that can only be used by someone who knows what a Deployment is has
  failed before it is enforced.
- **The operator owns the cluster and wants the objects.** They are the only
  person for whom the platform's own machinery is a useful answer.
- **They are hats, not people.** In a single-person installation they are the
  same human ten minutes apart, so nothing here needs two accounts — `operator`
  below contains `developer` entirely.

### What this protects against, and what it does not

Every Kitchen custom resource lives in `kitchen-system`. Scoping access to a
project is therefore an API-layer concern rather than a namespace one, which is
far easier to build — and means **the API is the only thing enforcing it**.

That is not a hole to be plugged later. It is the trust boundary:

> **Cluster access is operator access.** Anyone holding kubectl on this cluster
> is an operator whether or not Kitchen says so.

What the roles do protect: accident, blast radius, and developers seeing each
other's projects and unreleased work. What they do not protect against:
somebody who already holds the cluster. Kitchen is [not a SaaS](SCOPE.md) and
has no multi-tenant threat model, so these are the permissions a team of
colleagues needs, not the ones a hosting provider needs.

### The roles

Two axes. One flat list cannot answer both "may Anna change the base domain?"
and "may Anna deploy `billing`?", so there are two, each kept as small as it
can be.

**Platform role — exactly one per account.**

| Role | What it is |
|---|---|
| `operator` | Owns the platform: everything, everywhere. Implies project `admin` on every project, present and future |
| `member` | An ordinary account. No platform surface at all — it sees what project membership grants it, and it may create projects |

**Project role — per account, per project.**

| Role | What it is |
|---|---|
| `admin` | Everything `developer` may do, plus membership, the project's own settings (git source, registry, previews policy), and deleting it |
| `developer` | The day job: builds, redeploys, rollbacks, environment variables, domains, claims, logs, deleting an environment |
| `viewer` | Reads status, URLs, builds, releases and logs — and may open a protected preview. No writes |

`operator` contains `developer` deliberately, for three reasons: a
single-person installation needs one login that wears both hats; somebody has
to be able to fix a project whose owner left; and the operator can reach
everything through the cluster anyway, so withholding it in the API would be
theatre.

`viewer` exists because [preview protection](#preview-protection-forward-auth)
shipped. Without a role meaning "may look, may not touch", the gate can only
ask whether a visitor is signed in — and on a platform where everyone in the
organisation has a login, that publishes every unreleased feature to all of
them. It is also the role for the person a preview link gets pasted to.

**Creating a project is self-service.** Any account may create one, and becomes
its `admin`. The alternative — only operators create projects — makes rolling
something out on a Saturday somebody else's weekend, which is the exact
bottleneck the operator persona is trying to get out of. Project names share
one flat namespace under the base domain, so they are first-come-first-served.

There is no platform-wide read-only role. Reading `/platform/*` without being
able to change anything is the most likely third platform role, and it waits
until somebody asks for it.

### Where membership lives

**The token says who you are. Kitchen says what you may do.**

Nothing new goes into the token: no scopes to enforce, no role claims, no
project claims. Membership lives in Kitchen's own custom resources, on the
object the access is about — `spec.access` on a Project, `spec.access.operators`
on the Kitchen singleton.

```yaml
# Project
spec:
  access:
    - subject: user_01H8X…       # the issuer's `sub`
      email: anna@example.com    # informational, so the YAML reads
      role: developer
```

The identity provider ships an organizations plugin, so putting membership
there is the obvious move. It is the wrong one, for four reasons:

- **It widens the contract this document rests on.** Kitchen integrates with
  "an OIDC issuer that supports dynamic client registration" and nothing more.
  Membership in better-auth organizations makes that "…*and* an organization
  model with custom roles, exposed as claims" — a far narrower set of products,
  and an issuer swap stops being a configuration change and becomes a data
  migration.
- **A claim is a stale snapshot.** An access token is good for an hour, so a
  role carried inside one means removing somebody from a project leaves them on
  it for up to an hour — unless every token they hold is torn down. Removal is
  the case where that delay matters most.
- **The operator already has the answer in memory.** It watches Projects, so
  "is this subject on `shop`?" is a cache lookup. That keeps the stateless
  request path the API already has, without adding anything to the token to
  avoid a round trip.
- **Membership is platform state, and platform state is custom resources.** In
  the identity provider's Postgres, `kubectl get project -o yaml` stops telling
  the whole truth, and nobody can declare access in git next to the Project it
  is about.

The canonical subject is the issuer's `sub`, which is opaque; the dashboard
resolves an address to one when it writes a grant. Hand-written YAML may name
an `email` subject instead, and that resolves against the token's `email` claim
**only when `email_verified` is true** — an unverified-email grant is a grant to
whoever can claim that address at the issuer.

### What each surface requires

| Surface | Role |
|---|---|
| `/platform/*`, `PATCH /settings`, `/connections/{name}` (bar its repository listing) and every connection write, `/updates`, `GET /environments/{name}/objects`, `GET /compliance`, `GET /audit/verify` | `operator` |
| `GET /settings` | `operator` — it carries the base domain, the issuer, the gateway address and the operator list itself |
| `DELETE /projects/{name}`, the project's own settings, membership and key writes | project `admin` |
| Builds and cancellations, releases, environment variables, environments, domains, claims | project `developer` |
| Projects, builds, releases, environments, logs, metrics, requests, diagnostics, signals, traces, and a project's members and keys | project `viewer` |
| `POST /projects` | any account a person signs in as — see [Machine accounts](#machine-accounts) |
| `GET /connections/{name}/repositories` | any account |
| `GET /status`, `GET /connections` | any account, with a body that varies by role |
| `GET /me` | any account — it describes the caller to themselves |
| `/logs`, `/events`, `/traffic`, `/metrics/overview`, `/traces`, `/audit` and the collection `GET`s | filtered to the projects the caller can see |

Two things in that table are worth saying out loud, because both were places
the first implementation drifted from this section and had to be moved back.
**Environment variables are the developer's, the project's settings are the
admin's** — so they are two routes (`PATCH /projects/{name}/env` and `PATCH
/projects/{name}`), rather than one route sorting its own body by field.
**Reading who is on a project is part of knowing what the project is**, so the
members list is the viewer's and only the writes are the admin's; a project's
CI keys are the same list with its non-human half shown, and go with it.

Four rules go with that table:

- **A whole route is the unit of authorization.** Filtering a response body by
  role is the exception, and there are exactly two. `GET /status` is the
  dashboard's home page for both people, so it keeps the build queue for
  everyone — "why is my build waiting" is a developer question — and drops the
  tunnel, the gateway, the component survey and the node counts for a `member`;
  a second endpoint would have doubled the surface for one payload. `GET
  /connections` answers a member with names, capabilities and readiness alone,
  because a project needs a git source and a registry to exist at all, and
  self-service that stops at the first form field hands the developer straight
  back to the operator. Both thinned shapes are distinct types rather than the
  operator's view with fields blanked, so that a field added to one later is
  not published to everybody by a struct they share. The repository listing
  next to it (`GET /connections/{name}/repositories`) is not an exception to
  the rule but an ordinary route: it is the same form's next field, it answers
  everybody the same thing, and what it carries is what the credential can
  see — never the credential.
- **A field withheld by role is absent, never zeroed.** The dashboard has to be
  able to tell "no tunnel is configured" from "you are not allowed to know", and
  an empty component survey reads as a healthy platform running nothing.
- **A refusal names the role it wanted** — *you have viewer on shop;
  redeploying needs developer* — which is the rule `kitchen.validate` already
  follows for chart guards: say what is wrong *and* what would fix it.
- **`GET /environments/{name}/objects` is operator-only, and a developer
  needing it is a bug.** It answers with Deployments, Services and HTTPRoutes,
  and the premise of the platform is that a developer never needs them. If one
  has to open it to answer a question, the missing thing is a product surface —
  file it, rather than widening the role.

### Machine accounts

A CI key needs no role of its own. The api-key plugin runs with
`enableSessionForAPIKeys`, but the session it mints for a key is a session for
the account the key *belongs to* — so a key has no `sub` of its own, and what
it is exchanged for at `GET /token` is an ordinary platform token for its
owner. Every key therefore gets an owner created for it: a machine account
holding that one key and nothing else, never verified and never counted as a
person. Granting **that** account's subject a project role in `spec.access`
makes a key a non-human member of exactly one project,
which is what stops a key that can trigger a build from also being able to
change the base domain — without a fourth role, and without storing permissions
on the key. Revocation stays where it already is: one place, at the issuer.

**A machine account may not create a project**, and that is the one place the
platform asks what *kind* of account is calling rather than what role it holds.
The rest of the model is deliberately blind to the distinction — a role is
resolved from the subject alone, and a machine account's address is a display
detail — but project creation is not a private act. Its creator becomes its
`admin`; an `admin` issues keys; and the operator goes on to register a webhook
on the named repository through the *platform's* git connection, build it on
the platform's builders and publish it under the base domain. A key that could
do that would be a credential able to mint its own successors, which is exactly
what `POST /projects/{name}/keys` refuses to issue an `admin` key to avoid.

The check is the reserved domain and nothing cleverer: a machine account can
only exist under `machines.kitchen.local` and a person can only exist outside
it, both enforced by the identity provider's own user table, so the address on
a token is the issuer's statement rather than the caller's. It never grants
anything — an address that is absent or under any other domain reads as a
person's, which keeps a federated issuer that sends no `email` claim working —
and every role every caller holds is still resolved from the subject alone.

### Preview admission

A protected preview admits anyone holding any role on that project, `viewer`
included, plus operators. The gate resolves that itself, against its own cached
client rather than by asking the REST API, so previews do not close when the API
restarts and the membership rule has exactly one implementation.

The gate runs as its own ServiceAccount, bound to a role with `get`, `list` and
`watch` on `projects` and `kitchens` and nothing else, and reads both through an
informer — an admission decision is a map lookup, since the gate is in the
request path of every protected preview. Nothing in that path can reach the REST API at all: the one
thing it holds is a `previewgate.Directory` over the cache, so "the gate never
asks the API" is a property of what it was handed rather than a rule somebody
has to keep remembering.

It fails closed. A gate that cannot read the platform — no cache, an informer
that has not synced, a ServiceAccount somebody narrowed — refuses every
protected preview and says the platform cannot check membership right now.
Guessing the other way would publish every unreleased preview on the platform
at the moment nobody is watching.

### Bootstrap, and what happens on upgrade

The account created by the [bootstrap link](#bootstrap-settled) is the first
operator. Nothing else grants the platform role implicitly.

Installations that predate enforcement need care rather than a default. Before
it, every authenticated account could call every route, which read honestly
means every existing account **was** an operator; enforcing against an empty
list would turn all of them into members with no projects — locked out of their
own platform by a minor version bump, with no way back that does not involve
kubectl. So an upgrade seeds the operator list from the accounts that exist,
writes it out explicitly, and says so in the release notes. Narrowing it is then
a decision somebody takes, rather than one that happens to them.

One rule does both. **An absent operator list means nobody has ever said who
the operators are**, so the accounts that exist become the answer: on a fresh
install that is the one account the bootstrap link created, and on an upgrade
it is all of them. An *empty* list is somebody's decision and is left alone,
which is why `spec.access.operators` carries neither a default nor `omitempty`
— collapsing "nobody has said yet" into "somebody said nobody" on the first
write is the whole failure this avoids. The API will not produce that state
itself: `PATCH /settings` refuses to empty the list, for the same reason the
last `admin` cannot leave a project. It is reachable by editing the object,
and it is honoured when it is found, because an installation that has decided
to run without a Kitchen operator should not have one appointed for it on the
next reconcile. While no account exists at all,
nothing is written and the reconciler tries again.

Seeding reads the accounts from the identity provider, which only the bundled
one can be asked for — an installation federated to somebody else's issuer has
no account directory to enumerate, so there is nothing to seed from and nobody
would hold the platform role at all. That installation names its operators at
install time instead, in the chart value `kitchen.access.operators`, which
writes them to the same field; it is the "deploy-time chart values" exception,
and the reconciler treats a list the chart wrote exactly like one a person
wrote. The `OperatorsConfigured` condition says which of the two situations an
installation is in, and names the way out of the one it cannot seed itself.

Reviewing what was seeded is the settings screen's job, not `kubectl`'s: `GET
/settings` carries the list and `PATCH /settings` writes it, and the list
cannot be emptied — a platform with no operator has nobody left who can
appoint one, which is the same rule that stops the last `admin` leaving a
project.

## Decisions

| Decision | Choice | Why |
|---|---|---|
| IdP integration | OIDC + dynamic client registration as the contract | Swappable implementation, standard protocol everywhere |
| Default IdP | better-auth + OAuth/OIDC provider plugin | Excellent DX; the provider plugin is its youngest part — accepted risk, mitigated by the contract above |
| Auth storage | Chart-managed single-node Postgres (external override) | OLTP; SQLite would cap replicas at 1; mirrors the ClickHouse pattern |
| App auth surface | `ResourceClaim` type `oidcClient` | Reuses the existing claim → binding-secret → env flow |
| An app client's redirect list | Maintained by the operator, out of the URLs it owns | The component that creates and deletes environments is the only one that knows when a callback appears or goes |
| Changing a registered client | RFC 7592 where the issuer has it, Kitchen's own prefix otherwise | The shipped provider plugin implements registration and not management; a federated issuer keeps its clients and loses the maintenance, reported on the claim |
| API tokens for CI | better-auth's api-key plugin, exchanged for a JWT at the issuer | The plugin already holds the operator's credential; the operator stays stateless and revocation stays in one place |
| Dashboard sessions | Rotating refresh tokens (`offline_access`), one per browser in `localStorage` | Renewal that needs no redirect and no framing of the login page; rotation is what makes browser storage defensible |
| Account management | The dashboard calls the issuer directly, with the issuer's session cookie | A password must never pass through the operator, and the endpoints are mounted and session-gated already |
| Preview protection | An in-path gate the routes pass through | Gateway API has no external-auth filter, and Cilium exposes none of Envoy's |
| Gate's OAuth client | One client, one redirect URI, registered by the operator | Previews come and go without touching the client |
| Authorization model | Two platform roles, three project roles, `operator` ⊇ `developer` | One axis cannot scope the platform and a project at once; anything more granular has to justify itself |
| Where membership lives | Kitchen's custom resources, not the identity provider | Keeps the issuer contract at OIDC + DCR; a role claim inside an hour-long token is a stale snapshot |
| Token shape | Identity only — no roles, no scopes to enforce | The operator resolves authorization from the cache it already keeps, so the token needs nothing added |
| Project creation | Self-service; the creator becomes its `admin` | Shipping something new must not wait on whoever owns the cluster |
| CI credentials' powers | A machine account holding a project role | A key is already an identity; no new role type, and no permissions stored on the key |
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
  mints a client per app, and the account directory and client management
  under `/kitchen`.
- `bootstrapToken` — the first-administrator link, below.

### Bootstrap (settled)

`helm install` prints a one-time link, `https://auth.<baseDomain>/bootstrap?token=…`.
It serves a form that creates the first administrator, and the endpoint closes
as soon as the installation has an account — the account *is* the state, so
nothing has to remember whether the token was used. Public sign-up stays off,
and a later account arrives through a configured upstream provider — that, and
nothing else. This used to say "or an organization invitation", which is not
true and never was: accepting one requires a session, so it cannot be the thing
that gives somebody their first. See "What account management still is not".

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

### Managing an account (settled)

Signing in was built and managing the account behind it was not: for several
releases the whole of the account surface was an account menu showing an
address and a **Sign out**, and a password could not be changed anywhere
(issue #207). `/account` in the dashboard is that half: the display name, the
password, and the browsers currently signed in as this account, with a control
to sign one of them out.

**It talks to the identity provider, not to the operator API**, and that is
the design rather than an exception grudgingly made. A password is a
credential, and the platform's rule is that the API never reads a credential
back — routing a password change through the operator would put every password
on the installation through a service that has no reason to see one, in order
to reach an endpoint the issuer already mounts and already gates on its own
session. So the browser calls the issuer directly, the way it already does for
discovery, the token exchange and revocation. The operator API gains nothing
and is not asked to.

The screen is `ui/src/views/AccountView.vue`, its client is
`ui/src/lib/account.ts`, and the endpoints are better-auth's own:
`/get-session`, `/list-accounts`, `/list-sessions`, `/update-user`,
`/change-password` and `/revoke-session`.

**The credential is the issuer's session cookie**, which is a different thing
from the bearer token every other screen uses and has a different lifetime.
Three things have to hold for the dashboard to use it, and only the middle one
is code:

- the browser must be willing to send the cookie to a fetch from another
  origin, which holds while the dashboard and the issuer are subdomains of one
  site — the chart's default, `kitchen.<baseDomain>` and `auth.<baseDomain>`,
  and *not* something the dashboard can arrange for an installation that has
  moved `auth.host` to another domain. The screen says so when it happens: a
  401 there is reported as the issuer not recognising the browser, naming both
  the expired-session and the different-site cases, rather than as the
  dashboard's own session having ended;
- **better-auth must trust the dashboard's origin**, or it refuses every
  cookie-bearing POST with `403 INVALID_ORIGIN` — that check *is* the CSRF
  defence on these endpoints. `trustedOrigins` is therefore derived from the
  same `allowedOrigins()` the CORS headers come from (`auth/src/config.ts`), so
  the two cannot drift apart: before this, the issuer trusted only itself,
  which was enough while nothing but its own pages posted to it;
- the CORS headers must let the answer be read, which they already did.

**There is no session freshness window** (`session.freshAge: 0`). better-auth
guards `/list-sessions` on a session created within the last day, and nothing
in Kitchen can make a session fresher without ending it: the dashboard's sign
out revokes its own OAuth tokens and leaves the session at the issuer alone,
and a new sign-in round trip reuses that session rather than replacing it. The
default would leave an account unable to see its own sessions for six of the
seven days one lasts, with nothing to do about it but wait to be signed out.
Changing a password — the one operation here that is genuinely sensitive —
proves the current password instead, which is the check that actually
re-authenticates.

**No command carries this, deliberately.** The CLI holds an API key, exchanges
it at the issuer for a token, and never holds a session cookie — so the
endpoints above are not reachable from it at all, and a `kitchen account`
command would have nothing to authenticate with. That is a property of how the
CLI signs in (below), not an omission: the credential a CLI actually holds is
a key, and rotating one is `DELETE /projects/{name}/keys/{key}` followed by
`POST /projects/{name}/keys` — both on the operator API, both reachable with
`kitchen api`, and both already on a screen.

### What account management still is not

Everything below needs the platform to be able to **send mail**, and Kitchen
ships no mail transport: no SMTP field on the auth service's `Config`, no
mail-related environment variable, no sender configured on better-auth. That
is a decision waiting to be made rather than a bug in something that exists,
and until it is made these read as follows:

- **Password reset is off at runtime, not merely absent.** Without
  `sendResetPassword`, `POST /request-password-reset` answers
  `400 RESET_PASSWORD_DISABLED`, and `/reset-password` needs a token only that
  path can mint. So a forgotten password is not recoverable by the person who
  forgot it.
- **Changing an address is refused for the same reason.** `/change-email`
  throws `CHANGE_EMAIL_DISABLED` without `user.changeEmail.enabled`, and then
  "Verification email isn't enabled" without a sender. An address is proved by
  mail or it is not proved.
- **An organization invitation cannot create an account.**
  `POST /organization/accept-invitation` is behind the organization session
  middleware, so the invitee has to be signed in already — which is the thing
  they cannot do. `organization()` is also constructed with no
  `sendInvitationEmail`, so no invitation is ever sent. On an installation with
  no GitHub OAuth app, the bootstrap account is still the only account that can
  exist.
- **The operator cannot create or reset one either.** `internal/idp` reads
  accounts and writes only keys and OAuth clients; there is no
  `/api/v1/accounts`, no `/api/v1/invites`, and `internal/api/members.go` says
  as much in the error a member sees when they name an address the issuer has
  never seen.

#### Resetting a locked-out password by hand

Until the decision above is made, an account that has lost its password is
recovered at the identity provider's database, and this is the one operation on
the platform that still needs cluster access. It is written down because the
alternative is that it gets invented on the day it is needed.

Compute a hash the way better-auth does — scrypt, `N=16384 r=16 p=1 dkLen=64`,
formatted `<hex salt>:<hex key>` — from inside the auth pod, which has the
implementation:

```sh
kubectl -n kitchen-system exec deploy/<release>-auth -- \
  node --input-type=module \
  -e 'import { hashPassword } from "@better-auth/utils/password"; console.log(await hashPassword(process.argv[1]))' \
  -- 'the new password'
```

Then write it onto the account's `credential` row and end its sessions, so the
old password cannot still be in use somewhere. The database is the identity
provider's own — `kitchen_auth` on `<release>-postgres` by default, or wherever
`postgres.external` points:

```sh
kubectl -n kitchen-system exec -it <release>-postgres-0 -- psql -U kitchen kitchen_auth
```

```sql
UPDATE account SET password = '<hash>', "updatedAt" = now()
 WHERE "providerId" = 'credential'
   AND "userId" = (SELECT id FROM "user" WHERE lower(email) = lower('anna@example.com'));

DELETE FROM "session"
 WHERE "userId" = (SELECT id FROM "user" WHERE lower(email) = lower('anna@example.com'));
```

An account with no `credential` row signs in through an upstream provider and
has no password here to reset; the row to add in that case is a new one, which
is the operator-created-account feature that does not exist.

### How the CLI signs in (settled, for now)

`kitchen` authenticates with an **API key, exchanged here for a token** — the
same path CI takes, and for the same reasons: the key never reaches the
operator, revocation stays in one place, and a key is a machine account holding
a role on exactly one project, which is the narrowest credential this platform
issues. That matters more for a CLI than anywhere else, since a laptop is where
a too-broad token ends up.

**A person cannot sign the CLI in through a browser, and that is a fact about
this issuer.** Both flows a CLI would use are unavailable:

- **No device authorization grant.** `@better-auth/oauth-provider` (1.6.27)
  implements none — there is no device endpoint to advertise in the discovery
  document and nothing for a CLI to poll. Adding one is a change here, not in
  the CLI.
- **A loopback redirect needs a client seeded for one fixed port.** The provider
  refuses a client whose `redirect_uris` are on more than one host, and a port
  is part of a host, so `http://127.0.0.1:<port>/callback` can only be
  registered for a single port — which may be in use. A public `kitchen-cli`
  client would be seeded the way `kitchen-ui` is (`seed.ts`, `config.ui`), with
  that trade made deliberately.

Until one of the two is decided, the key path is the whole of it. See
[CLI.md](CLI.md#signing-in).

## Open items

- **Sign-in pages**: the service ships its own minimal login and consent
  pages. The Vue UI takes them over when it lands.
- **Browser sign-in for the CLI**: a device authorization grant in the OAuth
  provider, or a seeded loopback client. Neither exists yet; the section above
  says what each would take.
- **A mail transport, or a decision not to have one**: password reset, address
  changes, invitations and operator-created accounts all wait on it, and every
  one of them is a hole a person falls into rather than a feature nobody asked
  for. "What account management still is not", above, is the whole list; until
  it is decided, a locked-out account is recovered by hand at the database.
