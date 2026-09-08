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
