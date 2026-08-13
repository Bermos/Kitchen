# Kitchen's identity provider. Built from this directory:
#   docker build -t ghcr.io/bermos/kitchen-auth:dev auth
FROM docker.io/node:22-alpine AS builder
WORKDIR /app

# Dependencies first so a source change does not re-resolve the tree.
COPY package.json package-lock.json ./
RUN npm ci

COPY tsconfig.json ./
COPY src ./src
RUN npm run build

# The runtime tree, without anything only the compiler needed.
FROM docker.io/node:22-alpine AS deps
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
