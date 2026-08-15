#!/usr/bin/env bash
# Check one commit message against Conventional Commits v1.0.0
# (https://www.conventionalcommits.org/en/v1.0.0/).
#
# The message is read from the file named as the first argument, or from stdin
# when that is `-` or absent. `--label <text>` names the thing being checked in
# any error message, so CI can say which commit it means.
#
#   hack/check-commit-message.sh .git/COMMIT_EDITMSG
#   git log -1 --format=%B "${sha}" | hack/check-commit-message.sh - --label "commit ${sha}"
#
# Three callers share it, which is the point: the commit-msg hook installed by
# `make hooks`, the Commits workflow over every commit in a pull request, and
# the same workflow over the pull request *title* — squash-merging makes that
# title the subject of the commit that lands on main, so it is the one that
# release-please reads.
set -euo pipefail

# The types release-please knows about. Anything outside this list has no
# changelog section and no bump rule, so it is rejected rather than silently
# dropped from the release notes.
TYPES=(build chore ci docs feat fix perf refactor revert style test)

# commitlint's default, and about where `git log --oneline` stops being
# readable in a terminal.
MAX_HEADER=100

label="commit message"
source=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --label)
      label="${2:-}"
      shift 2
      ;;
    -h | --help)
      sed -n '2,14p' "${BASH_SOURCE[0]}" | sed 's/^# \?//'
      exit 0
      ;;
    -)
      source="-"
      shift
      ;;
    *)
      source="$1"
      shift
      ;;
  esac
done

if [[ -z "${source}" || "${source}" == "-" ]]; then
  message="$(cat)"
else
  message="$(cat "${source}")"
fi

# Everything git itself adds to the buffer: the instructions it comments out,
# and the diff `commit --verbose` appends below the scissors line.
message="$(printf '%s\n' "${message}" | sed '/^# -\+ >8 -\+$/,$d' | grep -v '^#' || true)"

# Leading blank lines would otherwise make the header look empty.
while IFS= read -r line; do
  [[ -n "${line}" ]] && break
done <<<"${message}"
header="${line:-}"

types_pattern="$(
  IFS='|'
  echo "${TYPES[*]}"
)"

fail() {
  echo "error: ${label} is not a Conventional Commit." >&2
  echo >&2
  echo "  ${header}" >&2
  echo >&2
  echo "  $1" >&2
  echo >&2
  echo "Expected: <type>[(scope)][!]: <description>" >&2
  echo "Types:    ${TYPES[*]}" >&2
  echo "  feat / fix bump the version; ! or a BREAKING CHANGE: footer bumps it further." >&2
  echo "  See https://www.conventionalcommits.org/en/v1.0.0/ and CONTRIBUTING.md." >&2
  exit 1
}

if [[ -z "${header}" ]]; then
  fail "The message is empty."
fi

# Git writes these itself, or writes them for a rebase to consume. None of them
# is a change anyone authored, so none of them is described by the spec.
if [[ "${header}" =~ ^(Merge|Revert\ \"|fixup!|squash!|amend!) ]]; then
  exit 0
fi

if ! [[ "${header}" =~ ^(${types_pattern})(\([^()]+\))?(!)?: ]]; then
  if [[ "${header}" =~ ^([a-zA-Z]+)(\([^()]+\))?(!)?: ]]; then
    fail "'${BASH_REMATCH[1]}' is not one of the allowed types."
  fi
  fail "The subject has no '<type>: ' prefix."
fi

description="${header#*: }"
if [[ -z "${description// /}" ]]; then
  fail "The description after the colon is empty."
fi
if [[ "${header}" =~ ^[^:]+:[^\ ] ]]; then
  fail "The colon needs a space after it."
fi
if [[ "${description}" == *. ]]; then
  fail "The description ends with a full stop; drop it."
fi
if ((${#header} > MAX_HEADER)); then
  fail "The subject is ${#header} characters, over the ${MAX_HEADER} allowed."
fi

# A body has to be separated from the subject by a blank line, or git and
# every tool reading it treat the whole thing as one runaway subject.
second="$(printf '%s\n' "${message}" | sed -n '2p')"
if [[ -n "${second}" ]]; then
  fail "Line 2 must be blank — the body starts on line 3."
fi

# The spec spells the footer in capitals, with either a space or a hyphen. A
# lowercase one reads as intended but silently loses the major/minor bump.
if printf '%s\n' "${message}" | grep -qiE '^breaking[ -]change:' &&
  ! printf '%s\n' "${message}" | grep -qE '^BREAKING[ -]CHANGE:'; then
  fail "A breaking-change footer has to read 'BREAKING CHANGE:' in capitals."
fi

exit 0
