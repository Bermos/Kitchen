# Kitchen — Policy decisions

Every verdict the platform's policy engine reaches — a promotion allowed, a
scheduled re-evaluation gone non-compliant, a replay — is a stored decision,
kept with everything needed to reproduce it: the policy bundle by digest, the
fully materialized input by digest and in full, and which rules fired. The
engine is a pure function of that bundle and that input — bundles are
compiled with no network builtins at all (`http.send`, `net.*` and
`opa.runtime` are removed from the capability set, so a bundle naming any of
them is refused before it runs) — which is what makes replaying a decision
years later meaningful rather than theatre.

Part of the [REST API](../API.md), which carries the authentication, the
authorization model and the full route table.

Decisions live in the platform's store, not the cluster, so visibility
follows the decision's own `project`: a member sees their projects'
decisions, a decision about no project belongs to the platform's operators,
and one the caller may not see answers the same 404 a missing one does.
An installation without a telemetry store still evaluates and enforces —
what it cannot do is keep replayable records, and `GET /compliance` (the
`policy` block) says so.

## Listing decisions

```sh
curl -H "authorization: Bearer $TOKEN" \
  "https://api.kitchen.example.com/api/v1/decisions?verdict=blocked&kind=promotion&limit=50"
```

Newest first. `?project=`, `?environment=` and `?release=` narrow to a pair
or an artifact's history; `?verdict=` to `allowed`, `allowed-with-exception`
or `blocked`; `?kind=` to `promotion`, `rescan` or `replay`; `?since=` /
`?until=` (RFC 3339) bound the window. Each item summarizes the decision —
the full input rides only on the single read below.

`rulesFired` is a list of `{"rule", "message", "waived", "exception"}`
objects. A waived rule *fired* — an exception changed the verdict, never the
facts — and `allowed-with-exception` means every fired rule was waived.

## Reading one decision

```sh
curl -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/decisions/{id}
```

The decision whole: verdict, fired rules, `bundleDigest`, `inputDigest`,
`dataSnapshot` (the dataset the evidence was produced against, e.g. a
scanner's vulnerability database identifier), and `input` — the exact
canonical JSON the engine evaluated, which is what the digest names and what
a replay re-runs.

## Replaying a decision

```sh
curl -X POST -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/decisions/{id}/replay
```

Re-evaluates the stored input against the stored bundle bytes (`policy_bundles`
keeps every bundle a decision has cited, so a replay outlives the ConfigMap a
bundle came from) and answers `201`:

```json
{"original": {"verdict": "blocked"},
 "replay": {"verdict": "blocked", "fired": [{"rule": "require-sbom", "message": "…"}]},
 "match": true, "decision": "…"}
```

`match: false` means the stored decision does not reproduce — which is
exactly the finding this endpoint exists to surface. The replay is itself
stored as a decision of kind `replay` (and audit-recorded), so the check has
a record too; exception expiry is judged against the *original* evaluation's
clock, so a decision made under a since-expired exception still reproduces.

Replaying needs `developer` on the decision's project — it writes a decision
in the project's name — enforced by the handler because the project lives on
the stored row, out of the enforcement table's reach. `409` answers a
decision that cannot be re-run: its input is unreadable, or its bundle is
neither in the store nor still available.

## The policy bundles

```sh
curl -H "authorization: Bearer $TOKEN" \
  https://api.kitchen.example.com/api/v1/policy/bundles
```

Operator-only: the bundles currently available to require, each with the
`digest` an environment's requirements pin, its `source`, and the stable
`rules` ids it can fire. Two sources exist:

- `built-in` — the default bundle compiled into the operator
  (`package kitchen.promotion`; every rule is opt-in through
  `input.parameters` or inert until its facts exist).
- `configmap/<name>` — a ConfigMap in `kitchen-system` labelled
  `kitchen.bermos.dev/policy-bundle: "true"`, each key a file (`.rego`
  modules, optionally one `data.json`). This is how an institution
  distributes bundles of its own — through the chart, GitOps, or by hand at
  bootstrap.

A bundle is pinned by digest, never by name: editing a ConfigMap in place
creates a *new* bundle with a new digest, and nothing that pinned the old one
moves until its owners repin. The bytes a decision cited are persisted to the
store on first use, so deleting the ConfigMap deletes nothing a decision
depends on.

## CLI

`kitchen decisions list|show|replay` cover the three reads and the replay;
`kitchen api GET /policy/bundles` reaches the bundle listing.
