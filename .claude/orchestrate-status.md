# Orchestration plan — 9 September 2026

Supersedes the 8 September checkpoint note, whose state is now folded in
(v0.40.1 published; every issue that run dispatched is closed).

Maintainer's ask: work #573 and #574, cut a release and wait for it, then a
batch of quick bugfixes "like #377".

## Wave 1 — running

| Issue | Branch / worktree | State |
| --- | --- | --- |
| #573 custom-domain HTTP-01 deadlock | `claude/domain-http01-route-573` / `../kitchen-wt-573` | implementer dispatched |
| #574 gateway 404s attributed to the environment | `claude/gateway-404-attribution-574` / `../kitchen-wt-574` | implementer dispatched |

Neither may arm auto-merge: a release cut follows immediately and a feature
landing mid-cut moves the release head.

## Release

`v0.40.1` is live with its chart and is `main`'s tip (`5127a6e`); there are no
commits after it. Open release PR **#570 (0.41.0) is stale** — release-please
created it at 07:28 UTC, five minutes before the `v0.40.1` tag existed, so it
computed its range from an older baseline and its changelog re-lists releases
back to 0.38.0. It will be recomputed when wave 1 lands. **Verify the
changelog against `git log v0.40.1..origin/main` before merging it**, per the
skill's step 5.2 — do not merge #570 as it stands.

## Wave 2 — mapped, waiting on the release

The genuinely small set, all open, none with a PR: **#445, #413, #377, #564,
#518, #547**. Two waves at concurrency 3, file sets disjoint within each:

- **2a**: #445 (attestation reads `Status.Strategy`), #413 (docs duplication),
  #377 (the k8s stack).
- **2b**: #564 (SIGPIPE in the e2e job — after #573 lands, so the rebase is
  already paid), #518 (offering says which consumer classes it admits),
  #547 (the opening reading on the two signals read paths).

**#377 is genuinely quick, contrary to its own issue text.** The mapper ran the
upgrade in a scratch tree: controller-runtime v0.23.3 with the k8s stack at
0.35.3 gives a clean `go build`, a clean `go vet` (which compiles the tests,
including 59 fake-client sites), green unit tests, and controller-gen
v0.17.2 → v0.20.1 changes only the `controller-gen.kubebuilder.io/version`
annotation in 17 CRDs. No source changes at all. Not cleared in the scratch
tree, and therefore the review: `make test`'s envtest suite, `make lint`, and
the cr 0.21–0.23 runtime changes (leader election, the priority queue becoming
the default) that only a kind run proves. Put it early so the rest rebases onto
it once.

**Four carried as "small" are not.** #370 is a backend-for-frontend, not a
bugfix — new session definition, CSRF everywhere, `docs/AUTH.md` rewritten;
#519 needs a CRD field `Project` does not have; #554 is a `feat` by its own
title; #481 extracts a 1070-line view into a shared panel. None is in wave 2.

Collision to respect: #564's SIGPIPE pattern is at 11 sites through the same
2400-line kind job that #573 adds a case to. Swept whole it conflicts outright,
which is why it waits for #573 to merge and then sweeps.

## Needs the maintainer's eye

- A **draft release object `v0.23.1`** has been sitting untagged since
  3 September (`untagged-528ff84108d181afd78f`) — a publish that never
  finished, from before the draft/finalize ordering was fixed. `v0.23.1` was
  released properly later, so this is a stray object to delete by hand.

## Decisions taken on the maintainer's behalf

- **D16 (#377) — typed `fix(deps)`.** It changes no API, but it ships a
  different operator binary (controller-runtime 0.23's leader election and
  priority queue), and CLAUDE.md's rule is that a change worth shipping typed
  `chore`/`build` does not ship at all. `build(deps)` was the alternative.
- **D17 (#564) — sweep all 11 SIGPIPE sites, not the one failing step.** The
  narrow fix leaves the same flake in ten other steps; the sweep's only cost
  was the conflict with #573, which is paid by ordering it after #573 merges.
- **D18 (#547) — adopt #546's field names verbatim** (`reading`,
  `readingAt`, per row) on both read paths. A single `evaluatedAt` for the
  whole body was the alternative and cannot express a finding carried forward
  on a stale input.
- **D19 (#481) — extract the log surface into a shared panel** both screens
  mount, per the #469 precedent, rather than copying a 1070-line view.

## Decisions open for the maintainer

- **D20 (#445): are artifacts already signed `strategy: auto` left alone, or
  re-stated?** Recommend leaving them — a signed record is a record of what was
  signed, and re-signing makes the attestation store no longer append-only.
  This one blocks wave 2a.
- **D21: which of #370, #519, #554, #481 are wanted at all**, given none is the
  quick bugfix the batch was asked for. Recommend #554 next (smallest, and its
  `kubectl` pointer in the chart README contradicts "nothing needs kubectl");
  recommend parking #370 behind #321's CSP until it is wanted as a project.
- **D22 (#554): fold `status.systemLogs` into `GET /platform/storage`, or give
  it its own route?** Folding costs no policy row, no route-table row and no
  CLI decision. Only wanted if the store's health will grow more fields.
- **D23 (#519): is the project's signal-policy override a CRD field on
  `Project` (and so a `kitchen.json` question), or API-only?**
- **D24 (#481): does the Platform log screen borrow
  `GET /platform/signals` as its `requires`, or is `GET /platform/logs` added?**
  Borrowing is free; adding is the only way the screen's requirement states
  what it actually needs.
- D13 (#499), D14 (#497), D15 (#496) — carried from 8 September, still open,
  not blocking anything in this run.
