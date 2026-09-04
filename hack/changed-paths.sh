#!/usr/bin/env bash
# Decide whether the kind jobs have anything to check in this pull request, and
# print `run=true` or `run=false` for a workflow to read.
#
# The four kind jobs — Chart install on kind, E2E on kind, Several workloads on
# kind, Gateway L7 flows on kind — cost twelve to fourteen minutes each and sit
# on the critical path of every merge. Most pull requests cannot affect all of
# them: a CLI change reaches no cluster at all, and the Hubble job installs no
# Kitchen, so nothing in this repository except its own script can change what
# it proves. There are three profiles rather than four because the two jobs in
# the E2E workflow share one.
#
# This is a job-level gate, not a `paths:` filter on the workflow, and the
# difference decides whether a pull request can merge. A skipped job still
# reports its check with conclusion `skipped`, which branch protection accepts;
# a workflow filtered out by `paths:` never runs, so a check required on main
# never reports and the pull request waits on it forever. See CONTRIBUTING.md,
# "Skipped is not missing".
#
# The lists below are what a profile may *ignore*, never what it needs. A path
# nobody has classified runs the job, so adding a directory cannot silently
# switch a gate off — the failure mode is a wasted twelve minutes, which is
# recoverable, rather than an unchecked merge, which is not.
#
#   hack/changed-paths.sh chart|e2e|hubble [base-ref]
#
# Run it against a branch to see what CI will decide:
#   ./hack/changed-paths.sh chart origin/main
set -euo pipefail

profile="${1:?usage: changed-paths.sh chart|e2e|hubble [base-ref]}"
base_ref="${2:-origin/main}"

# Every profile ignores the same things: prose, and the files that carry no
# behaviour at all. `*.md` matches at any depth here — a case pattern is not
# pathname expansion, so its `*` crosses slashes.
common_ignores=(
  '*.md'
  'docs/*'
  'LICENSE'
  '.gitignore'
  '.gitattributes'
  '.editorconfig'
  '.github/ISSUE_TEMPLATE/*'
  '.github/pull_request_template.md'
)

# The chart install job builds both images, installs the chart on kind and
# asserts what the platform does over HTTP. It never opens a dashboard page, so
# `ui/` reaches it only through the image build — and a dashboard that does not
# build fails the Dashboard workflow's `npm run build` in forty seconds, which
# is a required check of its own. The CLI holds no kubeconfig and is not
# installed by the chart.
chart_ignores=(
  'internal/cli/*'
  'cmd/kitchen/*'
  'ui/*'
  'test/*'
  '.github/workflows/test.yml'
  '.github/workflows/lint.yml'
  '.github/workflows/ui.yml'
  '.github/workflows/auth.yml'
  '.github/workflows/commits.yml'
  '.github/workflows/test-e2e.yml'
  '.github/workflows/hubble.yml'
  '.github/workflows/release.yml'
  '.github/workflows/publish.yml'
)

# The E2E workflow's two kind jobs, under one profile. One runs the operator's
# own suite (test/e2e) against kind; the other installs the chart and deploys a
# project of several workloads through a real build, which is why `charts/*` is
# *not* ignorable here even though the first job installs no chart. `test/` very
# much can change what both prove, which is why it is absent here and present
# above. Neither runs the auth service.
e2e_ignores=(
  'internal/cli/*'
  'cmd/kitchen/*'
  'ui/*'
  'auth/*'
  '.github/workflows/test.yml'
  '.github/workflows/lint.yml'
  '.github/workflows/ui.yml'
  '.github/workflows/auth.yml'
  '.github/workflows/commits.yml'
  '.github/workflows/helm.yml'
  '.github/workflows/hubble.yml'
  '.github/workflows/release.yml'
  '.github/workflows/publish.yml'
)

# The Hubble job is the odd one, and its own header says why: "it is not a job
# in the Helm workflow because it is not about the chart: it installs no
# Kitchen, and what it tests is the platform's CNI". What it proves is that
# Cilium's Envoy emits L7 flow records. Only its scripts, its fixtures and the
# two workflows that pin Cilium's version can change that answer — so almost
# everything in this tree is ignorable here, listed rather than assumed.
hubble_ignores=(
  'api/*'
  'cmd/*'
  'internal/*'
  'charts/*'
  'config/*'
  'ui/*'
  'auth/*'
  'test/crd/*'
  'test/e2e/*'
  'test/utils/*'
  'Dockerfile'
  'Dockerfile.*'
  'go.mod'
  'go.sum'
  'Makefile'
  'PROJECT'
  '.golangci.yml'
  'release-please-config.json'
  '.release-please-manifest.json'
  'hack/boilerplate.go.txt'
  'hack/check-commit-message.sh'
  'hack/gen-helm-manifests.sh'
  'hack/gen-test-crds.sh'
  'hack/gen-ui-policy/*'
  'hack/hooks/*'
  '.github/workflows/test.yml'
  '.github/workflows/lint.yml'
  '.github/workflows/ui.yml'
  '.github/workflows/auth.yml'
  '.github/workflows/commits.yml'
  '.github/workflows/test-e2e.yml'
  '.github/workflows/release.yml'
  '.github/workflows/publish.yml'
)

case "${profile}" in
  chart) ignores=("${common_ignores[@]}" "${chart_ignores[@]}") ;;
  e2e) ignores=("${common_ignores[@]}" "${e2e_ignores[@]}") ;;
  hubble) ignores=("${common_ignores[@]}" "${hubble_ignores[@]}") ;;
  *)
    echo "unknown profile '${profile}': expected chart, e2e or hubble" >&2
    exit 2
    ;;
esac

base="$(git merge-base "${base_ref}" HEAD)" || {
  echo "::warning::cannot find a merge base with ${base_ref}; running the job" >&2
  echo "run=true"
  exit 0
}

changed="$(git diff --name-only "${base}" HEAD)"
if [ -z "${changed}" ]; then
  echo "no files changed against ${base_ref}; skipping" >&2
  echo "run=false"
  exit 0
fi

decides=""
while IFS= read -r file; do
  [ -n "${file}" ] || continue
  ignored=false
  for pattern in "${ignores[@]}"; do
    # shellcheck disable=SC2254 # the pattern is meant to glob
    case "${file}" in
      ${pattern}) ignored=true; break ;;
    esac
  done
  if [ "${ignored}" = false ]; then
    decides="${file}"
    break
  fi
done <<<"${changed}"

if [ -n "${decides}" ]; then
  echo "${profile}: running — ${decides} is not ignorable" >&2
  echo "run=true"
else
  echo "${profile}: skipping — every changed file is ignorable for this job" >&2
  echo "run=false"
fi
