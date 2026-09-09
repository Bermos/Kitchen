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

## Wave 2 — mapping in flight

`dependency-mapper` is sizing #377, #547, #554, #564, #370, #413, #518, #519,
#481, #445. #377 is on the maintainer's "quick" list but the issue text itself
says it touches the operator's whole surface; the mapper is asked to say so
bluntly if it is not a quick bugfix.

## Needs the maintainer's eye

- A **draft release object `v0.23.1`** has been sitting untagged since
  3 September (`untagged-528ff84108d181afd78f`) — a publish that never
  finished, from before the draft/finalize ordering was fixed. `v0.23.1` was
  released properly later, so this is a stray object to delete by hand.

## Decisions open

- D13 (#499), D14 (#497), D15 (#496) — carried from 8 September, still open,
  not blocking anything in this run.
