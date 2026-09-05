#!/usr/bin/env bash
# A kind cluster whose CNI is Cilium — the one implementation of it.
#
# Two CI jobs need such a cluster and they need the same one:
#
#   - "Gateway L7 flows on kind" (hack/check-hubble-l7.sh) reads Hubble's L7
#     flow records off the shared Gateway's Envoy, which needs Cilium's Gateway
#     API support and therefore its kube-proxy replacement;
#   - "Chart install on Cilium" (.github/workflows/helm.yml) installs the whole
#     chart on a CNI that actually enforces NetworkPolicy, because kindnet does
#     not and a policy that blocked every flow on the platform would still
#     leave that job green.
#
# The forty lines that build the cluster live here rather than in both callers.
# Two copies of a Cilium install drift the moment either version moves, and the
# version is the thing they must agree on: CLAUDE.md's rule is that the Gateway
# API CRD version belongs to Cilium and is not written down twice, so it is
# resolved from the release under test — from the one CILIUM_VERSION pinned in
# .github/workflows/helm.yml for the whole repository.
#
#   hack/install-cilium.sh [--create] [--cluster NAME] [--hubble]
#                          [--versions-file PATH]
#
#     --create              create the kind cluster first, in the shape Cilium
#                           needs: no default CNI, and no kube-proxy
#     --cluster NAME        kind cluster to build (default: kind)
#     --hubble              also enable Hubble and Hubble Relay
#     --versions-file PATH  write the resolved CILIUM_VERSION and
#                           GATEWAY_API_VERSION there, for a caller that
#                           reports them
#
# Environment overrides: CILIUM_VERSION and GATEWAY_API_VERSION (both default
# to the pins in .github/workflows/helm.yml), KIND_NODE_IMAGE, LB_POOL_CIDR,
# KUBECONFIG.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

cluster=kind
create=false
hubble=false
versions_file=""
# kind's node image floats with kind itself, exactly as in the chart's kind job
# and the e2e job: they install the latest kind and take the node image that
# release was built against. Set KIND_NODE_IMAGE to pin a Kubernetes version.
kind_node_image="${KIND_NODE_IMAGE:-}"
# Where LoadBalancer addresses come from when the kind network's own subnet
# cannot be read back from Docker. LB_POOL_CIDR overrides both.
lb_pool_default=172.18.255.192/28

note() { printf '\n== %s\n' "$*"; }

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

usage() {
  sed -n '/^#   hack\/install-cilium.sh/,/^# KUBECONFIG/p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
}

parse_args() {
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --create) create=true ;;
      --hubble) hubble=true ;;
      --cluster) cluster="${2:?--cluster needs a name}"; shift ;;
      --versions-file) versions_file="${2:?--versions-file needs a path}"; shift ;;
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

# pin reads a version out of the chart's kind workflow, which is where the
# repository pins the Cilium release it targets and the Gateway API version to
# fall back on.
pin() {
  awk -v key="$1:" '$1 == key { print $2; exit }' "${repo_root}/.github/workflows/helm.yml"
}

resolve_pins() {
  CILIUM_VERSION="${CILIUM_VERSION:-$(pin CILIUM_VERSION)}"
  GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-$(pin GATEWAY_API_VERSION)}"
  [[ -n "${CILIUM_VERSION}" ]] ||
    die "no CILIUM_VERSION in .github/workflows/helm.yml; the pin this script reads has moved"
  [[ -n "${GATEWAY_API_VERSION}" ]] ||
    die "no GATEWAY_API_VERSION in .github/workflows/helm.yml; the pin this script reads has moved"

  # Cilium states the Gateway API version it supports in one place its docs
  # build from, so the pin is resolved from the release under test rather than
  # kept in step by hand — which is how it last drifted six minor versions
  # behind. A lookup that cannot reach GitHub falls back to the checked-in pin
  # rather than failing; one that disagrees with it says so, because then the
  # fallback is stale.
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

  note "Cilium ${CILIUM_VERSION}, Gateway API ${GATEWAY_API_VERSION}"
  # Both are handed back to whoever asked, so a caller reports the versions it
  # actually ran against rather than the ones it hoped for.
  if [[ -n "${versions_file}" ]]; then
    printf 'CILIUM_VERSION=%s\nGATEWAY_API_VERSION=%s\n' \
      "${CILIUM_VERSION}" "${GATEWAY_API_VERSION}" >"${versions_file}"
  fi
  if [[ -n "${GITHUB_ENV:-}" ]]; then
    printf 'CILIUM_VERSION=%s\nGATEWAY_API_VERSION=%s\n' \
      "${CILIUM_VERSION}" "${GATEWAY_API_VERSION}" >>"${GITHUB_ENV}"
  fi
}

create_cluster() {
  note "creating kind cluster ${cluster} with no CNI and no kube-proxy"
  # kube-proxy is off for a reason, not for taste: Cilium's Gateway API support
  # requires kubeProxyReplacement=true, and the two mechanisms keep independent
  # NAT tables that do not know about each other.
  local config
  config="$(mktemp)"
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
  } >"${config}"

  # No --wait: with the default CNI disabled the node stays NotReady until
  # Cilium is installed, so waiting for a Ready node here would wait for the
  # step after next. The API server is up when kind returns, which is all the
  # next steps need.
  local args=(create cluster --name "${cluster}" --config "${config}")
  [[ -n "${KUBECONFIG:-}" ]] && args+=(--kubeconfig "${KUBECONFIG}")
  kind "${args[@]}"
  rm -f "${config}"
}

install_gateway_api() {
  note "installing Gateway API ${GATEWAY_API_VERSION}"
  # Before Cilium, not after: the Gateway controller only registers for kinds
  # whose CRDs exist when it starts.
  kubectl apply -f \
    "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml"
}

install_cilium() {
  local api_server_ip
  if [[ "${hubble}" == true ]]; then
    note "installing Cilium ${CILIUM_VERSION} with Gateway API and Hubble"
  else
    note "installing Cilium ${CILIUM_VERSION} with Gateway API"
  fi
  api_server_ip=$(kubectl get node -l node-role.kubernetes.io/control-plane \
    -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}')
  [[ -n "${api_server_ip}" ]] || die "could not read the control-plane node's address"

  helm repo add cilium https://helm.cilium.io/ --force-update >/dev/null
  helm repo update cilium >/dev/null

  # Note what is *not* set here. No CiliumNetworkPolicy, no L7 visibility
  # annotation, no Hubble HTTP metrics — nothing that asks Cilium to proxy or
  # to police anything it would not anyway. Both callers want a stock install:
  # one asks whether the Gateway's Envoy reports its own traffic on one, the
  # other whether the chart's plain networking.k8s.io/v1 policies are enforced
  # on one. The values below are the Gateway API prerequisites and kind's own
  # requirements, and nothing else.
  local sets=(
    --set kubeProxyReplacement=true
    --set "k8sServiceHost=${api_server_ip}"
    --set k8sServicePort=6443
    --set ipam.mode=kubernetes
    --set operator.replicas=1
    --set gatewayAPI.enabled=true
  )
  if [[ "${hubble}" == true ]]; then
    sets+=(--set hubble.enabled=true --set hubble.relay.enabled=true)
  fi

  helm install cilium cilium/cilium --version "${CILIUM_VERSION#v}" \
    --namespace kube-system "${sets[@]}" --wait --timeout 10m

  kubectl -n kube-system rollout status ds/cilium --timeout=5m
  if [[ "${hubble}" == true ]]; then
    kubectl -n kube-system rollout status deploy/hubble-relay --timeout=5m
  fi
  kubectl wait --for=condition=Ready nodes --all --timeout=5m
}

# lb_pool_cidr carves a block out of the network kind put the nodes on, so the
# addresses handed to Gateways are inside it and could be reached from the host
# later if a job ever needs that. Docker allocates container addresses from the
# bottom of the subnet, so the top is free.
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
  # serves nothing. LB IPAM is the smallest thing that gives kind one, and it
  # is the natural choice because Cilium is already here.
  #
  # L2 announcement is deliberately not enabled: it is what makes the address
  # answer ARP on the node's segment, which only matters for reaching it from
  # outside the cluster. Both callers send their requests from a pod.
  #
  # The pool's own API version is asked for rather than written down: LB IPAM
  # graduated from v2alpha1 to v2, and the Cilium version here is whatever the
  # chart's workflow pins.
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
  name: kind
spec:
  blocks:
    - cidr: "${pool}"
EOF
}

main() {
  parse_args "$@"
  require docker kind kubectl helm curl awk sed

  resolve_pins
  if [[ "${create}" == true ]]; then
    create_cluster
  fi
  install_gateway_api
  install_cilium
  announce_addresses

  note "cluster ${cluster} is running Cilium ${CILIUM_VERSION}"
}

main "$@"
