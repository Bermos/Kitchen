#!/usr/bin/env bash
# Stage 0 of docs/OBSERVABILITY.md — prove the vantage point.
#
# The observability design hangs off a single fact it could not establish at
# design time: that a request through a Cilium Gateway produces a Hubble L7
# flow record carrying the HTTP method, URL, status, protocol and a non-zero
# latency, with no CiliumNetworkPolicy anywhere in the cluster. Everything the
# design calls a request row is read off that record (§3.1a, §3.2), and its
# named fallback — Envoy access logs, configured declaratively through
# CiliumGatewayClassConfig's spec.telemetry.accessLogs and read off stdout by
# the node collector — is a supported API rather than a fight with another
# controller, but it is still a format to parse and a second way to lose rows
# (§3.1b). It could not be settled without a cluster: Kitchen's CI never
# installs Cilium, and the chart's kind job deliberately leaves its Gateway
# unprogrammed, so there was no harness to borrow. This is that harness.
#
# It builds the cluster from nothing — kind without kube-proxy, Cilium at the
# repository's pinned version with Gateway API and Hubble, an echo backend
# behind a Gateway — sends requests through it, and reads the flows back off
# Hubble Relay with test/hubble. CI runs it as .github/workflows/hubble.yml; a
# developer runs exactly the same thing with:
#
#     ./hack/check-hubble-l7.sh          # and --keep to be left the cluster
#
# While the cluster is up it also answers the questions §9 left open for stage
# 0 — what Envoy's self-generated 503 looks like, whether trace context
# survives the edge, whether headers are obtainable, how much history Hubble's
# buffer holds. Those are printed and never asserted: they are questions the
# design flagged rather than guessed, and a job that failed on them would be
# asserting an answer nobody has.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Everything this cluster is made of. The names are fixed rather than
# configurable because the flow reader buckets flows by the path segment these
# requests carry, and two places already have to agree on it.
cluster="${CLUSTER_NAME:-kitchen-hubble}"
namespace=kitchen-stage0
gateway=stage0
probe_pod=stage0-probe
route_host=echo.stage0.example.com
# The W3C example trace id, sent in a traceparent header and looked for on the
# flow. One definition, passed to both halves.
trace_id=4bf92f3577b34da6a3ce929d0e0e4736
relay_port="${RELAY_PORT:-4245}"
flow_window="${FLOW_WINDOW:-45s}"

# The backend is a stock Kubernetes e2e image whose netexec answers 200 on any
# path, so every probe path below is served without a config file. Pinned the
# way .github/workflows/helm.yml pins the images it loads.
echo_image="${ECHO_IMAGE:-registry.k8s.io/e2e-test-images/agnhost:2.55}"
# kind's node image floats with kind itself, exactly as in the chart's kind job
# and the e2e job: they install the latest kind and take the node image that
# release was built against. Set KIND_NODE_IMAGE to reproduce a specific
# Kubernetes version locally.
kind_node_image="${KIND_NODE_IMAGE:-}"
# Where LoadBalancer addresses come from when the kind network's own subnet
# cannot be read back from Docker. LB_POOL_CIDR overrides both.
lb_pool_default=172.18.255.192/28

keep=false
work_dir=""
created=false
relay_pid=""
address=""
probe_image=""
probe_started_at=0

usage() {
  cat <<'EOF'
Usage: hack/check-hubble-l7.sh [--keep] [--cluster NAME] [--window DURATION]

  --keep             leave the kind cluster running afterwards
  --cluster NAME     name of the kind cluster (default: kitchen-hubble)
  --window DURATION  how long to follow the flow stream (default: 45s)

Environment overrides: CILIUM_VERSION, GATEWAY_API_VERSION and PROBE_IMAGE (all
three default to the pins in .github/workflows/helm.yml), CLUSTER_NAME,
KIND_NODE_IMAGE, ECHO_IMAGE, LB_POOL_CIDR, RELAY_PORT, FLOW_WINDOW.
EOF
}

note() { printf '\n== %s\n' "$*"; }

# summarise adds a line to the job summary when CI gave us one, so what this
# run established outlives the log — the flow reader appends its own report to
# the same file.
summarise() {
  [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] || return 0
  printf '%s\n' "$*" >>"${GITHUB_STEP_SUMMARY}"
}

warn() {
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    printf '::warning::%s\n' "$*"
  else
    printf 'warning: %s\n' "$*"
  fi
}

die() {
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then
    printf '::error::%s\n' "$*"
  else
    printf 'error: %s\n' "$*"
  fi
  exit 1
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --keep) keep=true ;;
      --cluster) cluster="${2:?--cluster needs a name}"; shift ;;
      --window) flow_window="${2:?--window needs a duration}"; shift ;;
      -h|--help) usage; exit 0 ;;
      *) usage >&2; die "unknown argument: $1" ;;
    esac
    shift
  done
}

require() {
  local tool missing=()
  for tool in "$@"; do
    command -v "${tool}" >/dev/null 2>&1 || missing+=("${tool}")
  done
  if [[ ${#missing[@]} -gt 0 ]]; then
    die "not on PATH: ${missing[*]}"
  fi
  docker info >/dev/null 2>&1 ||
    die "the Docker daemon is not running; in a dev container, start it with 'dockerd &'"
}

# pin reads a version out of the chart's kind workflow. The Cilium release and
# the Gateway API CRDs that go with it are pinned there for the whole
# repository, and CLAUDE.md's rule is that the CRD version is Cilium's to
# choose and must not be written down twice — so this job reads that pin rather
# than declaring a second one that would drift away from it. Bumping
# CILIUM_VERSION there moves this job too.
pin() {
  awk -v key="$1:" '$1 == key { print $2; exit }' "${repo_root}/.github/workflows/helm.yml"
}

resolve_pins() {
  CILIUM_VERSION="${CILIUM_VERSION:-$(pin CILIUM_VERSION)}"
  GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-$(pin GATEWAY_API_VERSION)}"
  [[ -n "${CILIUM_VERSION}" ]] ||
    die "no CILIUM_VERSION in .github/workflows/helm.yml; the pin this job reads has moved"
  [[ -n "${GATEWAY_API_VERSION}" ]] ||
    die "no GATEWAY_API_VERSION in .github/workflows/helm.yml; the pin this job reads has moved"
  # The same curl image the chart's kind job pokes services with, read from the
  # same place for the same reason.
  probe_image="${PROBE_IMAGE:-$(pin PROBE_IMAGE)}"
  [[ -n "${probe_image}" ]] || die "no PROBE_IMAGE in .github/workflows/helm.yml"

  # Cilium states the Gateway API version it supports in one place its docs
  # build from, so the pin is resolved from the release under test rather than
  # kept in step by hand — the same lookup, with the same fallback, that the
  # chart's kind job does. A lookup that cannot reach GitHub falls back to the
  # checked-in pin rather than failing; one that disagrees with it says so,
  # because then the fallback is stale.
  local conf required
  conf="https://raw.githubusercontent.com/cilium/cilium/${CILIUM_VERSION}/Documentation/conf.py"
  required=$(curl -fsSL --retry 3 --retry-delay 5 "${conf}" 2>/dev/null |
    sed -n "s/^gateway_api_version *= *'\(v[0-9.]*\)'.*/\1/p" | head -1 || true)
  if [[ -z "${required}" ]]; then
    warn "could not read the Gateway API version from Cilium ${CILIUM_VERSION};" \
      "falling back to the pinned ${GATEWAY_API_VERSION}"
  elif [[ "${required}" != "${GATEWAY_API_VERSION}" ]]; then
    warn "GATEWAY_API_VERSION is pinned at ${GATEWAY_API_VERSION} but Cilium ${CILIUM_VERSION}" \
      "requires ${required}; testing against ${required}." \
      "Update the pin in .github/workflows/helm.yml."
    GATEWAY_API_VERSION="${required}"
  fi

  note "Cilium ${CILIUM_VERSION}, Gateway API ${GATEWAY_API_VERSION}, backend ${echo_image}"
  summarise "## Hubble stage 0 — Cilium ${CILIUM_VERSION}, Gateway API ${GATEWAY_API_VERSION}"
}

create_cluster() {
  note "creating kind cluster ${cluster} without kube-proxy"
  # kube-proxy is off for a reason, not for taste: Cilium's Gateway API support
  # requires kubeProxyReplacement=true, and the two mechanisms keep independent
  # NAT tables that do not know about each other. The chart's kind job has no
  # such requirement, which is why it creates a plain cluster and this one
  # cannot.
  {
    cat <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  kubeProxyMode: none
nodes:
  - role: control-plane
EOF
    if [[ -n "${kind_node_image}" ]]; then
      printf '    image: %s\n' "${kind_node_image}"
    fi
  } >"${work_dir}/kind.yaml"

  # No --wait: with the default CNI disabled the node stays NotReady until
  # Cilium is installed, so waiting for a Ready node here would wait for the
  # step after next. The API server is up when kind returns, which is all the
  # next steps need.
  kind create cluster --name "${cluster}" --config "${work_dir}/kind.yaml" \
    --kubeconfig "${KUBECONFIG}"
  created=true

  # Pulled on the host, where a failed pull can be retried, rather than inside
  # the node where it surfaces as ImagePullBackOff.
  local image attempt
  for image in "${echo_image}" "${probe_image}"; do
    for attempt in 1 2 3; do
      docker pull "${image}" && break
      [[ "${attempt}" == 3 ]] && die "could not pull ${image}"
      sleep $((attempt * 15))
    done
    kind load docker-image --name "${cluster}" "${image}"
  done
}

install_gateway_api() {
  note "installing Gateway API ${GATEWAY_API_VERSION}"
  # Before Cilium, not after: the Gateway controller only registers for kinds
  # whose CRDs exist when it starts.
  kubectl apply -f \
    "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
}

install_cilium() {
  note "installing Cilium ${CILIUM_VERSION} with Gateway API and Hubble"
  local api_server_ip
  api_server_ip=$(kubectl get node -l node-role.kubernetes.io/control-plane \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  [[ -n "${api_server_ip}" ]] || die "could not read the control-plane node's address"

  helm repo add cilium https://helm.cilium.io/ --force-update >/dev/null
  helm repo update cilium >/dev/null

  # Note what is *not* set here. No CiliumNetworkPolicy, no L7 visibility
  # annotation, no Hubble HTTP metrics — nothing that asks Cilium to proxy
  # anything it would not proxy anyway. The whole question is whether the
  # Gateway's Envoy reports its own traffic on a stock install, so a stock
  # install is what this is: the values below are the Gateway API and Hubble
  # prerequisites and kind's own requirements, and nothing else.
  helm install cilium cilium/cilium --version "${CILIUM_VERSION#v}" \
    --namespace kube-system \
    --set kubeProxyReplacement=true \
    --set k8sServiceHost="${api_server_ip}" \
    --set k8sServicePort=6443 \
    --set ipam.mode=kubernetes \
    --set operator.replicas=1 \
    --set gatewayAPI.enabled=true \
    --set hubble.enabled=true \
    --set hubble.relay.enabled=true \
    --wait --timeout 10m

  kubectl -n kube-system rollout status ds/cilium --timeout=5m
  kubectl -n kube-system rollout status deploy/hubble-relay --timeout=5m
  kubectl wait --for=condition=Ready nodes --all --timeout=5m
}

# lb_pool_cidr carves a block out of the network kind put the nodes on, so the
# addresses handed to Gateways are inside it and could be reached from the host
# later if this job ever needs that. Docker allocates container addresses from
# the bottom of the subnet, so the top is free.
lb_pool_cidr() {
  local subnet prefix
  if [[ -n "${LB_POOL_CIDR:-}" ]]; then
    printf '%s\n' "${LB_POOL_CIDR}"
    return
  fi
  subnet=$({ docker network inspect kind \
    --format '{{ range .IPAM.Config }}{{ println .Subnet }}{{ end }}' 2>/dev/null || true; } |
    grep -m1 '\.' || true)
  case "${subnet}" in
    *.*.0.0/16)
      prefix="${subnet%.0.0/16}"
      printf '%s.255.192/28\n' "${prefix}"
      ;;
    *)
      printf '%s\n' "${lb_pool_default}"
      ;;
  esac
}

announce_addresses() {
  # Cilium reports Programmed=False / AddressNotAssigned on a Gateway whose
  # LoadBalancer Service never gets an address, and an unprogrammed Gateway
  # serves nothing — so this job needs exactly what the chart's kind job
  # deliberately does without. LB IPAM is the smallest thing that gives kind
  # one, and it is the natural choice because Cilium is already here.
  #
  # L2 announcement is deliberately not enabled: it is what makes the address
  # answer ARP on the node's segment, which only matters for reaching it from
  # outside the cluster. The requests below come from a pod, cross the same
  # Envoy, and prove the same thing with fewer moving parts.
  #
  # The pool's own API version is asked for rather than written down: LB IPAM
  # graduated from v2alpha1 to v2, and this job's Cilium version is whatever
  # the chart's workflow pins.
  local version pool
  for _ in $(seq 30); do
    version=$(kubectl get crd ciliumloadbalancerippools.cilium.io \
      -o jsonpath='{.spec.versions[?(@.storage==true)].name}' 2>/dev/null || true)
    [[ -n "${version}" ]] && break
    sleep 2
  done
  [[ -n "${version}" ]] || die "Cilium registered no CiliumLoadBalancerIPPool CRD"
  pool="$(lb_pool_cidr)"
  note "announcing LoadBalancer addresses from ${pool} (cilium.io/${version})"

  kubectl apply -f - <<EOF
apiVersion: cilium.io/${version}
kind: CiliumLoadBalancerIPPool
metadata:
  name: stage0
spec:
  blocks:
    - cidr: "${pool}"
EOF
}

deploy_backend() {
  note "deploying the echo backend, the Gateway and its routes"
  # The operator creates the GatewayClass when it starts, and a Gateway naming
  # a class that is not there yet is simply ignored — which then looks like the
  # Gateway never being programmed rather than like a race.
  for _ in $(seq 60); do
    kubectl get gatewayclass cilium >/dev/null 2>&1 && break
    sleep 2
  done
  kubectl get gatewayclass cilium >/dev/null 2>&1 ||
    die "Cilium created no 'cilium' GatewayClass; is gatewayAPI.enabled reaching the operator?"

  kubectl apply -f - <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${namespace}
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: echo
  namespace: ${namespace}
spec:
  replicas: 1
  selector:
    matchLabels:
      app: echo
  template:
    metadata:
      labels:
        app: echo
    spec:
      containers:
        - name: echo
          image: ${echo_image}
          args: ["netexec", "--http-port=8080"]
          ports:
            - containerPort: 8080
          readinessProbe:
            httpGet:
              path: /healthz
              port: 8080
---
apiVersion: v1
kind: Service
metadata:
  name: echo
  namespace: ${namespace}
spec:
  selector:
    app: echo
  ports:
    - name: http
      port: 80
      targetPort: 8080
---
# The dead backend §9 asks about: a Service that resolves, so the route is
# accepted and Envoy builds a cluster for it, selecting a workload that does
# not exist, so the cluster has no endpoint and Envoy has to answer by itself.
apiVersion: v1
kind: Service
metadata:
  name: echo-dead
  namespace: ${namespace}
spec:
  selector:
    app: echo-never-scheduled
  ports:
    - name: http
      port: 80
      targetPort: 8080
---
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: ${gateway}
  namespace: ${namespace}
spec:
  gatewayClassName: cilium
  listeners:
    - name: http
      protocol: HTTP
      port: 80
      allowedRoutes:
        namespaces:
          from: Same
---
apiVersion: gateway.networking.k8s.io/v1
kind: HTTPRoute
metadata:
  name: echo
  namespace: ${namespace}
spec:
  parentRefs:
    - name: ${gateway}
  hostnames:
    - "${route_host}"
  rules:
    - matches:
        - path:
            type: PathPrefix
            value: /stage0/dead
      backendRefs:
        - name: echo-dead
          port: 80
    - matches:
        - path:
            type: PathPrefix
            value: /stage0
      backendRefs:
        - name: echo
          port: 80
EOF
  kubectl -n "${namespace}" rollout status deploy/echo --timeout=5m
}

await_gateway() {
  note "waiting for the Gateway to be programmed"
  kubectl -n "${namespace}" wait --for=condition=Programmed "gateway/${gateway}" --timeout=5m
  address=$(kubectl -n "${namespace}" get gateway "${gateway}" \
    -o jsonpath='{.status.addresses[0].value}')
  [[ -n "${address}" ]] || die "the Gateway is programmed but published no address"
  note "gateway address ${address}"
}

# count answers how many objects of a kind exist, and zero when the kind is not
# even registered.
count() {
  { kubectl get "$@" --no-headers 2>/dev/null || true; } | wc -l | tr -dc '0-9'
}

# assert_no_policy is the other half of the claim, and it is asserted rather
# than merely arranged. Cilium emits L7 flows for pod traffic when a
# CiliumNetworkPolicy with L7 rules routes it through the proxy — §3.1c, the
# candidate the design rejected — so a policy anywhere in this cluster would
# make the flows below prove the wrong thing. Nothing here creates one; this is
# what says so.
assert_no_policy() {
  local when=$1 cnp ccnp netpol
  cnp=$(count ciliumnetworkpolicies.cilium.io -A)
  ccnp=$(count ciliumclusterwidenetworkpolicies.cilium.io)
  netpol=$(count networkpolicies.networking.k8s.io -A)
  note "policies ${when} the requests: ${cnp} CiliumNetworkPolicy, ${ccnp} clusterwide, ${netpol} NetworkPolicy"
  summarise "- Policies ${when} the requests: **${cnp}** CiliumNetworkPolicy," \
    "**${ccnp}** CiliumClusterwideNetworkPolicy, **${netpol}** NetworkPolicy."
  if [[ "${cnp}" -ne 0 || "${ccnp}" -ne 0 || "${netpol}" -ne 0 ]]; then
    kubectl get ciliumnetworkpolicies.cilium.io -A || true
    kubectl get ciliumclusterwidenetworkpolicies.cilium.io || true
    kubectl get networkpolicies.networking.k8s.io -A || true
    die "a network policy exists ${when} the requests, so any L7 flow below could be" \
      "the policy's doing and proves nothing about the Gateway's vantage point"
  fi
}

open_relay() {
  note "port-forwarding Hubble Relay to 127.0.0.1:${relay_port}"
  # Opened before the requests, so the gap between the last request and the
  # stream is a couple of seconds: the reader asks for buffered flows, and
  # Hubble's ring holds a few thousand of them.
  kubectl -n kube-system port-forward svc/hubble-relay "${relay_port}:80" \
    >"${work_dir}/port-forward.log" 2>&1 &
  relay_pid=$!

  local _
  for _ in $(seq 30); do
    if (exec 3<>"/dev/tcp/127.0.0.1/${relay_port}") 2>/dev/null; then
      return
    fi
    sleep 1
  done
  cat "${work_dir}/port-forward.log" || true
  die "Hubble Relay never accepted a connection on 127.0.0.1:${relay_port}"
}

send_requests() {
  note "sending requests through the Gateway"
  probe_started_at=$(date +%s)
  kubectl -n "${namespace}" delete configmap "${probe_pod}" --ignore-not-found >/dev/null
  kubectl -n "${namespace}" create configmap "${probe_pod}" \
    --from-file=probe.sh="${repo_root}/hack/hubble-l7-probe.sh" >/dev/null

  kubectl apply -f - <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${probe_pod}
  namespace: ${namespace}
spec:
  restartPolicy: Never
  volumes:
    - name: probe
      configMap:
        name: ${probe_pod}
  containers:
    - name: curl
      image: ${probe_image}
      command:
        - sh
        - /probe/probe.sh
        - "${route_host}"
        - "${trace_id}"
        - "http://${address}"
        - "http://cilium-gateway-${gateway}.${namespace}.svc.cluster.local"
      volumeMounts:
        - name: probe
          mountPath: /probe
EOF

  local phase=""
  for _ in $(seq 120); do
    phase=$(kubectl -n "${namespace}" get pod "${probe_pod}" \
      -o jsonpath='{.status.phase}' 2>/dev/null || true)
    case "${phase}" in Succeeded|Failed) break ;; esac
    sleep 2
  done
  kubectl -n "${namespace}" logs "${probe_pod}" 2>&1 || true
  [[ "${phase}" == Succeeded ]] ||
    die "the probe did not get its requests through the Gateway (pod phase '${phase}')"
}

read_flows() {
  note "reading the flows back off Hubble Relay"
  local since
  since=$(( $(date +%s) - probe_started_at + 120 ))
  # The reader is a Go program rather than parsed `hubble` output because it
  # speaks the same API the operator's follower does: if Relay stops handing
  # over what internal/flows reads, this stops too.
  (cd "${repo_root}" && go run ./test/hubble \
    -relay "127.0.0.1:${relay_port}" \
    -since "${since}s" \
    -window "${flow_window}" \
    -trace-id "${trace_id}" \
    -summary "${GITHUB_STEP_SUMMARY:-}")
}

diagnostics() {
  note "diagnostics"
  kubectl get nodes -o wide || true
  kubectl -n kube-system get pods -o wide || true
  kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status --brief ||
    kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium status --brief || true
  kubectl -n kube-system get ciliumenvoyconfigs.cilium.io -A || true
  kubectl get ciliumloadbalancerippools.cilium.io -o wide || true
  kubectl -n "${namespace}" get gateway,httproute,svc,pods -o wide || true
  kubectl -n "${namespace}" get "gateway/${gateway}" -o yaml || true
  kubectl -n "${namespace}" get httproute echo -o yaml || true
  kubectl -n "${namespace}" describe "svc/cilium-gateway-${gateway}" || true
  kubectl -n "${namespace}" logs "${probe_pod}" --tail=50 || true
  kubectl -n kube-system logs deploy/hubble-relay --tail=50 || true
  kubectl -n kube-system logs ds/cilium -c cilium-agent --tail=100 || true
  # Recent flows, if the relay is still reachable: observe mode asserts nothing,
  # so this is a dump and not a second verdict.
  if [[ -n "${relay_pid}" ]]; then
    (cd "${repo_root}" && go run ./test/hubble -observe -match / \
      -relay "127.0.0.1:${relay_port}" -since 10m -window 10s) || true
  fi
  [[ -f "${work_dir}/port-forward.log" ]] && cat "${work_dir}/port-forward.log"
  return 0
}

on_exit() {
  local status=$?
  trap - EXIT
  set +e
  if [[ "${status}" -ne 0 && "${created}" == true ]]; then
    diagnostics
  fi
  if [[ -n "${relay_pid}" ]]; then
    kill "${relay_pid}" 2>/dev/null
    wait "${relay_pid}" 2>/dev/null
  fi
  if [[ "${created}" == true ]]; then
    if [[ "${keep}" == true ]]; then
      note "keeping cluster ${cluster}: kind export kubeconfig --name ${cluster}"
    else
      kind delete cluster --name "${cluster}" >/dev/null 2>&1
    fi
  fi
  [[ -n "${work_dir}" ]] && rm -rf "${work_dir}"
  exit "${status}"
}

main() {
  parse_args "$@"
  require docker kind kubectl helm go curl awk sed

  work_dir="$(mktemp -d)"
  trap on_exit EXIT
  # Its own kubeconfig: this creates and deletes a cluster, and it has no
  # business touching the one a developer has open.
  export KUBECONFIG="${work_dir}/kubeconfig"

  resolve_pins
  create_cluster
  install_gateway_api
  install_cilium
  announce_addresses
  deploy_backend
  await_gateway
  assert_no_policy before
  open_relay
  send_requests
  read_flows
  assert_no_policy after

  note "the Gateway's Envoy is a usable vantage point for request telemetry"
}

main "$@"
