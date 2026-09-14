# Kitchen — Accounts

What an account is allowed to do is the same question the rest of this API
asks on every request; this is the one route that answers it directly.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Who the caller is, and what they may do

`GET /me` is the caller described to themselves, and nothing about anybody
else — which is why any valid token may ask for it:

```json
{"subject": "user_01H8X…", "email": "anna@example.com", "name": "Anna",
 "platformRole": "operator", "kind": "person"}
```

`kind` is which of the three sorts of caller this is — `person`, `key` for a
project's CI key, or `credential` for one of the platform's own. It is answered
because a credential asking who it is has no other way to find out: what marks
one is the reserved domain its address sits under, which is a convention of the
identity provider's that no client should be reimplementing.

A **platform credential** is told what it holds as well:

```json
{"subject": "user_01H8Y…", "email": "nightly@platform.kitchen.local",
 "platformRole": "member", "kind": "credential",
 "scopes": ["platform.read"], "projects": ["shop"]}
```

`scopes` is the *live* answer: the expiry is applied where the scopes are
resolved, so a credential that has lapsed reports holding none — which is the
question somebody asks when a scheduled job starts getting 403s. `projects` is
the allowlist its project-shaped scoped routes were narrowed to, absent when it
was not narrowed.

Both are absent for the overwhelming majority of callers, who hold no scope at
all: a person holds a role, and an empty list would read as somebody who has
been granted nothing. They are resolved for **every** caller rather than only
for a credential, though, because a grant in `spec.access.credentials` is
resolved from the subject alone like every other grant — an operator can write
one naming a person, and an answer that reported scopes only for an address
under the reserved domain would describe that person to themselves while
leaving out what they hold. See
[AUTH.md, "Platform credentials"](../AUTH.md#platform-credentials).

The project half of the answer is not here, because a dashboard rendering a
list of projects would have to join it back on: **every project payload carries
the calling account's role on that project**, as `role`, in `GET /projects` and
`GET /projects/{name}` alike.

```json
{"name": "shop", "role": "developer", "repo": "acme/shop", "…": "…"}
```

It is the role itself rather than a set of capability booleans (`canDeploy`,
`canDelete`). The role is what the API enforces, and what a client may offer is
derived from the same table it is enforced from — a second vocabulary would be
a second opinion, and the two would drift. An operator reads `admin` on every
project, including ones they are not listed on.

## Personal keys

`GET /me/keys`, `POST /me/keys`, `DELETE /me/keys/{key}` — the credentials this
account signs its own automation with
([#593](https://github.com/Bermos/Kitchen/issues/593)).

A personal key is the caller: the token it is exchanged for carries their
subject, so every project role they hold and the operator role if they wear one
are resolved from it exactly as when they sign in. It is what "write a script
that does what I would do" needs, and it is the credential this platform spent
two features refusing to issue — [AUTH.md, "Personal
keys"](../AUTH.md#personal-keys) has why that changed and what bounds it.

There is no `subject` parameter on any of the three, and there is not going to
be one: these routes are about the caller and nobody else, which is what lets
them ask for nothing but a valid token. Reading who else holds what is
[the access survey's](access.md) question, and it answers it without ever
naming a key.

### What a key may do: all of you, or a slice

A key is **unrestricted** — the whole of what its owner can do — or
**fine-grained**: issued for named projects, at most a named role inside them
([#595](https://github.com/Bermos/Kitchen/issues/595)). Naming a `role` is what
narrows it.

| Request | The key holds |
|---|---|
| `{"name": "laptop"}` | everything its owner holds, including the operator role if they wear one |
| `{"name": "ci", "role": "developer"}` | developer, capped, on every project its owner can reach — and no operator role |
| `{"name": "ci-shop", "role": "developer", "projects": ["shop"]}` | developer on `shop`, nothing anywhere else |
| `{"name": "backups", "role": "viewer", "projects": ["shop"], "scopes": ["backup.run"]}` | the above, plus one platform operation. Operators only |

**The role is a ceiling, not a grant.** What a key holds on a project is the
lesser of its role and what its owner holds there, resolved on every request —
so a key cannot be widened by editing its entry, by the owner being promoted
later, or by anything else. It can only ever be narrower than the person.

**It is a role rather than a grid of permissions**, and that is Kitchen's model
rather than a simplification of GitHub's. A role is what this API enforces, and
what a client may offer is derived from the same table it is enforced from; a
key that could be given "may deploy but not read logs" would be a second
vocabulary for saying what a developer is, and the two would drift the first
time a route was added.

Two things a narrowed key never gets, whoever issues it:

- **the operator role**, because it is everything everywhere and a credential
  carrying it is not narrower than its owner in any sense worth the word;
- **`POST /projects`**, because a project's creator becomes its admin — which
  is the one act that would hand a key something it was not issued for. The
  refusal names the key and what it reaches.

A **platform scope** on a key is an operator's to hand out and is refused to
anybody else — and it is honoured only while its owner is still an operator, so
a credential cannot outlive the authority that issued it.

**A key issued by 0.41.0 must be reissued.** That release wrote the credential
at the identity provider and nothing on the platform, because there was nothing
to narrow; a key now holds what its entry on the singleton says, and a key with
no entry holds nothing at all — which is what makes deleting an entry a
revocation rather than a promotion. Such a key reports `"unknown": true` and is
refused in those words. `kitchen keys list` shows them; revoke and issue again.

### Issuing one needs a browser sign-in

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" -d '{
  "name": "ci-shop", "role": "developer", "projects": ["shop"], "expiresInDays": 30
}' https://kitchen.apps.example.com/api/v1/me/keys
```

```json
{"name": "ci-shop", "prefix": "7f21ac", "created": "2026-09-14T09:12:04Z",
 "expires": "2026-10-14T09:12:04Z",
 "scope": {"projects": ["shop"], "role": "developer"},
 "key": "7f21ac…"}
```

A `role` above what you hold on a named project is refused rather than clamped:
a key issued as `admin` that silently resolves to `developer` is a key whose
first failure is somebody else's outage. A project you cannot see is answered
as one that is not there, like every other unreadable project. And naming
projects without a role is refused too — a request that meant to narrow and did
not say how should not be granted everything.

`key` is in this response and in no other. It is stored hashed, so nothing can
read it back — not the dashboard, not this API, not an operator. A lost key is
revoked and reissued.

`name` is lowercase letters, digits and dashes, at most 32 characters: it is
how the key is addressed and revoked, so name it after what will hold it.
One name is one key per account; a repeat is `409`. `expiresInDays` defaults to
30 and anything over 90 is refused rather than clamped — a caller who asked for
a year and was quietly given ninety days would find out when the pipeline
broke.

**`403` unless the token was issued to one of the platform's own OAuth
clients**, which is to say unless somebody signed in for it:

```json
{"error": "issuing a personal key needs a browser sign-in: this token was exchanged from a credential, and a credential does not issue credentials. Sign in to the dashboard and do it there"}
```

That is the whole of how the platform's "no credential mints its own
successor" rule survives a credential that carries every role its holder has.
A CI key, a platform credential and a personal key are all exchanged at the
issuer for a token that names no client, so none of them can ask. The dashboard
is where one is made — Account → Personal keys.

### Reading and revoking

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  https://kitchen.apps.example.com/api/v1/me/keys
```

```json
{"items": [
  {"name": "laptop", "prefix": "7f21ac", "created": "2026-09-14T09:12:04Z",
   "expires": "2026-10-14T09:12:04Z", "lastUsed": "2026-09-20T06:00:11Z"},
  {"name": "nightly", "prefix": "0ab41d", "created": "2026-06-02T11:40:00Z",
   "expires": "2026-09-01T11:40:00Z", "expired": true}
]}
```

`lastUsed` is absent for a key nothing has used yet, which is a different
statement from "used at the zero time" and is what answers "is this still the
credential my pipeline is holding". `expired` is answered rather than left to
be worked out from two dates: the issuer refuses a lapsed key on presentation
and deletes the row then, so this is the window in between.

`scope` is what a narrowed key may do, and its absence means the key is its
owner entire. A key carrying `"unknown": true` is one the identity provider
still has and **this platform has no record of** — revoked here and not there,
or its entry removed — and it holds nothing at all. It is answered rather than
shown as unscoped, because a list that read the most alarming state as the most
permissive one would be a list nobody could act on.

`GET /me` answers the same thing about the credential in hand, under `key`,
which is where `kitchen whoami` reads it:

```json
{"subject": "user_01H8X…", "email": "anna@example.com", "platformRole": "member",
 "kind": "person", "key": {"name": "ci-shop", "role": "developer", "projects": ["shop"]}}
```

That line is the answer to "why can this token not do what I can", and nothing
else on the payload can give it: the token says it is Anna, and Anna is an
admin. A key the platform does not recognise reads `"unknown": true` there too,
and every route refuses it in those words rather than as "you have no role" —
which would be true of a credential belonging to somebody who administers the
project, and would send them to ask for access they already have.

`DELETE /me/keys/{key}` answers `204`. It stops working immediately and
everywhere — the key is verified at the identity provider, so there is nothing
cached for it to keep working against — while a token somebody already
exchanged it for lives out its few minutes, which is the bargain every
credential on this platform makes. **A key may revoke itself**, and should, if
the one in hand is the one that leaked.

Both reads and the revoke ask for nothing but a valid token. Revoking is not a
widening, and a credential that can take a key back the moment it leaks should
be able to. A caller that is not a person — a CI key, a platform credential —
holds no personal keys and is answered with an empty list rather than a
refusal.

### The CLI

`kitchen keys list` and `kitchen keys revoke NAME` are these two, and there is
no `kitchen keys create`: the CLI holds a key, and a key may not issue one. The
listing's `MAY` column is each key's reach, and `kitchen whoami` says which key
is in hand and what it was narrowed to. A personal key is stored like any other
credential — `kitchen login --api-key-stdin` — and from then on the CLI is that
person, or the slice of them the key was issued for. See
[CLI.md](../CLI.md#signing-in).
