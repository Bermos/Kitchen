# Kitchen — Exceptions

The break-glass surface. An Exception is a bounded, two-person, per-rule
waiver: it names the policy rules it waives (never everything), the project
and environment it applies to (optionally one release), the requester, the
approver, a reason, and an expiry. The rules still evaluate and still report
— an exception changes the verdict to `allowed-with-exception`, never the
facts — and every use of one is a privileged audit record plus a
`break-glass/v1` attestation on the artifact it carried. The design rule
behind all of it: never hard-block an emergency deployment, because a blocked
hotfix gets deployed around the platform and leaves no record at all.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table.

## Requesting an exception

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/projects/shop/exceptions \
  -d '{
    "environment": "shop-production",
    "ruleIDs": ["max-severity"],
    "reason": "hotfix for the checkout outage",
    "approvedBy": "cto@example.com",
    "incidentRef": "INC-421",
    "expiresAt": "2026-08-22T09:00:00Z"
  }'
```

`201` with the exception. Developer on the project may request — the on-call
engineer at 3am — and the **escalation ladder** decides who must approve:
the approver named in `approvedBy` must hold the role the ladder demands for
the requested duration (`expiresAt` minus now). The default ladder: up to
24h needs `developer` on the project, up to 720h (30 days) needs `admin`,
anything longer needs a platform `operator`. The ladder is configurable on
`Kitchen.spec.compliance.exceptions.ladder`.

What is verified, and what is asserted — stated plainly because the trust
model matters: the platform verifies the approver is *somebody else* (an
`approvedBy` matching the caller is a `400`, and the CRD refuses
`approvedBy == requestedBy` besides) and that they *hold the required role*
on the project or platform. That the approver actually agreed is the
requester's assertion, made on a permanent, privileged audit record carrying
both names, visible in the register and on every artifact the exception
moves. `release` optionally narrows the grant to one release; `autoRollback`
(default false) is carried for the rescan controller and does not change
what this endpoint does.

Refusals: `400` for a missing reason, no `ruleIDs`, an `expiresAt` not in the
future, an approver who is the caller, or an approver without the ladder's
role — each says which and how to fix it.

## The register

```sh
curl -H "authorization: Bearer $TOKEN" \
  "https://api.kitchen.example.com/api/v1/exceptions?historical=true"
```

Soonest to expire first. Active grants by default; `?historical=true` adds
the expired and the resolved — exceptions are **retained** through their
whole lifecycle, because the register's history is the point (project
deletion is what garbage-collects them). `?project=` and `?environment=`
narrow. Visibility follows the caller's projects, like every cross-project
read.

Each item carries the grant whole (`ruleIDs`, `reason`, `requestedBy`,
`approvedBy`, `incidentRef`, `expiresAt`) plus `phase`
(`Active`/`Expired`/`Resolved`, judged against the clock, so a grant past its
expiry never reads Active however recently it was reconciled), `usedBy` —
every promotion that relied on it — and, once resolved, `resolvedBy` and
`resolvedAt`.

`GET /api/v1/exceptions/{name}` reads one.

## Resolving an exception

```sh
curl -X PATCH -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/exceptions/{name} \
  -d '{"resolved": true, "reason": "patched in 1.4.2"}'
```

`200` with the exception, now `Resolved`. Admin on the project (an operator
holds that everywhere): granting a waiver took two people, but ending one
only narrows what is allowed. The reason is required — a resolution is an
auditable act, recorded privileged with the reason in its details. Resolving
an already-resolved exception is a `409`.

## What an exception does

At promotion, the engine evaluates every rule as always; fired rules named by
an active, unexpired exception covering the (project, environment,
release-or-any) triple are reported `waived: true` with the exception's name.
All fired rules waived ⇒ verdict `allowed-with-exception`; the promotion
proceeds, the exception's `usedBy` gains the promotion, the audit log gains a
privileged record, and the artifact gains a
`https://kitchen.bermos.dev/attestation/break-glass/v1` attestation
(exception, waived ruleIDs, reason, requestedBy, approvedBy, expiresAt,
environment, promotion, incidentRef) — the fact travels with the image, while
the authoritative record stays on the Exception.

One rule id lives outside the policy engine: **`require-pull-request`**. A
project's `requirePullRequest` setting refuses a direct push to the
production branch at *build* time, before any promotion exists. An active
exception for the project's production environment (with no `release`
narrowing) whose `ruleIDs` include `require-pull-request` converts that
refusal into an allowed, loudly recorded build: privileged audit record, the
exception's name on the build's source status, and an `exception`/`exempt`
field on the signed pull-request-approval attestation.

**Expiry** is enforced by the platform, not by good intentions: an expired,
unresolved exception waives nothing — further promotions that needed it are
`Blocked`, naming the expired exception in their message — and the deployed
release goes non-compliant on the next re-evaluation pass. `autoRollback` is
available for installations that want the environment rolled back on expiry,
and off by default.
