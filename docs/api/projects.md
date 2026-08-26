# Kitchen — Projects

A project and everything that belongs to it. Its environment variables are a
route of their own rather than a field of its settings, because a whole route
is the unit of authorization here.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Creating a project

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "shop", "repo": "acme/shop", "connection": "gh", "registry": "harbor"}' \
  https://kitchen.apps.example.com/api/v1/projects
```

A project is a name, a repository in the provider's `owner/name` form, and the
two Connections it builds and stores images with — `connection` needs the
`gitSource` capability, `registry` needs `imageStore`. Optional fields with
their defaults:

```json
{"productionBranch": "main", "previews": true}
```

`rootDirectory` and `dockerfilePath` may be set here too, which is what
`POST /connections/{name}/detect` exists to get right: the preflight reads the
repository the way a build would, and a build context it showed to be wrong is
corrected on the form rather than after the first build has failed.

From a terminal this is [`kitchen projects create`](../CLI.md#creating-a-project),
which runs the preflight, creates the project and links the directory to it in
one command — and takes the repository and the name from the checkout it is run
in.

The name has to work as a DNS label of at most 46 characters, because
everything the platform derives from it — the application namespace, release
names, generated hostnames — has to fit Kubernetes' 63-character limit.
Naming a Connection that does not exist, or one without the needed
capability, is a `400`; a Connection the operator has not assessed yet is
accepted, and the project's own conditions report whether it fits.

**Project names are one flat namespace under the platform's base domain**, and
they are first-come-first-served. Every URL the platform generates is a
subdomain of that domain, so there is no scope a second `shop` could be
qualified with — and the second person to want the name is told so in words
rather than being handed the API server's account of an object in a namespace:

```json
{"error": "the project name \"shop\" is taken: names are one flat namespace under the platform's base domain, since every URL the platform generates is a subdomain of it, so they are first-come-first-served — choose another name"}
```

Answers `201` with the new project. The operator takes it from there:
namespace, webhook, and — once the first build of the production branch
lands — the production environment.

**That first build is created by the platform, not by the next push.** As soon
as the project's source and registry connections are usable, the
`ProjectReconciler` resolves the production branch's current tip and creates
one Build of it, recording the fact in `status.initialBuildRef` so it happens
exactly once. Without it, a project created from a repository nobody was about
to commit to would sit at "no builds yet" until somebody pushed an empty
commit to wake it up. The Build carries the same deterministic name the
webhook receiver would give it — `<project>-bld-<sha[:12]>` — so a push that
arrives at the same moment is the same object rather than a second build of
one commit.

**Creating a project is self-service, and the account that creates one becomes
its `admin`** — written into `spec.access` on the new Project, not implied, so
that `kubectl get project -o yaml` and a git diff both tell the whole truth
about who may do what with it. The grant is part of the create itself, one
request carrying both, so there is never an instant in which a project exists
that nobody administers.

## Changing a project's settings

`PATCH /projects/{name}` edits what a project already is. Every field is
optional and absent ones keep their value:

```json
{"productionBranch": "trunk", "previews": true, "previewsProtected": false,
 "buildStrategy": "dockerfile", "dockerfilePath": "build/Dockerfile", "rootDirectory": "apps/shop",
 "port": 8080, "replicas": 3, "cpu": "250m", "memory": "512Mi"}
```

`cpu` and `memory` are Kubernetes quantities and set request and limit alike;
an empty string clears one. The repository and the two connections are
deliberately not editable: rebinding a project to another repository is a
different project.

`processes` is what the project runs *besides* its web process — its queue
workers and its scheduled jobs, which share the release's image and
environment and are started with another command. It belongs on this route
rather than one of its own because it is the same decision as the port and the
replica count above it: what this project runs, and how much of it. The write
replaces the whole list, and an empty list removes every process. See
[Workers and scheduled jobs](processes.md) for the fields, for why a preview
runs none of them unless a process opts in, and for reading what an
environment is actually running.

`dataClass` classifies the data the project handles — `public`, `internal`,
`confidential` or `strictlyConfidential`, in ascending order; `""` removes
the classification, and absent means unclassified, shown as such and never
defaulted. It is the top of the classification hierarchy: a
[claim](connections.md)'s class may not exceed it, environments the platform
creates inherit it, and a release flip onto an environment rated below it is
refused — outright on environments without requirements, and as the named
`dataclass-le-environment` rule where a policy bundle is pinned. Reclassifying is always
allowed — environments that now sit below the class read as non-compliant in
the [inventory](audit.md#the-classification-inventory) rather than the
correction being refused — and every change is audit-logged privileged, with
the previous value.

**Environment variables are not on this route.** They are the developer's day
job where the project's own settings are the admin's, and a whole route is the
unit of authorization here — so they have one of their own, below. A body that
carries `env` is a `400` naming it, rather than a field quietly dropped:

```json
{"error": "environment variables are not changed here any more: send them to PATCH /projects/shop/env, which needs developer rather than admin"}
```

Settings land in the next release's snapshot — what is already running keeps
the configuration it was released with until the next deploy.

## Changing a project's environment variables

`PATCH /projects/{name}/env` carries one field, and it replaces the whole
list:

```json
{"env": [
   {"name": "PUBLIC_URL", "value": "https://shop.example.com", "previewValue": "https://preview.invalid"},
   {"name": "API_KEY", "fromSecret": {"name": "shop-api-key", "key": "key"}},
   {"name": "DATABASE_URL", "fromClaim": {"name": "shop-db", "key": "url"}}]}
```

A variable is a literal `value` (with an optional `previewValue` used in
previews), a `fromSecret` reference, or a `fromClaim` reference; naming more
than one source is a `400`. `{"env": []}` clears every variable, which somebody
may well mean; a body with no `env` at all is a `400` rather than the same
thing, because that is a client that forgot the field and not one asking for an
empty list.

A value goes in and never comes back out. Reading a project reports whether a
variable has one, not what it is:

```json
{"env": [
   {"name": "PUBLIC_URL", "set": true, "previewSet": true},
   {"name": "API_KEY", "set": false, "previewSet": false,
    "fromSecret": {"name": "shop-api-key", "key": "key"}}]}
```

A plain variable is exactly where somebody in a hurry pastes an API key, so it
is held to the same rule as a connection's credential. Replacing the whole list
therefore does not mean sending the values back: a variable whose `value` the
request leaves out keeps the one it already has, and an empty `value` clears it
— the bargain the credential fields make too. Repointing a variable at a
`fromSecret` or a `fromClaim` drops the value it used to carry, since the
reference is what replaces it.

The answer is the project, so a client that changed a variable renders the new
list without a second read. Variables land in the next release's snapshot, like
every other project setting.

## Who is on a project

Membership is a project `admin`'s to *change*, which is the point of it: adding
somebody to `shop` does not go through whoever installed the platform. An
operator holds `admin` on every project, so they can do it too — they need no
rule of their own here, and neither does anybody else.

**Reading the list is a `viewer`'s**, because knowing who else is on a project
is part of knowing what the project is: a viewer who opened the People tab and
was refused on load would be reading a screen about a project they can
otherwise see in full. Only the three writes want `admin`. The same split
applies to [the CI keys](#keys-for-ci), which are the same list with its
non-human half shown.

All four methods answer on one path, `/projects/{name}/members`, and it is the
readable form of `spec.access` on the Project:

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
{"items": [
   {"subject": "user_01H8X…", "email": "grace@example.com", "role": "admin"},
   {"subject": "user_01J2Q…", "email": "anna@example.com", "role": "developer"}]}
```

`subject` is the issuer's `sub` and is the canonical identifier; `email` is
informational, so a list of opaque strings still reads. (The two swap round for
an entry hand-written against an address — see
[AUTH.md](../AUTH.md#where-membership-lives) — where `subject` carries the address
and `email` is usually empty.)

**Adding somebody names them by address, and the platform resolves it.**

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"email": "anna@example.com", "role": "developer"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
{"subject": "user_01J2Q…", "email": "anna@example.com", "role": "developer"}
```

The address is turned into the account's `sub` at the identity provider before
anything is written, because the address is what a person can type and the
`sub` is what a token will actually carry. An address the identity provider
does not know is a `404` — *they have to sign in to Kitchen once before they
can be given a role on a project* — rather than a grant that would sit on the
project matching nobody. Somebody who is already a member is a `409`; change
their role rather than adding a second entry.

`subject` is the other way in, and takes an identifier as given:

```json
{"subject": "svc_ci", "role": "developer"}
```

That is for an identity with no address to resolve, and for an installation
federated to an issuer that serves no account directory, where resolving an
address answers `503` saying exactly this. Exactly one of `email` and
`subject` is required, and a `subject` that looks like an address is refused:
pass it as `email`, so it is resolved rather than stored as the weaker
verified-address grant. A CI key is a machine account and so is one of these
grants, but it is not written this way — [`POST
/projects/{name}/keys`](#keys-for-ci) creates the account, the credential and
the grant together, which is the only way to end up with all three.

**A member is addressed by `subject` in the body, not in the path**, on both of
the writes that change one:

```sh
curl -sS -X PATCH -H "authorization: Bearer $TOKEN" \
  -d '{"subject": "user_01J2Q…", "role": "admin"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members

curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  -d '{"subject": "user_01J2Q…"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/members
```

A `sub` is opaque and may contain `/`, `%` or `#`; every path segment this API
addresses an object by is a Kubernetes name, and adding a percent-encoding rule
that only bites on the accounts with awkward identifiers is worse than a
`DELETE` that carries a body. `PATCH` answers `200` with the grant, `DELETE`
answers `204`, and a subject the project has no grant for is a `404`.

**The last `admin` cannot be removed or demoted.** Both writes refuse with a
`409` that says what would fix it:

```json
{"error": "anna@example.com is the only admin on shop, and a project with no admin has nobody left who can add one: make somebody else an admin first, then remove this one"}
```

An operator is not counted as a substitute. They could indeed repair such a
project, but a project whose only listed admin is gone is exactly the abandoned
project the rule exists to prevent: everyone working on it would have to go and
find an operator to get anything changed, which is the bottleneck self-service
membership was built to remove.

Every membership write is recorded in the [audit log](./audit.md#the-audit-log) as an update
to the `Project`, with the member, the role and whether they were added,
changed or removed. A grant is the most consequential thing an admin can do to
a project short of deleting it, and — like a deletion — removing one leaves no
trace anywhere else once the entry is gone. The writes also carry the caller's
`resourceVersion`, so two admins editing the list at the same time get a `409`
rather than one of them silently overwriting the other's decision.

## Keys for CI

A key is a member of the project, so its routes sit next to the membership
ones and want the same roles: **`admin` to issue or revoke one**, because that
is adding and removing a member, and **`viewer` to list them**, because the
listing is the membership list with its non-human half shown and carries no key
value — only the prefix the issuer keeps of one, which is useless as a
credential.

**A key is owned by a machine account created for it.** That is the part worth
knowing, because the obvious reading is wrong: the identity provider's api-key
plugin runs with `enableSessionForAPIKeys`, and the session it mints for a key
is a session for *the account the key belongs to*. So the `sub` in the token a
key is exchanged for is its owner's, and granting "the key's subject" a role
would grant it to whoever created the key, on their own account. Every key
therefore gets an owner of its own — an account that is not a person, holds
that one key, and exists only to have a `sub` the project can grant a role to.

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"name": "nightly"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys
{"name": "nightly", "subject": "user_01K4M…",
 "email": "shop.nightly@machines.kitchen.local", "role": "developer",
 "prefix": "9f3a1c", "created": "2026-08-19T09:12:44Z",
 "key": "9f3a1c…"}
```

**`key` is in that response and in no other.** It is stored hashed, exactly as
every other key at the issuer is, so a lost key is deleted and reissued rather
than looked up. Every read answers the `prefix` alone, which is enough to tell
two keys apart and useless as a credential.

**Creating writes both halves, or neither.** The key at the issuer and the
grant in `spec.access` are the whole of the feature: a key nothing has granted
anything to authenticates and can do nothing, which reads as a broken platform.
So if the grant cannot be written the key is taken back before the request
answers, and in the one case where it cannot be taken back either, the error
says so and names the key rather than leaving a credential nobody knows about.

`role` is optional and defaults to `developer`. `viewer` is the other value, for
a key that only reads; `admin` is refused, because admin is the role that issues
keys and a credential in a build pipeline that can mint its own successors is one
nobody can account for. A narrower role than `developer` — a `deployer` that can
build and promote and nothing else — is
[deliberately open](../AUTH.md#machine-accounts) and would arrive as another value
here.

That reasoning has one other consequence, and it is why `POST /projects`
refuses a key: **a machine account may not create a project.** Refusing to
issue an `admin` key means nothing if a key can create a project it is already
the admin of and issue keys there instead. It is also the only route that asks
what kind of account is calling rather than what role it holds — everything
else about a key is an ordinary grant on an ordinary project. See
[AUTH.md](../AUTH.md#machine-accounts) for how the distinction is drawn, and
why it never grants anything.

**A key name is a DNS label**, lowercase letters, digits and dashes, at most 32
characters — it addresses the key in the path, and it is half of the machine
account's own address at the issuer. One name per project: the same name twice
is a `409`, because two credentials behind one grant would make "revoke that
key" ambiguous. The clash is decided against the issuer's own list *before*
anything is recorded, so the audit log never carries "the key `nightly` was
issued for `shop`" for a request that issued nothing.

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys
{"items": [
   {"name": "nightly", "subject": "user_01K4M…",
    "email": "shop.nightly@machines.kitchen.local", "role": "developer",
    "prefix": "9f3a1c", "created": "2026-08-19T09:12:44Z",
    "lastUsed": "2026-08-19T09:40:02Z"}]}

curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop/keys/nightly
```

`role` on a listed key is read from `spec.access`, not from anything stored on
the key. A key listed with no role is one whose grant has been removed — it can
authenticate and do nothing, and the listing says so rather than hiding it.

**`DELETE` revokes and un-grants, in that order**, and answers `204`. The
credential goes first because that is the half that matters: a grant naming an
account that no longer exists is a line to tidy up, and a key that still works
is not. Both writes are recorded in the [audit log](./audit.md#the-audit-log) as updates
to the `Project`, the same way a membership change is.

**Keys and people are one list.** A key's grant appears in
`GET /projects/{name}/members` like anybody else's, carrying `"kind": "key"`
and the key's name so it reads as what it is rather than as a stranger with an
odd address:

```json
{"subject": "user_01K4M…", "email": "shop.nightly@machines.kitchen.local",
 "role": "developer", "kind": "key", "name": "nightly"}
```

`kind` is derived from the address and is a display rule only — no access
decision anywhere reads it, and a role is resolved from the subject alone.

An installation federated to an issuer of its own serves no key endpoints: all
three answer `503` saying so, because keys are that issuer's to hand out.

## Deleting a project

```sh
curl -sS -X DELETE -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/projects/shop
```

Answers `202`: the operator's finalizer deregisters the git webhook, tears
down the project's environments (production included), garbage-collects its
builds, releases, domains and claims, and removes the application namespace.
There is no undo, which is why the dashboard makes you type the project's name
first.
