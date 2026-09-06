#!/usr/bin/env bash
# Re-resolve every image this repository pins and say whether the tag still
# points at the digest written beside it.
#
# The pins are `repository:tag@sha256:…` — the tag for a person to read, the
# digest for the kubelet to resolve (see internal/controller/images.go). The
# two can only ever disagree in one direction: upstream republishes the tag,
# and the comment beside the digest starts describing an image nobody runs.
# Nothing breaks when that happens, which is exactly why it needs asking
# about rather than waiting for.
#
# It reaches the registry, so it is **run by hand** and not by CI: whether a
# tag has moved is a fact about somebody else's repository, not about the
# change under test, and a job that went red for it would be red on branches
# that touched nothing. The half CI does hold is the other one —
# TestEveryImageConstantNamesADigest refuses a pin with no digest at all.
#
#   hack/check-image-pins.sh            # every pin, against its registry
#
# Exit status is 0 when every pin still agrees, 1 when one has moved (the new
# digest is printed, which is what to paste in), and 2 when a pin could not be
# resolved at all — a repository that is gone, or no network.
#
# It speaks the registry's HTTP API with curl rather than depending on a
# client, so it needs nothing that is not already here. `crane digest <ref>`
# and `skopeo inspect --format '{{.Digest}}' docker://<ref>` answer the same
# question one image at a time.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="${ROOT:-${HERE}/..}"

for tool in curl jq; do
  command -v "${tool}" >/dev/null 2>&1 || {
    echo "hack/check-image-pins.sh needs ${tool}" >&2
    exit 2
  }
done

# The manifest types worth asking for, index first: what a pin should name is
# the multi-arch index, so that a cluster of mixed nodes can pull it.
ACCEPT=(
  -H 'Accept: application/vnd.oci.image.index.v1+json'
  -H 'Accept: application/vnd.docker.distribution.manifest.list.v2+json'
  -H 'Accept: application/vnd.oci.image.manifest.v1+json'
  -H 'Accept: application/vnd.docker.distribution.manifest.v2+json'
)

# registry_host <repository> — where the repository lives, and what it is
# called there. A first component carrying a dot or a port is a registry;
# anything else is Docker Hub, where a bare name lives under library/.
registry_host() {
  local repository="$1" first="${1%%/*}"
  if [ "${first}" != "${repository}" ] && [[ "${first}" == *.* || "${first}" == *:* ]]; then
    printf '%s %s\n' "${first}" "${repository#*/}"
    return
  fi
  case "${repository}" in
    */*) printf 'registry-1.docker.io %s\n' "${repository}" ;;
    *) printf 'registry-1.docker.io library/%s\n' "${repository}" ;;
  esac
}

# token_from <challenge> <path> — the anonymous pull token a `401` asked for.
# Nothing here reads a private repository: an image the platform pulls
# unauthenticated is an image this can resolve unauthenticated.
token_from() {
  local challenge="$1" path="$2" realm service
  case "${challenge}" in
    Bearer*) ;;
    *) return 0 ;;
  esac
  realm="$(printf '%s' "${challenge}" | sed -n 's/.*realm="\([^"]*\)".*/\1/p')"
  service="$(printf '%s' "${challenge}" | sed -n 's/.*service="\([^"]*\)".*/\1/p')"
  [ -n "${realm}" ] || return 0
  curl -sS --get --data-urlencode "service=${service}" \
    --data-urlencode "scope=repository:${path}:pull" "${realm}" |
    jq -r '.token // .access_token // empty'
}

# digest_of <host> <path> <tag> — what the tag points at today.
#
# One request where the registry serves anonymously, two where it challenges,
# and a retry on the rate limit Docker Hub applies per address: an anonymous
# pull budget spent by something else is not an answer about the pin.
digest_of() {
  local host="$1" path="$2" tag="$3" headers token digest attempt
  headers="$(mktemp)"
  for attempt in 1 2 3; do
    : >"${headers}"
    curl -sS --head -o /dev/null -D "${headers}" "${ACCEPT[@]}" \
      ${token:+-H "Authorization: Bearer ${token}"} \
      "https://${host}/v2/${path}/manifests/${tag}" 2>/dev/null
    digest="$(tr -d '\r' <"${headers}" | awk 'tolower($1)=="docker-content-digest:"{print $2; exit}')"
    if [ -n "${digest}" ]; then
      rm -f "${headers}"
      printf '%s\n' "${digest}"
      return 0
    fi
    if [ -z "${token:-}" ] && grep -qiE '^www-authenticate:' "${headers}"; then
      token="$(token_from "$(tr -d '\r' <"${headers}" |
        awk 'tolower($1)=="www-authenticate:"{ $1=""; print substr($0,2); exit }')" "${path}")"
      [ -n "${token}" ] && continue
    fi
    if grep -qE '^HTTP/[0-9.]+ 429' "${headers}"; then
      sleep $((attempt * 5))
      continue
    fi
    break
  done
  rm -f "${headers}"
  return 1
}

# Every pin in the Go sources, deduplicated. The shape is the one the test
# enforces, so a constant this misses is a constant that test has already
# failed on.
mapfile -t pins < <(
  grep -rhoE '[a-zA-Z0-9][a-zA-Z0-9._/-]*:[a-zA-Z0-9][a-zA-Z0-9._-]*@sha256:[0-9a-f]{64}' \
    --include='*.go' "${ROOT}/internal" | sort -u
)

if [ "${#pins[@]}" -eq 0 ]; then
  echo "no pinned images found under internal/ — has the shape changed?" >&2
  exit 2
fi

status=0
for pin in "${pins[@]}"; do
  reference="${pin%@*}"
  recorded="${pin##*@}"
  repository="${reference%:*}"
  tag="${reference##*:}"
  read -r host path <<<"$(registry_host "${repository}")"

  current="$(digest_of "${host}" "${path}" "${tag}")"
  if [ -z "${current}" ]; then
    echo "UNRESOLVED ${reference} — ${host} answered no digest for the tag"
    [ "${status}" -eq 1 ] || status=2
    continue
  fi
  if [ "${current}" = "${recorded}" ]; then
    echo "ok         ${reference}"
    continue
  fi
  echo "MOVED      ${reference}"
  echo "           recorded ${recorded}"
  echo "           registry ${current}"
  status=1
done

case "${status}" in
  0) echo "every pin still names the digest its tag points at" ;;
  1) echo "a tag has moved: read the release notes, then write the new digest beside it" >&2 ;;
  2) echo "a pin could not be resolved" >&2 ;;
esac
exit "${status}"
