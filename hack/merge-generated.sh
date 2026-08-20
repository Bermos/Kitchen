#!/usr/bin/env bash
# The merge driver for the generated files listed in .gitattributes.
#
# Git calls this whenever both sides of a merge have touched one of them. It
# does not try to reconcile the two outputs: it keeps the side being merged
# into and leaves the file to be regenerated from the merged sources, which
# hack/hooks/post-merge does immediately afterwards.
#
# Resolving to either side is safe *because* of that regeneration — the content
# this leaves behind is discarded. What it must not do is fail: a conflict here
# stops the merge over a file no one edits by hand.
#
#   driver = ./hack/merge-generated.sh %O %A %B %P
#
# %A is both our side and the result git reads back, so keeping ours is doing
# nothing at all. Installed by `make hooks`.
set -uo pipefail

path="${4:-a generated file}"

echo "merge: keeping the current side of ${path}; it is generated and will be" >&2
echo "       regenerated from the merged sources (make manifests generate \\" >&2
echo "       helm-manifests ui-policy)." >&2

exit 0
