#!/usr/bin/env bash
# Regenerate the files hack/merge-generated.sh refused to merge.
#
# That driver resolves the generated files in .gitattributes to one side rather
# than interleaving two outputs, and leaves them to be regenerated from the
# merged sources. This is that half. It is called from two hooks because git
# reports the two ways of catching up with main through different ones:
# post-merge for a merge, post-rewrite for a rebase — and a rebase is what this
# repository asks for, so post-merge alone would almost never have fired.
#
# On a rebase the side the driver keeps is the *upstream's*, since that is what
# "ours" means once your commits are being replayed onto it. So after a rebase
# the generated files reflect main and not your branch at all until this runs.
#
# It never fails the operation it is called from. A tree with no toolchain
# cannot have run the generators anyway; `make test` regenerates them too, and
# CI fails on the difference, so this is the convenience and not the guarantee.
set -uo pipefail

repo_root="$(git rev-parse --show-toplevel 2>/dev/null)" || exit 0
cd "${repo_root}" || exit 0

generated=(
  api/v1alpha1/zz_generated.deepcopy.go
  config/crd/bases
  config/rbac/role.yaml
  charts/kitchen/templates/crds.yaml
  charts/kitchen/templates/manager-clusterrole.yaml
  ui/src/lib/policy.generated.ts
)

# Only when the operation actually moved one of them. ORIG_HEAD is the branch
# tip before the merge or the rebase in both cases, so the same range works.
if ! git diff --name-only ORIG_HEAD HEAD -- "${generated[@]}" 2>/dev/null | grep -q .; then
  exit 0
fi

if ! command -v make >/dev/null 2>&1 || ! command -v go >/dev/null 2>&1; then
  echo "regenerate: generated files moved, but make or go is missing here. Run" >&2
  echo "            'make manifests generate helm-manifests ui-policy' where you can." >&2
  exit 0
fi

echo "regenerate: rebuilding the generated files from the merged sources"
if ! make manifests generate helm-manifests ui-policy; then
  echo "regenerate: failed. Run 'make manifests generate helm-manifests ui-policy'" >&2
  echo "            by hand and commit the result." >&2
  exit 0
fi

if ! git diff --quiet -- "${generated[@]}"; then
  echo
  echo "regenerate: the generated files moved. Review and commit them:"
  git diff --stat -- "${generated[@]}"
fi
exit 0
