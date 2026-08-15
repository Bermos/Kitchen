#!/usr/bin/env bash
# Extract the cert-manager CRDs the envtest suite needs from the cert-manager
# sub-chart the platform already vendors, and write them to test/crd/cert-manager.
#
# The operator creates a ClusterIssuer and a Certificate as unstructured
# objects, so nothing in the build knows their schema: only a real CRD makes an
# envtest API server prune a misspelled field, which is what turns a typo in
# those specs into a failing assertion rather than a passing round-trip.
#
# The schemas come from charts/kitchen/charts/cert-manager-*.tgz rather than
# from the internet, so the tests always run against the version the chart
# installs. Re-run this after bumping the cert-manager dependency.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
out_dir="${repo_root}/test/crd/cert-manager"

chart_tgz=$(ls "${repo_root}"/charts/kitchen/charts/cert-manager-*.tgz 2>/dev/null | head -1)
if [[ -z "${chart_tgz}" ]]; then
  echo "error: no vendored cert-manager chart in charts/kitchen/charts" >&2
  echo "       run 'helm dependency update charts/kitchen' first" >&2
  exit 1
fi
chart_version="$(basename "${chart_tgz}" .tgz | sed 's/^cert-manager-//')"

work_dir="$(mktemp -d)"
trap 'rm -rf "${work_dir}"' EXIT
tar -xzf "${chart_tgz}" -C "${work_dir}"

mkdir -p "${out_dir}"

# The chart's CRDs are Helm templates: an install-time conditional around the
# whole document, and a metadata header carrying the chart's own labels and
# resource policy. Both are stripped — the header is rebuilt as plain YAML,
# and the schema below `spec:` is copied verbatim.
for kind in certificates clusterissuers; do
  src="${work_dir}/cert-manager/templates/crd-cert-manager.io_${kind}.yaml"
  if [[ ! -f "${src}" ]]; then
    echo "error: ${src##*/} is not in cert-manager ${chart_version}" >&2
    exit 1
  fi

  {
    echo "# cert-manager.io/${kind}, extracted from the cert-manager ${chart_version}"
    echo "# sub-chart by hack/gen-test-crds.sh. DO NOT EDIT."
    awk '
      # Everything before the schema is the chart-templated metadata header.
      /^spec:/ { body = 1 }
      !body {
        if ($0 ~ /{{/) next                              # Helm directives
        if ($0 ~ /^  annotations:$/) next                # only holds the policy
        if ($0 ~ /^    helm\.sh\/resource-policy: keep$/) next
        if ($0 ~ /^  labels:$/) next                     # only holds the include
      }
      body && $0 ~ /{{/ { next }                         # the closing conditional
      { print }
    ' "${src}"
  } >"${out_dir}/cert-manager.io_${kind}.yaml"

  echo "wrote test/crd/cert-manager/cert-manager.io_${kind}.yaml (cert-manager ${chart_version})"
done
