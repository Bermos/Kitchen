# Reviewer memory

Defects that reached `main` or CI in bermos/kitchen, and the blind spots that let
them through. Each is a question to ask of every diff. Dated, one line each.

## Defects that shipped

- 2026-09-07: Every `BP_*` build variable the operator set since #69 was inert: the CNB lifecycle drops container env not on its include list. Two PRs asserted the variables on the container and passed. Ask: does the consumer of this variable actually read the process environment, or a file?
- 2026-09-07: A stock-Nuxt e2e case merged red twice because its job was not a required check. Ask: is the job that proves this change required, and did it pass on this head rather than on an earlier snapshot?
- 2026-09-07: An e2e case's `kubectl get resourceclaim` hit Kubernetes 1.34's built-in kind, and `|| true` in the wait loop hid it. Ask: what does each `|| true` and `2>/dev/null` in a workflow swallow?
- 2026-09-07: A Certificate was created into `kitchen-databases` before the namespace existed; the fake client admitted it. Ask: for every object created into a namespace the reconciler also creates, is the namespace ensured first, and is the ordering pinned?
- 2026-09-06: A test that hashed a record whose timestamp defaulted to `time.Now()` flaked. Ask: does any test depend on the wall clock?
- 2026-09-06: A workload's Service selector matched its own workers because a selector cannot say "has no label". Ask: does every selector select exactly the pods meant, including ones added later?
- 2026-09-06: An audit route answered 503 for an installation with no audit table and nothing read it end to end. Ask: is there a CI read of the new route's failure mode, not only its success?

## Chain links that were missed

- 2026-09-06: A route landed without its `docs/API.md` row; a field landed without `docs/CRDS.md`; a chart value landed without its README row. The tests cover policy, schema and the dashboard's policy copy; the docs rows and the screen are what they cannot.
- 2026-09-06: A CLI decision left unstated. Every PR that touches a route says either which command carries it or that `kitchen api` does.
- 2026-09-07: A new claim condition surfaced on the API but not on the claim's screen. Ask: where does a developer see this without kubectl?

## Decisions that should have been surfaced

- 2026-09-07: A stage environment stopped inheriting the project's criticality; a default that changes behaviour for existing installations must be in the PR body and the changelog.
- 2026-09-07: Backup encryption on by default stopped uploads for installations without a key; carried as `!` with a `BREAKING CHANGE:` footer — that is the right shape, check for it.
- 2026-09-07: The platform CA is now created for its own sake, changing a stated design comment; the PR body named it. Ask: does the diff contradict any comment or doc that states a design intent, and is the contradiction named?
- 2026-09-07: One provider request per build for an opt-in feature; a queued build re-reads its config every requeue. Ask: what does this cost an installation that never turns the feature on?

## Commit and title

- 2026-09-06: A `chore` that should have shipped would not have cut a release. Ask: does the type match what the change does to the version, on every commit and on the title?
- 2026-09-06: Under squash the title alone lands; under rebase every subject lands. Read both as release notes.
