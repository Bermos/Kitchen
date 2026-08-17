#!/bin/sh
# The requests stage 0 needs Hubble to have seen — sent from inside the
# cluster, through the shared Gateway, by hack/check-hubble-l7.sh.
#
# This runs in a curl image as a Pod, not on the runner: the Gateway's address
# comes from Cilium's LB IPAM and is routed by Cilium's own datapath, so a pod
# is the one client that is certain to reach it without also announcing the
# address on the node's L2 segment. The vantage point under test is the same
# either way — every one of these requests is proxied by the Gateway's Envoy,
# which is the only thing the flow assertion depends on.
#
# Its log is the report: one "probe <label> <path> -> <code>" line per request
# and a final "probe-ok". The path segment after /stage0/ is the label the flow
# reader (test/hubble) buckets flows by, so these names and its constants are
# one vocabulary.
#
# Usage: probe.sh <host> <trace-id> <base-url>...
set -eu

host=$1
trace_id=$2
shift 2

# Wait for the Gateway to answer on one of the addresses offered. A listener
# that Cilium has programmed but not yet wired up refuses connections for a
# few seconds, and the first candidate is the LoadBalancer address the Gateway
# published — the second is the Service Cilium creates for it, tried only so a
# datapath surprise reads as "reached it another way" rather than as a failure
# of the thing under test.
base=""
attempts=0
while [ -z "${base}" ]; do
  for candidate in "$@"; do
    if curl -fsS -o /dev/null -m 5 -H "Host: ${host}" "${candidate}/stage0/warmup"; then
      base="${candidate}"
      break
    fi
  done
  if [ -n "${base}" ]; then
    break
  fi
  attempts=$((attempts + 1))
  if [ "${attempts}" -ge 60 ]; then
    echo "probe-failed: no gateway address answered: $*"
    exit 1
  fi
  sleep 3
done
echo "probe base ${base}"

last_code=""

# request <label> <path> [curl arguments...]
request() {
  label=$1
  path=$2
  shift 2
  last_code=$(curl -sS -o /dev/null -m 10 -w '%{http_code}' \
    -H "Host: ${host}" "$@" "${base}${path}" 2>/dev/null || echo 000)
  echo "probe ${label} ${path} -> ${last_code}"
}

# The plain request the assertion is made on, several times over: one flow is
# proof, a handful gives the rate and loss measurements something to measure.
sent=0
while [ "${sent}" -lt 5 ]; do
  request ok /stage0/ok
  sent=$((sent + 1))
done
if [ "${last_code}" != 200 ]; then
  echo "probe-failed: the echo backend answered ${last_code}, not 200;"
  echo "              a flow assertion over a broken backend would prove nothing"
  exit 1
fi

# §9: does Cilium populate flow.trace_context from an incoming traceparent?
request trace /stage0/trace -H "traceparent: 00-${trace_id}-00f067aa0ba902b7-01"

# §9: is grpc-status obtainable in the header list? A real gRPC call needs a
# real gRPC server; what settles the question cheaply is whether Hubble records
# any header at all for Gateway traffic, so this sends the headers a gRPC call
# carries and the reader reports what came back.
request grpc /stage0/grpc \
  -H 'content-type: application/grpc' \
  -H 'grpc-status: 0' \
  -H 'te: trailers'

# §9: what does Envoy's own answer look like? This route's Service exists and
# selects nothing, so there is no endpoint to proxy to and Envoy answers by
# itself.
request dead /stage0/dead

# §3.4 labels gRPC traffic by protocol, so it matters what Hubble calls HTTP/2.
request h2 /stage0/h2 --http2-prior-knowledge

echo probe-ok
