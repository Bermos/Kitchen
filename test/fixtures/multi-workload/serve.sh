#!/bin/sh
# Every workload of this fixture that keeps running, answering the four
# questions the end-to-end case asks of it over HTTP.
#
# It writes the answers as files and serves the directory, rather than running
# a request handler, because busybox has an HTTP server and no scripting
# language: a file per question is the whole implementation, and each one is a
# fact this container cannot make up.
set -eu

mkdir -p /www

# Which stage of the Dockerfile this image came out of, baked in at build time.
printf '%s\n' "${STAGE:-unknown}" > /www/stage.txt

# Which workload of the unit this container is. The platform sets
# KITCHEN_PROCESS for everything but the web process, which gets no such
# variable because its workload is the environment itself.
printf '%s\n' "${KITCHEN_PROCESS:-web}" > /www/process.txt

# Which pod answered. It is what makes "the preview reached its own sibling"
# a checkable claim rather than a hopeful one: the name says which environment
# the pod belongs to.
hostname > /www/host.txt

# Which release this container was started for, from the release's own frozen
# environment. A deploy that was not applied is a pod still answering with the
# release before it.
printf '%s\n' "${RELEASE_LABEL:-none}" > /www/release.txt

# PORT is the platform's: the web process gets the project's runtime port and
# a service workload gets its own, so the same script serves both.
exec httpd -f -v -p "${PORT:-8080}" -h /www
