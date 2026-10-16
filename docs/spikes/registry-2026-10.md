# Spike: is zot still the right bundled registry?

*October 2026. This re-examines the choice of [zot](https://zotregistry.dev/)
as the registry the chart ships, after per-project accounts (#637) found a
cross-project leak in it. The only platform change is the zot version bump
that came with it.*

The answer, before the working:

- **Keep zot, and run 2.1.21 or later.** The leak that prompted the question
  is real in 2.1.20, the version the chart pinned, and gone from 2.1.21 on.
  The chart now pins 2.1.22, the latest release, and its registry check tries
  the leak on every run.
- **Distribution 3.1.2 isolates projects at least as well, and by design**
  rather than by a recent fix. But it reclaims disk only while stopped or
  read-only. Moving to it would mean the operator taking over retention and a
  scheduled maintenance window. That is the reason zot was chosen, and it
  still holds.
- **Harbor 2.15.4 is a platform, not a registry.** It would replace the token
  service and the per-project accounts with its own, and bring a database, a
  cache and several services to run beside Kitchen's. It answers questions
  Kitchen already answers.

## What zot has cost so far

| What happened | zot's doing? | Where it was settled |
|---|---|---|
| Every build failed at the last step with `415 Unsupported Media Type` | Yes: zot is OCI-native and refuses Docker schema 2 unless told otherwise | `http.compat: ["docker2s2"]`, guarded by the chart's registry check |
| Accounts could not follow projects: htpasswd and access control are files read at startup | Yes | #637: zot trusts tokens from the operator's token service instead |
| One project's account could read another project's layer by digest | Yes, up to 2.1.20 | This change: 2.1.22, and the check below |
| The volume filled, and pushes failed as "blob upload unknown" | No: retention kept more than the volume held. Any registry fills the same way | #633 explains the failure on the Build; #634 warns from 76.5% used |

Two of the four were zot's, and both have been settled. The token service
from #637 is not zot-specific either. It is the Docker token flow, which
Distribution speaks too, so it would move with the platform.

## The test

Two projects, `alpha` and `beta`. The platform's account pushes a layer to
`beta` that nothing else holds. Then `alpha`'s push account, with a token that
names `alpha` and nothing else, tries to reach that layer:

| `alpha`'s token asks for `beta`'s layer… | zot 2.1.20 | zot 2.1.22 | Distribution 3.1.2 |
|---|---|---|---|
| `GET /v2/alpha/blobs/<digest>` | 404 | 404 | 404 |
| `HEAD /v2/alpha/blobs/<digest>` | **200** | 404 | 404 |
| `POST /v2/alpha/blobs/uploads/?mount=<digest>&from=beta` | **201, mounted** | 202, ordinary upload | 401 |
| `GET /v2/alpha/blobs/<digest>` after that | **200, the layer** | 404 | 404 |
| `GET /v2/beta/blobs/<digest>` | 401 | 401 | 401 |
| `GET /v2/alpha/manifests/<beta's tag>` | 404 | 404 | 404 |

Every registry ran with token authentication against the same RSA key, and
zot ran with deduplication on, as the chart configures it. Distribution only
accepts a token whose `kid` is the RFC 7638 thumbprint of a key in its
`rootcertbundle`, and it checks `iss`. zot checks neither. The operator's
token service would need that `kid` before Distribution would accept it.

`hack/registry-push-check.py` now makes the last three of these attempts as
part of `check_isolation`. Run against the chart's rendered configuration, it
fails on 2.1.20 and passes on 2.1.21 and 2.1.22:

```text
FAIL alpha's account reads beta's layer through alpha: yes
FAIL alpha's account mounts beta's layer into alpha: yes
FAIL alpha's account reads beta's layer after trying: yes
```

## The three candidates

| | zot 2.1.22 | Distribution 3.1.2 | Harbor 2.15.4 |
|---|---|---|---|
| Reclaims disk while serving | Yes: online GC on an interval | No: `garbage-collect` needs the registry stopped or `storage.maintenance.readonly` on | Yes, inside a read-only window it schedules |
| Tag retention | Built in: keep N, keep anything pushed within a window | None: deleting tags is the operator's job | Built in, per project |
| Project isolation | By the token's scope; layers too since 2.1.22 | By the token's scope; layers are linked per repository | Projects and robot accounts of its own |
| Fits the token service from #637 | As it is | Needs the RFC 7638 `kid` | Replaces it |
| Docker schema 2 | With `http.compat` | Native | Native |
| What runs | One binary, one volume | One binary, one volume | Core, job service, registry, registry controller, portal, PostgreSQL, Redis (from memory of its chart, not counted here) |

### Distribution, and what moving to it would take

Distribution is the conservative choice. It is what `registry:3` runs, most of
the ecosystem tests against it, and its per-repository blob links mean the
isolation above does not depend on a recent fix. It costs two pieces of work,
and neither is small:

- **The operator owns retention.** Nothing in Distribution deletes a tag. The
  operator would delete manifests through the API. It could choose better than
  zot does, by keeping exactly what a Release still references, but that is
  new code with a failure mode of its own: deleting what an environment is
  running.
- **Reclaiming disk means a maintenance window.** Deleting a manifest frees
  nothing until `garbage-collect` runs, and it runs against a registry that
  accepts no writes. That means a Job that switches the StatefulSet to
  read-only, runs the collection and switches it back, with every build that
  lands in the window failing its push. The disk-full incident (#633) is the
  failure this would make more likely, not less.

### Harbor

Harbor would make per-project accounts native (projects, robot accounts,
quotas) and bring retention, replication and Trivy scanning. Kitchen already
has the first two through #637 and its own gates and scans, and Kitchen
already owns the question of who may see which project. Running Harbor would
mean a second source of truth for both. It would also be the heaviest thing
in the chart by a distance, for a registry that, in an installation of this
size, holds a few projects' last twenty tags.

## When to look again

- **A zot release reopens the isolation check.** CI fails the bump, and the
  question becomes whether to hold the pin or move.
- **zot's GC or retention loses data or fails to reclaim it.** These are the
  two features zot was chosen for. If they fail, nothing is left in its
  favour.
- **The operator grows release-aware retention anyway**, for example to keep
  every image a running environment needs whatever its age. Then
  Distribution's one real cost is half paid, and the trade is worth
  recomputing.

## Repeating it

The registry check, against the image the chart pins:

```sh
hack/check-registry-accepts-docker-manifests.sh
```

The Distribution column came from the release binary
(`registry_3.1.2_linux_amd64.tar.gz`) with `auth.token` pointed at a
self-signed certificate. The requests above were made with tokens minted
directly, with the `kid` as described.
