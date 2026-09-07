# Implementer memory

What previous implementers of bermos/kitchen learned the hard way. One line per
lesson, dated, under the heading it belongs to. Merge duplicates; do not drop.

## Test blind spots

- 2026-09-07: The controller-runtime fake client admits objects into namespaces that do not exist. A first-provision ordering bug (a Certificate created before `ensureNamespace`) passed every unit test and failed only on kind. Pin ordering with a fake-client `interceptor` recording Create calls, and prefer a kind e2e case for anything that creates objects into a namespace it also creates.
- 2026-09-07: The CNB lifecycle (0.21.x) builds a buildpack's environment from an include list and **drops every other container variable**; `BP_*` and `NODE_OPTIONS` reach a buildpack only as files under the platform directory (`-platform`, `detector` and `builder` only). An assertion on a pod's container env proved nothing for months. The operator now writes a ConfigMap per buildpacks Job mounted at `/kitchen/platform/env`; assert on that, not on `env`.
- 2026-09-07: A kind e2e job with a component disabled (`--set cert-manager.enabled=false`) turns every path that needs it into dead code there; its green runs said nothing about that path. Check what the job disables before trusting its coverage.
- 2026-09-06: `Seal` in `internal/audit` stamps `time.Now()` when the timestamp is zero, so a "deterministic encoding" test that did not pin the instant flaked at millisecond boundaries. Pin timestamps in any test that seals, hashes or fingerprints.
- 2026-09-07: `kubectl get resourceclaim` resolves to Kubernetes 1.34's built-in `resource.k8s.io` ResourceClaim, not Kitchen's CRD. Always write `resourceclaims.kitchen.bermos.dev` in workflows and docs; a loop that swallowed the error with `|| true` hid this for the whole of a fifteen-minute wait.

## Tooling and environment

- 2026-09-06: Run `make` with `LOCALBIN=<main checkout>/bin` from a worktree, or it downloads envtest, controller-gen and golangci-lint again. Symlink `ui/node_modules` rather than `npm ci`.
- 2026-09-06: Auth service tests need a local Postgres (`pg_ctlcluster 16 main start`; `KITCHEN_AUTH_TEST_DATABASE_URL=postgres://postgres:postgres@127.0.0.1:5432/kitchen_auth_test`). The ClickHouse integration suite needs Docker (`dockerd &`) and `KITCHEN_CLICKHOUSE_URL` against the pinned `clickhouse/clickhouse-server` image; say in the PR body if you could not run it.
- 2026-09-06: The Go build cache fills the disk over a long session; `find ~/.cache/go-build -type f -amin +120 -delete` and `docker system prune -af --volumes` recover it.
- 2026-09-07: `make lint` catches what `make test` does not: goconst on a string repeated across test assertions (`"dir"`, a release name), gocyclo at 30 on `Reconcile` (extract a helper), unparam on an always-nil return.

## Rebase and merge

- 2026-09-07: After `git rebase origin/main`, build before trusting the tree: `main` had since exported `ConditionReady` and the branch redeclared it. Regenerate generated files after any rebase; the merge driver keeps one side and a stash pop drops deepcopy and the chart CRDs on purpose.
- 2026-09-06: Files that conflict between concurrent branches: `docs/api/claims.md`, `docs/api/environments.md`, `internal/cli/internals_test.go` (both sides add tests; keep both), `.github/workflows/test-e2e.yml` (both add a case to the same job). Expect and resolve, never hand-merge a generated file.
- 2026-09-06: `main` refuses merge commits on push (`GH013`). Catch up with `git fetch origin main && git rebase origin/main` and force-push with lease on your own branch only.

## CI behaviour

- 2026-09-07: Re-running a workflow run reuses the merge snapshot it was created from; after `main` moves, only a new head gives a fresh run. Re-run only for a setup-step rate limit, a Docker Hub 5xx, or a runner lost before any test body ran, and at most once.
- 2026-09-07: `enable_pr_auto_merge` is refused while any check is failing, required or not ("unstable"). Kind jobs take 15–25 minutes; wait with one bounded loop, not three-minute polls.
- 2026-09-07: The required checks are: Build/typecheck/unit tests, Chart lint and render, E2E on kind, Image build, Typecheck and integration tests, Chart install on kind (the aggregate over the kind and Cilium legs), golangci-lint, Unit and envtest, Conventional Commits, Several workloads on kind. Two PRs merged with the last one red before it was required; do not assume a merged PR's e2e passed.

## Design rules that bit

- 2026-09-06: A claim type is an enum value plus a reconcile path; a route is a policy row plus a docs row plus a screen plus a CLI decision. Reviewers check the chain; the docs rows and the screen are the links tests cannot.
- 2026-09-06: A credential never leaves `kitchen-system` as a key; only a certificate is copied to application namespaces. A ClusterIssuer resolves its Secret from cert-manager's cluster-resource namespace, which must be `kitchen-system` (documented for `cert-manager.enabled=false`).
- 2026-09-06: The API never echoes a credential; `kitchen env set` sends names and only the changed values for the same reason.
- 2026-09-07: `KITCHEN_URL`-style values are derived, not read from status, because status is written at the end of the reconcile that needs them.
