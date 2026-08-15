# Contributing to Kitchen

## Commit messages

Every commit message follows
[Conventional Commits 1.0.0](https://www.conventionalcommits.org/en/v1.0.0/):

```
<type>[(scope)][!]: <description>

[body]

[footers]
```

This is not a style preference. release-please reads these messages to decide
the next version and to write the change notes, so a message outside the spec
is a release that either bumps the wrong digit or silently omits a change.

**Types**, and what each one does to the version and the changelog:

| Type       | Version    | Changelog section       |
| ---------- | ---------- | ----------------------- |
| `feat`     | minor      | Features                |
| `fix`      | patch      | Bug fixes               |
| `perf`     | patch      | Performance             |
| `revert`   | patch      | Reverts                 |
| `refactor` | none       | Refactoring             |
| `docs`     | none       | Documentation           |
| `build`    | none       | Build and dependencies  |
| `ci`       | none       | hidden                  |
| `test`     | none       | hidden                  |
| `style`    | none       | hidden                  |
| `chore`    | none       | hidden                  |

A breaking change is a `!` before the colon, a `BREAKING CHANGE:` footer, or
both. **While Kitchen is on 0.x that bumps the minor**, not the major — 0.4.2
with a breaking change becomes 0.5.0. It is still worth marking: the footer is
what the release notes tell operators to read before upgrading.

Scopes are free-form. The useful ones name the piece that changed — `chart`,
`operator`, `api`, `ui`, `auth`, `docs`, `deps`.

```
feat(ui): show the platform version in the sidebar
fix(chart): keep the namespace annotation on upgrade
feat(api)!: drop the v1alpha1 settings shape

BREAKING CHANGE: clients reading /api/v1/settings must send the new field names.
```

The rest of the rules: no full stop at the end of the description, a blank line
before any body, and a subject under 100 characters.

### Where it is enforced

`hack/check-commit-message.sh` is the single implementation, and three things
run it:

- **Locally**, as a `commit-msg` hook. Install it once:

  ```sh
  make hooks        # points core.hooksPath at hack/hooks
  make check-commits # or check what the branch already has
  ```

- **On every pull request**, over each commit the branch adds.
- **On the pull request title**, because squash-merging makes that title the
  subject of the commit that lands on main — so it, not the branch's history,
  is what release-please reads. Get the title right even if the commits behind
  it are messy.

The Commits workflow is the check to make required on `main`. With it required,
neither a merge nor a direct push can land a message the release tooling cannot
read.

## Cutting a release

Nobody edits a version number by hand, and nobody writes the changelog.

1. Conventional commits land on `main`.
2. [release-please](https://github.com/googleapis/release-please) keeps a
   release pull request open, titled `chore(release): kitchen X.Y.Z`. It works
   out the version from the commits since the last tag, writes `CHANGELOG.md`
   (creating it on the first release — it owns that file and rewrites the top
   of it, so nothing else should), and bumps every file that spells the version
   out —
   `charts/kitchen/Chart.yaml` (both `version` and `appVersion`),
   `ui/package.json` and `auth/package.json` with their lockfiles. The files
   are listed in `release-please-config.json`; a new one goes there, not into a
   release checklist.
3. Review it as a release note. Merging it is the decision to release: it tags
   `vX.Y.Z` and creates the GitHub release from the changelog entry.
4. `.github/workflows/release.yml` then calls the publish workflow, which
   builds and pushes both images and packages and pushes the chart, all
   stamped `X.Y.Z`.

This needs **Settings → Actions → General → Workflow permissions → "Allow
GitHub Actions to create and approve pull requests"** to be on. Without it
release-please does all its work, pushes the release branch, and then fails on
the last step with *"GitHub Actions is not permitted to create or approve pull
requests"* — a red Release run and a branch with no pull request attached to
it. Re-run the workflow after switching it on; the branch is regenerated.

A release pull request appears only when the commits since the last tag
contain something that moves a version — `feat`, `fix`, `perf`, `revert` or a
breaking change. A run of nothing but `ci`, `docs`, `chore`, `refactor`,
`test`, `style` and `build` opens no pull request, because there is no version
to move to. That is the tool working, not a jam.

To release without merging first — a hotfix from a tag, say — push a `vX.Y.Z`
tag or run the Publish workflow by hand with a version. Both paths land in the
same jobs.

### The first automated release

Kitchen was released by hand up to `v0.1.4`, and six commits landed on `main`
after it that were never published. Those commits predate the convention, so
release-please can date them but cannot describe them: it skips a message it
cannot parse, which would put the dashboard's telemetry into a release whose
notes never mention it.

`bootstrap-sha` is therefore pinned to `v0.1.4`'s commit — the range for the
first automated release is exactly "everything not yet published" — and
`.release-please-manifest.json` starts at `0.1.4` to match the newest tag that
exists. A lower baseline would compute a version whose tag is already taken,
and creating it fails.

So the first release pull request covers those six commits plus everything
conventional since, and its `CHANGELOG.md` will list only the latter. **Fill
the gap in by hand on the release branch before merging** — the six are worth
one line each, and that entry is the only one anyone will ever have to write.
Merge it without letting other commits land on `main` first: a new commit makes
release-please regenerate the branch, and the hand-written lines go with it.

After that release, `bootstrap-sha` has done its job and is ignored.

### Why the release workflow calls publish instead of waiting for the tag

A tag pushed with a workflow's own `GITHUB_TOKEN` does not start another
workflow run. So `release.yml` invokes `publish.yml` through `workflow_call`
and hands it the version directly; publish's `push: tags` trigger is left in
place only for a tag a person pushes.

## The version at runtime

`internal/version.Version` holds the release, and the linker sets it — see
`LDFLAGS` in the Makefile and `ARG VERSION` in the Dockerfile. Nothing in the
Go source is bumped for a release.

It reaches the dashboard through `/config.json`, which the operator already
serves for the SPA's issuer and client id, and shows up in the sidebar and on
the settings page. A local `make build` stamps `git describe`, so a
development build identifies itself as what it is rather than claiming a
release.

## Before you push

```sh
make manifests helm-manifests   # if you touched api/ or an RBAC marker
make test                       # also runs go fmt and regenerates deepcopy
make lint                       # CI runs it as its own job; test passing does not imply it
```

See [CLAUDE.md](CLAUDE.md) for the design constraints these commands exist to
protect.
