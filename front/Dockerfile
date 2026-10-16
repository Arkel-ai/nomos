# Build
FROM node:24-alpine AS build
WORKDIR /src
# Reproducible build: install the exact tree from the tracked lockfile.
# A floating `npm install` here once pulled a broken transitive dep that
# stopped TS preprocessing — keep the lockfile authoritative.
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY . .
RUN npm run build

# Run the SvelteKit Node server emitted by adapter-node.
# Better Auth (`/auth/*`) runs inside this server — it cannot be
# served as static files.
FROM node:24-alpine
WORKDIR /app

# Production deps only — the build stage installed devDependencies for
# the bundler; we just need runtime deps here (e.g. `pg`, `better-auth`).
COPY package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund

# adapter-node output: `build/index.js` is the entrypoint, plus the
# `client/` and `server/` chunks it loads at runtime.
COPY --from=build /src/build ./build

ENV NODE_ENV=production \
    HOST=0.0.0.0 \
    PORT=8080
EXPOSE 8080

# adapter-node honors $HOST/$PORT. DATABASE_URL and BETTER_AUTH_SECRET
# are injected at deploy time.
USER node
CMD ["node", "build/index.js"]
