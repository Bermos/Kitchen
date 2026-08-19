# Kitchen's identity provider. Built from this directory:
#   docker build -t ghcr.io/bermos/kitchen-auth:dev auth
#
# Both stages that run anything are pinned to the build host, and the final
# stage only copies files in, so a linux/arm64 image is built without emulating
# an instruction. That is not an optimisation: with the target-architecture
# node image and QEMU, `npm ci` on musl stopped completing at all — the auth
# image took 94 seconds for 0.5.0 and then hung for the full six-hour job limit
# for 0.5.1 and 0.6.0 with the same source, which held back the chart job that
# waits on it and left both releases with no published chart.
#
# Nothing in here needs the target's architecture. `tsc` emits the same
# JavaScript everywhere, and the dependency tree is pure JavaScript: no package
# in package-lock.json declares `cpu` or `os`, and none has an install script.
# A dependency that does not hold — anything with a native addon — has to be
# built for the target, and the honest way to do it is a native runner rather
# than putting the emulator back.

FROM --platform=$BUILDPLATFORM docker.io/node:22-alpine AS builder
WORKDIR /app

# Dependencies first so a source change does not re-resolve the tree.
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json ./
COPY src ./src
RUN npm run build

# The runtime tree, without anything only the compiler needed.
FROM --platform=$BUILDPLATFORM docker.io/node:22-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev && npm cache clean --force

FROM docker.io/node:22-alpine
WORKDIR /app
ENV NODE_ENV=production
COPY --from=deps /app/node_modules ./node_modules
COPY --from=builder /app/dist ./dist
COPY package.json ./

# The `node` user ships with the image; the chart pins the same uid.
USER 1000:1000
EXPOSE 8080

ENTRYPOINT ["node", "dist/src/main.js"]
