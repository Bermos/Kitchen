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

# Build the manager and gate binaries.
#
# This stage runs on the build host's architecture and cross-compiles to the
# target, rather than running an emulated toolchain. Without the `--platform`
# pin, buildx pulls a target-architecture golang image and runs it under QEMU:
# building linux/arm64 that way took 33 minutes of a 43-minute release, against
# roughly 3 for the same two binaries compiled natively. Nothing here needs the
# target's architecture — CGO is off, so the Go toolchain cross-compiles from
# GOARCH alone.
FROM --platform=$BUILDPLATFORM docker.io/golang:1.23 AS builder
ARG TARGETOS
ARG TARGETARCH

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
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
# The forward-auth gate protected previews are routed through. It is a
# separate process with a separate Deployment, but the same source tree and
# the same release, so it rides along in this image.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o gate cmd/gate/main.go

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/gate .
USER 65532:65532

ENTRYPOINT ["/manager"]
