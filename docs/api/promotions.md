# Kitchen — Promotions

A promotion is one request to move one release into one environment, with the
policy's answer on it. It is how an artifact travels a project's staged
pipeline: the same image digest at every stage — nothing is rebuilt between
them — judged at each boundary by that environment's own requirements.
Rollback is the same request at an older release, which is why it needs no
rebuild either.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table these sections belong to.

## Asking for a release to land

```sh
curl -sS -X POST -H "authorization: Bearer $TOKEN" \
  -d '{"environment": "shop-production", "release": "shop-rel-41", "reason": "ship 1.4"}' \
  https://kitchen.apps.example.com/api/v1/projects/shop/promotions
```

Both `environment` and `release` must belong to the project — a body naming
something that does not is a `400`, not a promotion that fails later.
`reason` is optional and lands in the audit record; an emergency move is
expected to have one.

Answers `201` with the promotion, phase `Pending`, and that is the whole of
what the API does: the promotion reconciler resolves the references,
materializes the policy input from the artifact's *stored* evidence — never a
live check — evaluates the environment's requirements through the platform's
one policy engine, records the decision (audit log, decision store, and the
`promotion-decision` attestation on the artifact), and only then moves the
environment. The spec is immutable, like a Release's: retrying a blocked
promotion is a new promotion, and the old one stays as the record of what was
refused and why.

The phases, and where they end:

- `Pending` → `Evaluating` — picked up, requirements being evaluated.
- `Allowed` / `AllowedWithException` — the policy said yes (with every fired
  rule waived by an exception, for the latter); the flip is about to happen.
- `Applied` — the environment runs the release. `appliedAt` says when, and
  the artifact carries a signed `deployment/v1` attestation saying so.
- `Blocked` — terminal. `unmetRules` names the rules that stood in the way,
  as the stable ids the bundle publishes; `message` carries their own words;
  `decisionID` leads to the stored decision with the full fired list and the
  replayable input.
- `Failed` — terminal. The request itself was unusable: references that do
  not line up, or objects that are gone. Nothing judged the artifact.

An environment that declares no requirements accepts anything, exactly as it
always has — the promotion still records a decision, so the register says the
release was allowed with no rules evaluated.

## Reading what became of the asking

```sh
curl -sS -H "authorization: Bearer $TOKEN" \
  "https://kitchen.apps.example.com/api/v1/projects/shop/promotions?phase=Blocked"
```

Newest first. `?environment=`, `?release=` and `?phase=` narrow it. One
promotion whole is `GET /api/v1/promotions/{name}`.

## Where promotions come from

Three places, all landing in the same object:

- **This endpoint** — a person or a CI key asking (`trigger: manual`).
- **A finished production-branch build** — when the target environment
  declares requirements, the build controller creates a promotion instead of
  moving the environment itself (`trigger: automatic`, requested by
  `system:controller/build`). A target with no requirements is moved
  directly, exactly today's behaviour.
- **An applied stage** — a project with `spec.promotion.stages` configured
  chains them: when a stage applies and the next one has `autoPromote`, the
  promotion reconciler creates the next promotion. Gating an automatic
  promotion on evidence — a passing end-to-end gate from the stage below —
  is a rule on the next environment's requirements (`require-gate`), not a
  second mechanism.

`PATCH /environments/{name}` keeps working for environments without
requirements. Against one that declares them it answers `202` with the
promotion the move became, because the answer to "may this land" is the
policy engine's, whichever route asks.
