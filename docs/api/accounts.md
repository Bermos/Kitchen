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
 "platformRole": "operator"}
```

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
