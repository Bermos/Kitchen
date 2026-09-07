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
- 2026-09-07: A claim's Secret reaches a pod by two paths — the automatic `KITCHEN_SERVICE_*` (`serviceBindingEnv`) and the release snapshot's `fromResourceClaim` in `resolveEnv` — and a new policy gate added to one left the other reading the old Secret while the condition claimed otherwise. Ask: does *every* path that reads this object go through the new gate, or only the one the tests call?
- 2026-09-07: `ui/` has no component tests (no @vue/test-utils, no jsdom), so a Nuxt UI prop misuse type-checks, builds and passes `npm test` with the control dead. `UCheckbox` array `v-model` + `:value` only works inside a `CheckboxGroupRoot`; bare it sets the ref to a boolean. Ask: does a new form control copy the `:model-value="arr.includes(x)"` + `@update:model-value` pattern the repo already uses?
- 2026-09-07: A refusal built from a format string interpolated an English phrase into a JSON example command (`'{"serves":["the class of the environment that needs it"]}'`). Ask: is the copy-pasteable command in every refusal actually accepted by the route it names?

- 2026-09-07: A cross-project correlator was widened to "any rule firing in N projects at once" using each finding's `Since` — but a large part of the catalogue stamps `Since` as `snapshot.Now` or `now - <constant>`, so its coincidence window is vacuous and conditions weeks apart read as simultaneous. Ask: for every rule this new logic reads, is the timestamp it compares a real instant or the round's own?
- 2026-09-07: A derived rule folded N Warning findings into one hardcoded `SeverityCritical` / `TierPage` row, so three volumes at 85% became a page on every existing installation under the unchanged default preset. Ask: what is the severity and tier of a derived finding, and is it bounded by the findings it is derived from?
- 2026-09-07: An "intersection over the affected set, only where exactly one member survives" test always passes on a single-node / single-StorageClass cluster, so the explanation rung fired on every correlation and blamed the only node there is — while the same file correctly special-cased a single Gateway (`len(Gateways) < 2`). Ask: does this intersection distinguish "they share it" from "the cluster only has one"?
- 2026-09-07: A signal round began querying the audit table on every evaluation; an installation with `compliance.audit.enabled: false` has no such table, so `status.signals.unreadable` gained a permanent `audit_records` UNKNOWN_TABLE line — the same failure #441 fixed for the audit route. Ask: does this new read ask whether the installation keeps the thing it is reading?

## Chain links that were missed

- 2026-09-06: A route landed without its `docs/API.md` row; a field landed without `docs/CRDS.md`; a chart value landed without its README row. The tests cover policy, schema and the dashboard's policy copy; the docs rows and the screen are what they cannot.
- 2026-09-06: A CLI decision left unstated. Every PR that touches a route says either which command carries it or that `kitchen api` does.
- 2026-09-07: A new claim condition surfaced on the API but not on the claim's screen. Ask: where does a developer see this without kubectl?
- 2026-09-07: A refusal written for the *provider's* owners was rendered verbatim on the *consumer's* Project-scope screen, telling a reader to make a call they would be refused. UI.md's "never handed a button they cannot press" covers another team's action too, and `design.test.ts` cannot see text that arrives as data. Ask: whose action is this sentence, and is that the reader?
- 2026-09-07: A kind e2e step asserted status, a Secret and one real HTTP call, and still never ran the second half of the chain (the environment-side read of the binding). Ask: which half of the feature does the green job actually execute?

- 2026-09-07: A finding that asserts a negative ("no shared node, no shared dependency, no change of ours") never consulted the snapshot's unreadable-input state, so it printed the negative for inputs it could not read. Ask: does any sentence this code prints claim something was checked, and can the check have been skipped?

## Decisions that should have been surfaced

- 2026-09-07: A stage environment stopped inheriting the project's criticality; a default that changes behaviour for existing installations must be in the PR body and the changelog.
- 2026-09-07: Backup encryption on by default stopped uploads for installations without a key; carried as `!` with a `BREAKING CHANGE:` footer — that is the right shape, check for it.
- 2026-09-07: The platform CA is now created for its own sake, changing a stated design comment; the PR body named it. Ask: does the diff contradict any comment or doc that states a design intent, and is the contradiction named?
- 2026-09-07: One provider request per build for an opt-in feature; a queued build re-reads its config every requeue. Ask: what does this cost an installation that never turns the feature on?
- 2026-09-07: A default that breaks existing installations was named only in the PR body's "decisions" list, with no `!` on the title and no `BREAKING CHANGE:` footer — the Commits check passes either way, so this is the reviewer's to catch. Ask: does the body describe a break the subject does not carry?
- 2026-09-07: A stated blast radius was understated: "workloads deploy without the variables" was true of one code path and false of the other, where the environment went notReady and stopped applying its Deployment. Ask: is the blast radius in the body true of every path, or only the one the author tested?

- 2026-09-07: A screen, a CRD godoc, a docs page and an API-served preset description all stated a per-project override as fact while it was deferred to a follow-up issue. Ask: does the copy describe the platform that exists, or the one the design intends?
- 2026-09-07: `Closes #<issue>` on a branch that names one of the issue's own normative sentences as unbuilt. Ask: does the issue text treat the deferred criterion as required, and should this be `Refs` plus a maintainer decision?

## Commit and title

- 2026-09-06: A `chore` that should have shipped would not have cut a release. Ask: does the type match what the change does to the version, on every commit and on the title?
- 2026-09-06: Under squash the title alone lands; under rebase every subject lands. Read both as release notes.
