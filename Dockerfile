# Build the dashboard, so the operator can embed and serve it: one image
# carries the whole control plane, and the chart has nothing extra to ship.
# The output is static JS and CSS, identical for every target, so this stage
# pins itself to the build host and runs once no matter how many platforms
# are being built.
FROM --platform=$BUILDPLATFORM docker.io/node:22-alpine AS ui-builder
WORKDIR /ui
COPY ui/package.json ui/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY ui/ ./
RUN npm run build

# Build the manager and the commands that ride along in its image.
#
# This stage runs on the build host's architecture and cross-compiles to the
# target, rather than running an emulated toolchain. Without the `--platform`
# pin, buildx pulls a target-architecture golang image and runs it under QEMU:
# building linux/arm64 that way took 33 minutes of a 43-minute release, against
# three and a half for the same two binaries cross-compiled. Nothing here needs
# the target's architecture — CGO is off, so the Go toolchain cross-compiles
# from GOARCH alone, and the arm64 image can be built on a host with no arm64
# emulation installed at all.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH
# The release being built, stamped into both binaries so the dashboard can show
# which one it is running. It has to be handed in: this stage copies the source
# it compiles and nothing else, so there is no .git in here to ask. The publish
# workflow passes the version release-please decided on, `make docker-build`
# passes what `git describe` says, and a bare `docker build` leaves it at the
# default.
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/
# The dashboard rides into the manager via go:embed.
COPY --from=ui-builder /ui/dist/ internal/ui/dist/

# TARGETOS and TARGETARCH are what buildx was asked to produce, so the binary
# matches the image it is packaged into even though the compiler is not running
# on that architecture. They are empty for a plain `docker build` with no
# `--platform`, which is what leaves GOARCH unset and builds for the host.
#
# `go build -a` is deliberately absent. It forces a rebuild of every dependency
# including the standard library, and is scaffold default left over from before
# Go 1.10, when it was how you got a static binary out of CGO_ENABLED=0 — the
# build cache has handled that correctly since. Dropping it produced
# byte-identical binaries for both architectures while halving the compile
# step, because the other binaries share nearly all of their dependencies with
# the manager and can now reuse what the manager build just compiled. Re-adding
# it would buy nothing and cost that again.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X github.com/Bermos/Kitchen/internal/version.Version=${VERSION}" \
    -o manager cmd/main.go
# The backup and restore commands. They ride in this image for the same reason
# the gate below does — same source tree, same release — and for one more: the
# archive and the code that reads it have to agree about the schema underneath,
# and the version stamped here is what decides whether they do.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X github.com/Bermos/Kitchen/internal/version.Version=${VERSION}" \
    -o backup cmd/backup/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X github.com/Bermos/Kitchen/internal/version.Version=${VERSION}" \
    -o restore cmd/restore/main.go
# The forward-auth gate protected previews are routed through. It is a
# separate process with a separate Deployment, but the same source tree and
# the same release, so it rides along in this image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X github.com/Bermos/Kitchen/internal/version.Version=${VERSION}" \
    -o gate cmd/gate/main.go
# The publisher that carries a quality gate's findings out of the pod that
# produced them. It runs beside an image somebody else wrote, in an
# application's namespace, and does one thing: read a file and store it in the
# registry. Same source tree, same release, same image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -ldflags "-X github.com/Bermos/Kitchen/internal/version.Version=${VERSION}" \
    -o qualitygate cmd/qualitygate/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/backup .
COPY --from=builder /workspace/restore .
COPY --from=builder /workspace/gate .
COPY --from=builder /workspace/qualitygate .
USER 65532:65532

ENTRYPOINT ["/manager"]
