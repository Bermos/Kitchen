#!/bin/sh
# The unit's deploy task: the schema migration that is not one.
#
# Whether it succeeds is a variable rather than a code path, because what the
# end-to-end case needs is the same task failing for one release and
# succeeding for the next — and the two releases have to run the same image
# for "the previous release is still serving" to mean anything.
set -u

echo "migrating for release ${RELEASE_LABEL:-none}"
if [ "${MIGRATION_FAILS:-0}" = "1" ]; then
  echo "the migration refused to apply"
  exit 1
fi
echo "the migration applied"
