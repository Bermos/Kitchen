#!/usr/bin/env python3
"""Push an image at a running registry, once in Docker media types and once in
OCI's, and fail loudly if either is refused.

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
import sys
import urllib.error
import urllib.request

DOCKER_MANIFEST = "application/vnd.docker.distribution.manifest.v2+json"
DOCKER_CONFIG = "application/vnd.docker.container.image.v1+json"
DOCKER_LAYER = "application/vnd.docker.image.rootfs.diff.tar.gzip"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
OCI_LAYER = "application/vnd.oci.image.layer.v1.tar+gzip"


class Registry:
    def __init__(self, base, username, password):
        self.base = base.rstrip("/")
        token = base64.b64encode(f"{username}:{password}".encode()).decode()
        self.auth = f"Basic {token}"

    def request(self, method, path, body=None, content_type=None, accept=None):
        url = path if path.startswith("http") else self.base + path
        request = urllib.request.Request(url, data=body, method=method)
        request.add_header("Authorization", self.auth)
        if content_type:
            request.add_header("Content-Type", content_type)
        if accept:
            request.add_header("Accept", accept)
        try:
            with urllib.request.urlopen(request) as response:
                return response.status, dict(response.headers), response.read()
        except urllib.error.HTTPError as err:
            return err.code, dict(err.headers), err.read()

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


def main():
    if len(sys.argv) != 4:
        sys.exit("usage: registry-push-check.py BASE_URL USERNAME PASSWORD")
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


if __name__ == "__main__":
    main()
