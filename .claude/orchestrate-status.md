# Orchestration run — 9 September 2026, final

The maintainer asked for #573 and #574, a release, then quick bugfixes like
#377, then a second release and a wrap-up. All of that is done. This note is
the run's record; delete it when its content is back in a plan.

## Shipped

| Release | Contents |
| --- | --- |
| **v0.40.2** | #573 (PR #577), #574 (PR #578) |
| **v0.40.3** | #377 (PR #584), #413 (PR #582) |

Both published live with the chart `.tgz` attached and all three artifact
digests resolved — the check that exists because 0.5.1 and 0.6.0 once went
live with two images and no chart.

## The one thing that went wrong, and is not recoverable

**#577 is absent from the v0.40.2 release notes and always will be.**
release-please could not parse its squash body — `unexpected token '(' at
42:22`, the nested parentheses in a Gomega expression quoted inside a
`* type(scope):` bullet — so it discarded the whole commit, reported
`commits: 0`, and exited 0 with a green tick. `hack/check-commit-message.sh`
validates subjects only, so nothing caught it; the pull request title and
every commit subject conformed. **#579** carries the fix and the reasoning.
The maintainer chose to ship as-is rather than revert and re-land.

Mitigation in force until #579 lands: **every squash body is written by hand
at merge time**, never left to GitHub's `* <subject>` concatenation, and every
Release run's log is read for `could not be parsed` / `commits: 0` rather than
trusted on its green tick. Verified working — the three merges after #577
parsed 3, 6 and 6 commits respectively.

## Open, and why

- **#573 stays open on purpose.** PR #577 says `Refs`, not `Closes`. The
  redirect it scoped was wrong on its own terms, but the reporter's own two
  probes — a `301` on the base domain and a bare `404` on the custom domain in
  the same minute — prove something more specific already owned that vhost on
  :80, so the fix may not be what unsticks their domain. The disambiguator is
  `kubectl get challenge -A -o wide`: `wrong status code '301', expected '200'`
  means the redirect was the cause; anything else means the deadlock is
  elsewhere.
- **#445 was never dispatched**, blocked on D20 below.
- Filed during the run and open: **#579** (the parser trap), **#581** (the
  `redis` claim type has no docs subsection), **#576** (`/metrics/overview`
  still counts edge-answered requests), **#583** (migrate off the apply patch
  deprecated in controller-runtime 0.23).

## Decisions still the maintainer's

- **D20 (#445)** — leave artifacts already signed `strategy: auto` alone
  (recommended: a signed record is a record of what was signed, and re-signing
  makes the attestation store no longer append-only), or re-state them. This
  blocks #445.
- **D21** — which of #370, #519, #554, #481 are wanted at all. None is the
  quick bugfix the batch was for: #370 is a backend-for-frontend (new session
  definition, CSRF everywhere, `docs/AUTH.md` rewritten), #519 needs a CRD
  field `Project` does not have, #554 is a `feat` by its own title, #481 is a
  1070-line view extraction with an unresolved authorization question.
  Recommended: #554 next, #370 parked behind #321's CSP.
- **D22 (#554)** fold `status.systemLogs` into `GET /platform/storage` or give
  it its own route. **D23 (#519)** CRD field on `Project` or API-only.
  **D24 (#481)** borrow `GET /platform/signals` as the screen's `requires` or
  add a route.
- D13 (#499), D14 (#497), D15 (#496) carried from 8 September, still open.

## Needs the maintainer's hands

- A **stray untagged draft release object for `v0.23.1`**, sitting since
  3 September (`untagged-528ff84108d181afd78f`) — a publish that never
  finished, from before the draft/finalize ordering was fixed. `v0.23.1` was
  released properly afterwards, so this is litter to delete in the UI.

## Decisions taken on the maintainer's behalf

- **D16 (#377) `fix(deps)`**, not `build`/`chore`: it ships a different
  operator binary, and a change worth shipping typed `chore` does not ship.
- **D17 (#564)** sweep all 11 SIGPIPE sites rather than the one failing step —
  not reached this run.
- **D18 (#547)** adopt #546's field names verbatim — not reached this run.
- **D19 (#481)** extract the log surface rather than copy the view — not
  reached.
- **#377's rate limiter**: controller-runtime 0.21 turned the client-side
  limiter off by default. Both long-lived binaries restore 20 QPS / burst 30,
  so an installation behaves afterwards as it did before. Upstream documents
  that setting as the supported way to re-enable it, so this is not fighting
  the framework. The alternative — take the new default and rely on API
  Priority and Fairness — is a three-line deletion.

## Two things worth remembering about the day

- A **Go module mirror incident** (`proxy.golang.org`, `sum.golang.org`,
  `stream error … INTERNAL_ERROR; received from peer`) caused five spurious CI
  failures between roughly 15:10 and 16:35, including a `golangci-lint` job on
  `main` that died inside `go install golangci-lint` without linting a line.
  None was ever the branch's fault. Read the log before blaming the diff.
- **release-please reuses the branch but not the pull request number.** #570
  was 0.40.2; 0.40.3 was #580 on the same branch. A merge call against the old
  number is a no-op that echoes the old merge SHA, which reads like success.
