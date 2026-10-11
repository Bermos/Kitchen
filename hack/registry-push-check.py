#!/usr/bin/env python3
"""Push an image at a running registry, once in Docker media types and once in
OCI's, and fail loudly if either is refused — then, given two projects'
accounts, check that each reaches its own repositories and no other's.

Docker manifest schema 2 is what both of Kitchen's build strategies push by
default, so a registry that rejects it fails every build at its last step —
after the image is built and every layer uploaded. The OCI push is here so that
the compatibility setting that admits the first one is never mistaken for a
switch away from the second: BuildKit writes OCI media types whenever an
attestation is asked for, and both have to work on the same registry.

Only the standard library, so this runs wherever python3 does.
"""

import base64
import hashlib
import json
import re
import sys
import urllib.error
import urllib.parse
import urllib.request

DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
DOCKER_CONFIG = "application/vnd.docker.container.image.v1+json"
DOCKER_LAYER = "application/vnd.docker.image.rootfs.diff.tar.gzip"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
OCI_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"


class Registry:
    """A registry client that speaks the Docker token flow, which is the only
    way the bundled registry admits anybody: a 401 names the token service, the
    client asks it for the scope the request needs with its username and
    password, and retries with the token it got back."""

    def __init__(self, base, username, password):
        self.base = base.rstrip("/")
        self.basic = "Basic " + base64.b64encode(f"{username}:{password}".encode()).decode()
        self.tokens = {}

    def _send(self, method, url, body, content_type, accept, authorization):
        request = urllib.request.Request(url, data=body, method=method)
        if authorization:
            request.add_header("Authorization", authorization)
        if content_type:
            request.add_header("Content-Type", content_type)
        if accept:
            request.add_header("Accept", accept)
        try:
            with urllib.request.urlopen(request) as response:
                return response.status, dict(response.headers), response.read()
        except urllib.error.HTTPError as err:
            return err.code, dict(err.headers), err.read()

    def token(self, challenge):
        params = dict(re.findall(r'(\w+)="([^"]*)"', challenge))
        query = {"service": params.get("service", "")}
        if params.get("scope"):
            query["scope"] = params["scope"]
        key = query.get("scope", "")
        if key not in self.tokens:
            url = params["realm"] + "?" + urllib.parse.urlencode(query)
            status, _, body = self._send("GET", url, None, None, None, self.basic)
            if status != 200:
                return None, status
            self.tokens[key] = json.loads(body)["token"]
        return self.tokens[key], 200

    def request(self, method, path, body=None, content_type=None, accept=None):
        url = path if path.startswith("http") else self.base + path
        status, headers, answer = self._send(method, url, body, content_type, accept, None)
        challenge = headers.get("Www-Authenticate") or headers.get("WWW-Authenticate") or ""
        if status != 401 or not challenge.lower().startswith("bearer "):
            return status, headers, answer
        token, token_status = self.token(challenge)
        if token is None:
            return token_status, {}, b"the token service refused the credential"
        return self._send(method, url, body, content_type, accept, f"Bearer {token}")

    def push_blob(self, repository, payload):
        digest = "sha256:" + hashlib.sha256(payload).hexdigest()
        status, headers, body = self.request(
            "POST", f"/v2/{repository}/blobs/uploads/"
        )
        if status != 202:
            fail(f"the registry refused a blob upload session: {status} {body!r}")
        location = headers["Location"]
        if not location.startswith("http"):
            location = self.base + location
        separator = "&" if "?" in location else "?"
        status, _, body = self.request(
            "PUT",
            f"{location}{separator}digest={digest}",
            body=payload,
            content_type="application/octet-stream",
        )
        if status != 201:
            fail(f"the registry refused a blob: {status} {body!r}")
        return digest, len(payload)


def fail(message):
    print(f"::error::{message}")
    sys.exit(1)


def push_image(registry, repository, tag, manifest_type, config_type, layer_type):
    config = json.dumps(
        {
            "architecture": "amd64",
            "os": "linux",
            "config": {},
            "rootfs": {"type": "layers", "diff_ids": []},
        }
    ).encode()
    layer = b"kitchen registry media type check"

    config_digest, config_size = registry.push_blob(repository, config)
    layer_digest, layer_size = registry.push_blob(repository, layer)

    manifest = json.dumps(
        {
            "schemaVersion": 2,
            "mediaType": manifest_type,
            "config": {
                "mediaType": config_type,
                "digest": config_digest,
                "size": config_size,
            },
            "layers": [
                {
                    "mediaType": layer_type,
                    "digest": layer_digest,
                    "size": layer_size,
                }
            ],
        }
    ).encode()

    status, _, body = registry.request(
        "PUT",
        f"/v2/{repository}/manifests/{tag}",
        body=manifest,
        content_type=manifest_type,
    )
    if status == 415:
        fail(
            f"the registry answered 415 Unsupported Media Type to a {manifest_type} "
            "manifest. zot admits Docker manifest schema 2 only when http.compat "
            'includes "docker2s2", and both build strategies push it by default, so '
            "every build would fail at its last step with MANIFEST_INVALID."
        )
    if status != 201:
        fail(f"the registry refused a {manifest_type} manifest: {status} {body!r}")

    status, _, body = registry.request(
        "GET", f"/v2/{repository}/manifests/{tag}", accept=manifest_type
    )
    if status != 200:
        fail(f"the registry stored a {manifest_type} manifest it will not serve: {status}")
    if json.loads(body)["config"]["digest"] != config_digest:
        fail(f"the registry served back a different {manifest_type} manifest")
    print(f"pushed and read back a {manifest_type} image")


def can_push(registry, repository):
    status, _, _ = registry.request("POST", f"/v2/{repository}/blobs/uploads/")
    return status == 202


def can_pull(registry, repository, tag):
    status, _, _ = registry.request(
        "GET", f"/v2/{repository}/manifests/{tag}", accept=DOCKER_MANIFEST
    )
    return status == 200


def check_isolation(base, platform, alpha, alpha_pull, beta):
    """Each project's accounts reach its own repositories and no other's —
    which is the whole point of having one per project."""
    # Something to read in each project, pushed by the platform's account.
    for repository in ("alpha", "alpha/worker", "beta"):
        push_image(platform, repository, "seed", DOCKER_MANIFEST, DOCKER_CONFIG, DOCKER_LAYER)

    alpha_push = Registry(base, *alpha)
    alpha_read = Registry(base, *alpha_pull)
    beta_push = Registry(base, *beta)
    expectations = [
        ("alpha's account pushes to alpha", can_push(alpha_push, "alpha"), True),
        ("alpha's account pushes to alpha/worker", can_push(alpha_push, "alpha/worker"), True),
        ("alpha's account pulls alpha", can_pull(alpha_push, "alpha", "seed"), True),
        ("alpha's account pushes to beta", can_push(alpha_push, "beta"), False),
        ("alpha's account pulls beta", can_pull(alpha_push, "beta", "seed"), False),
        ("alpha's account pushes to alpha-worker", can_push(alpha_push, "alpha-worker"), False),
        ("alpha's read account pulls alpha/worker", can_pull(alpha_read, "alpha/worker", "seed"), True),
        ("alpha's read account pushes to alpha", can_push(alpha_read, "alpha"), False),
        ("beta's account pulls alpha", can_pull(beta_push, "alpha", "seed"), False),
        ("beta's account pulls beta", can_pull(beta_push, "beta", "seed"), True),
    ]
    wrong = [what for what, got, want in expectations if got != want]
    for what, got, want in expectations:
        print(f"{'ok  ' if got == want else 'FAIL'} {what}: {'yes' if got else 'no'}")
    if wrong:
        fail("a project's registry account reached past its own repositories, or not as "
             "far as them: " + "; ".join(wrong))


def main():
    if len(sys.argv) not in (4, 10):
        sys.exit("usage: registry-push-check.py BASE_URL USERNAME PASSWORD "
                 "[ALPHA_USER ALPHA_PASSWORD ALPHA_PULL_USER ALPHA_PULL_PASSWORD BETA_USER BETA_PASSWORD]")
    registry = Registry(sys.argv[1], sys.argv[2], sys.argv[3])

    status, _, _ = registry.request("GET", "/v2/")
    if status != 200:
        fail(f"the registry did not accept the chart's own credential: {status}")

    push_image(
        registry, "kitchen-demo", "docker-schema2",
        DOCKER_MANIFEST, DOCKER_CONFIG, DOCKER_LAYER,
    )
    push_image(
        registry, "kitchen-demo", "oci",
        OCI_MANIFEST, OCI_CONFIG, OCI_LAYER,
    )

    if len(sys.argv) == 10:
        args = sys.argv[4:]
        check_isolation(sys.argv[1], registry, (args[0], args[1]), (args[2], args[3]), (args[4], args[5]))


if __name__ == "__main__":
    main()
