/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

// Every image this package makes the cluster run, in one place, named by
// digest.
//
// # Why a digest and not a tag
//
// A tag is a mutable pointer, and every one of these is somebody else's
// repository. `alpine/helm:3.19.0` repointed overnight is somebody else's
// code running as the self-update Job — which the chart binds to
// cluster-admin — and neither the operator nor the kubelet would notice,
// because a tag that moved is exactly what a tag is for. The digest is what
// the kubelet resolves: content that does not hash to it is not the image,
// so the platform runs what was reviewed or it runs nothing.
//
// The tag is kept beside the digest because it is what a person reads. It
// carries no weight at pull time — a reference naming both resolves by the
// digest alone — so the pair is only ever wrong in the direction of a
// misleading comment, which the script below is what catches.
//
// # Bumping one
//
// Read the digest of the tag you want off the registry and write both halves
// in one edit, the way internal/provider/cache/valkey.go writes its
// catalogue and addon_keda.go writes its two chart versions:
//
//	crane digest paketobuildpacks/builder-jammy-base:0.4.625
//	skopeo inspect --format '{{.Digest}}' docker://alpine/helm:3.19.0
//	hack/check-image-pins.sh          # every pin here, against the registry
//
// Take the digest of the multi-arch index rather than of one platform's
// manifest, so a cluster of mixed nodes can pull it — which is what all
// three of those report. `hack/check-image-pins.sh` re-resolves every pin in
// this repository and says which tags have moved; it reaches the registry,
// so it is run by hand rather than by CI. What CI holds is the other half:
// TestEveryImageConstantNamesADigest refuses a constant that names no
// digest at all.
const (
	// DefaultHelmImage runs the self-update job when the chart names no
	// other, and the install jobs of the addons the operator seeds. It is
	// pinned by digest rather than left on a moving tag: the job it runs
	// rewrites every object the platform is made of, under an account bound
	// to cluster-admin, so which helm does it is part of the release rather
	// than something inherited from whoever can push the tag.
	DefaultHelmImage = "alpine/helm:3.19.0@sha256:aef9b56f64e866207d9591d0abd8f6d767b36aadd12edf68f8a719716d9d29c9"

	// BuildkitImage runs the in-cluster builds.
	BuildkitImage = "moby/buildkit:v0.23.2-rootless@sha256:cab936745de5d673465948f1e93ff4d6e372bbe33f218afd3314eba45a6f85a9"

	// SBOMGeneratorImage is the scanner BuildKit runs over a finished image
	// to produce its bill of materials.
	//
	// It is pinned for the same reason the builder above is, and the reason
	// bites harder here: the tag BuildKit reaches for by default is
	// `stable-1`, a floating tag on an image nobody in this repository owns.
	// Evidence about an artifact should not change because somebody else's
	// tag moved overnight — and a scanner that changed under an installation
	// would produce a differently-shaped bill of materials for the same
	// image, which reads as the image having changed.
	//
	// It is also pulled on **every** build that asks for an SBOM: the build
	// pod is ephemeral, so nothing caches it between builds. That is the
	// cost the Kitchen object's `compliance.attestation.build.sbom` switch
	// exists to let an installation decline.
	SBOMGeneratorImage = "docker/buildkit-syft-scanner:1.12.0@sha256:ae4f3b554449e7e25548e7d8ccc029d17357348e30c6e3df01b92bc93654d6a9"

	// VendorSBOMGeneratorImage is the default generator of the observed
	// SBOM: Syft, run standalone.
	//
	// It is a different image from SBOMGeneratorImage, which is BuildKit's
	// scanner-protocol wrapper around the same tool and cannot be run
	// standalone. Pinned for the reason the other is: evidence about an
	// artifact should not change because somebody else's tag moved
	// overnight.
	VendorSBOMGeneratorImage = "anchore/syft:v1.18.1@sha256:b8c170b8e51bfc4779ec3ef4399942c57290f5ce76a9c3af564c9d00d4946a6b"

	// BuildpacksBuilderImage is the Cloud Native Buildpacks builder the
	// buildpacks strategy runs. Paketo's jammy "base" builder carries the
	// buildpacks for the languages a project is likely to be written in —
	// Node, Go, Python, Java, .NET — without the extra utilities of "full".
	//
	// The builder is what decides the contents of the image, so a moving tag
	// would mean the same commit rebuilt tomorrow produces something else.
	// Moving it means checking cnbUID and cnbGID in buildpacks.go, which are
	// this image's own unprivileged user.
	BuildpacksBuilderImage = "paketobuildpacks/builder-jammy-base:0.4.625@sha256:5799343cd316c1a03fa3ff7ab0915d9e6d134e95df4583016d70c6f5330d3898"

	// HerokuBuilderImage is the second Cloud Native Buildpacks builder, and
	// it exists for one repository shape the first cannot build: a Node
	// project whose dependencies are locked by pnpm.
	//
	// No Paketo builder carries a pnpm buildpack — not the pinned one, not
	// the current one, not "full"; `paketo-buildpacks/nodejs` has three
	// groups and they are yarn, npm and bare node. A pnpm repository falls
	// into the npm group, where `npm-install` detects on `package.json`
	// alone, reports `package-lock.json -> "Not found"` and runs
	// `npm install` anyway: the lockfile the application was tested against
	// is ignored, and on a tree npm's own resolver cannot read it dies inside
	// npm instead (#568). `heroku/nodejs` selects npm, yarn or pnpm from the
	// repository and installs with it.
	//
	// The tag names Heroku's base image rather than a build of the builder —
	// it moves — so the digest is what pins it, as it is for every image
	// here. Moving it means checking herokuUID and herokuGID in
	// buildpacks.go, which are this image's own unprivileged user, and it
	// means re-reading what the builder is *not* told: Heroku's buildpack
	// takes the package manager, the Node version and the build script from
	// `package.json` itself, so a variable added there is a variable nothing
	// reads.
	HerokuBuilderImage = "heroku/builder:24@sha256:e4ead318bb7027bf2b224569cf47b972c62226595d3dc2eba09f1ed912bb320d"

	// GitCloneImage fetches the commit a build builds. It is a container of
	// its own for both strategies, and for the same reason twice over: the
	// CNB lifecycle only ever builds a directory that is already on disk,
	// and BuildKit — which can fetch a git context itself — must not be
	// given a credential it would then hold while it runs the repository's
	// Dockerfile (#425).
	GitCloneImage = "alpine/git:v2.54.0@sha256:6f3b5029566da8e90b24945933dcd806be866b64b1e706f51828bf84faccf21b"

	// CloudflaredImage runs the optional tunnel.
	CloudflaredImage = "cloudflare/cloudflared:2025.8.1@sha256:b77d84e8704db38db22c22661cf7e56468c526e3a6a5fe9c8b7c151452fa1472"
)
