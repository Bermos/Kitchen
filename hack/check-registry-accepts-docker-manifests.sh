#!/usr/bin/env bash
# Push a real Docker manifest schema 2 image at the bundled registry, rendered
# from the chart and running the image the chart deploys.
#
# The failure this exists for is not visible in the rendered configuration.
# zot 2.x is OCI-native and refuses a manifest media type it was not told to
# accept with `415 Unsupported Media Type`, and both of Kitchen's build
# strategies push Docker manifest schema 2: the Cloud Native Buildpacks
# lifecycle's exporter writes it and offers no option, and BuildKit only
# switches to OCI media types when an attestation asks it to. So the whole of
# the bug — and the whole of the fix, `http.compat: ["docker2s2"]` — lives in
# what zot does with the bytes, and only pushing the bytes can see it.
#
# Everything the registry needs comes out of one `helm template`, so the
# credential is the one the chart generates and the image is the one values.yaml
# pins: bumping zot re-runs this against the new version.
#
# zot authenticates nobody itself: it sends every client to the operator's
# token service, which checks the password and signs a token naming the
# repositories it may touch. So the script runs that service too — the
# operator's own handler, from hack/registry-token-server, on the signing key
# the chart generated — and, since it is there, checks the thing per-project
# accounts are for: that one project's account reaches its own repositories
# and nobody else's.
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
HELM="${HELM:-helm}"
CHART="${CHART:-${HERE}/../charts/kitchen}"
WORK="$(mktemp -d)"
CONTAINER="kitchen-registry-mediatype-check-$$"
TOKEN_PID=""
cleanup() {
  docker rm --force "${CONTAINER}" >/dev/null 2>&1 || true
  [ -z "${TOKEN_PID}" ] || kill "${TOKEN_PID}" 2>/dev/null || true
  rm -rf "${WORK}"
}
trap cleanup EXIT

# One render, not three: the chart generates the registry password and the
# token signing key when it cannot look them up, so separate renders would hand
# zot a certificate for a key this script does not have.
"${HELM}" template kitchen "${CHART}" --namespace kitchen-system \
  --set kitchen.baseDomain=apps.example.com \
  --set kitchen.tls.acme.email=platform@example.com \
  --set kitchen.tls.acme.dns01.cloudflare.apiTokenSecretName=cloudflare-api-token \
  -s templates/registry/configmap.yaml \
  -s templates/registry/secret.yaml \
  -s templates/registry/statefulset.yaml > "${WORK}/rendered.yaml"

python3 - "${WORK}" <<'EXTRACT'
import json, pathlib, secrets, shlex, socket, sys, yaml

work = pathlib.Path(sys.argv[1])
objects = [d for d in yaml.safe_load_all((work / "rendered.yaml").read_text()) if d]
by_kind = {d["kind"]: d for d in objects}
for kind in ("ConfigMap", "Secret", "StatefulSet"):
    if kind not in by_kind:
        sys.exit(f"::error::the chart rendered no registry {kind}")

config = json.loads(by_kind["ConfigMap"]["data"]["config.json"])
secret = by_kind["Secret"]["stringData"]
container = by_kind["StatefulSet"]["spec"]["template"]["spec"]["containers"][0]

# The realm the chart renders is the registry's public host. Here the token
# service is a process beside the container, so the realm is pointed at it;
# nothing else in the configuration changes, and zot never calls the realm
# itself — the client does.
bearer = config["http"]["auth"]["bearer"]
if not bearer.get("realm", "").endswith("/kitchen/token"):
    sys.exit(f"::error::the registry's token realm is {bearer.get('realm')!r}, not the token service's path")
with socket.socket() as probe:
    probe.bind(("127.0.0.1", 0))
    token_port = probe.getsockname()[1]
bearer["realm"] = f"http://127.0.0.1:{token_port}/kitchen/token"
(work / "config.json").write_text(json.dumps(config))
(work / "token.crt").write_text(secret["tokenCert"])
(work / "token.key").write_text(secret["tokenKey"])
# Shell-quoted, because the generated password is random bytes and the
# argument list has to survive as a list.
(work / "env").write_text(
    "".join(
        "{}={}\n".format(name, shlex.quote(value))
        for name, value in (
            ("REGISTRY_IMAGE", container["image"]),
            ("REGISTRY_PORT", str(config["http"]["port"])),
            ("REGISTRY_USER", secret["username"]),
            ("REGISTRY_PASSWORD", secret["password"]),
            ("REGISTRY_ARGS", " ".join(container["args"])),
            ("TOKEN_PORT", str(token_port)),
            # Two projects' accounts, in the shape the operator mints them.
            ("ALPHA_PASSWORD", secrets.token_hex(16)),
            ("ALPHA_PULL_PASSWORD", secrets.token_hex(16)),
            ("BETA_PASSWORD", secrets.token_hex(16)),
        )
    )
)
EXTRACT

# shellcheck source=/dev/null
. "${WORK}/env"
# mktemp -d is 0700 and owned by whoever ran this; the container reads through
# the bind mount as the zot image's own user, which need not be either.
chmod 0755 "${WORK}"
chmod 0644 "${WORK}/config.json" "${WORK}/token.crt"

echo "starting the registry token service on the chart's signing key"
(cd "${HERE}/.." && go build -o "${WORK}/registry-token-server" ./hack/registry-token-server)
"${WORK}/registry-token-server" -listen "127.0.0.1:${TOKEN_PORT}" -key "${WORK}/token.key" \
  -account "${REGISTRY_USER}:${REGISTRY_PASSWORD}:platform" \
  -account "alpha.push:${ALPHA_PASSWORD}:project=alpha" \
  -account "alpha.pull:${ALPHA_PULL_PASSWORD}:project-pull=alpha" \
  -account "beta.push:${BETA_PASSWORD}:project=beta" &
TOKEN_PID=$!

echo "starting ${REGISTRY_IMAGE} on the chart's own config"
# shellcheck disable=SC2086  # REGISTRY_ARGS is an argument list
docker run --detach --name "${CONTAINER}" \
  --publish 127.0.0.1:0:"${REGISTRY_PORT}" \
  --volume "${WORK}/config.json:/etc/zot/config/config.json:ro" \
  --volume "${WORK}/token.crt:/etc/zot/token/token.crt:ro" \
  "${REGISTRY_IMAGE}" ${REGISTRY_ARGS} >/dev/null  # word splitting is the point

ADDRESS="$(docker port "${CONTAINER}" "${REGISTRY_PORT}/tcp" | head -n1)"
BASE="http://${ADDRESS}"

# /readyz is one of the three endpoints zot answers without authentication,
# which is why the pod can probe a registry that admits nobody anonymously.
for _ in $(seq 1 60); do
  if curl --fail --silent --output /dev/null "${BASE}/readyz"; then
    break
  fi
  sleep 1
done
curl --fail --silent --output /dev/null "${BASE}/readyz" || {
  echo "::error::the registry never became ready on the chart's config"
  docker logs "${CONTAINER}" || true
  exit 1
}

if ! python3 "${HERE}/registry-push-check.py" "${BASE}" "${REGISTRY_USER}" "${REGISTRY_PASSWORD}" \
  alpha.push "${ALPHA_PASSWORD}" alpha.pull "${ALPHA_PULL_PASSWORD}" beta.push "${BETA_PASSWORD}"; then
  docker logs "${CONTAINER}" || true
  exit 1
fi
