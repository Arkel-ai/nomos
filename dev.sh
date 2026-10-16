#!/usr/bin/env bash
set -e

ROOT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Create .env files from templates if missing
if [ ! -f "$ROOT_DIR/api/.env" ]; then
  echo "==> Creating api/.env from template..."
  cp "$ROOT_DIR/api/.env.template" "$ROOT_DIR/api/.env"
fi
if [ ! -f "$ROOT_DIR/front/.env" ]; then
  echo "==> Creating front/.env from template..."
  cp "$ROOT_DIR/front/.env.template" "$ROOT_DIR/front/.env"
  echo "==> Bootstrap admin: admin@example.com / NOMOS2026 (change it in front/.env)"
fi

# ---------------------------------------------------------------------------
# Local Postgres (pgvector + unaccent).
#
# Nomos needs pgvector for halfvec columns and unaccent for the french text
# search configuration (docs/PLAN.md §6.2) — neither ships with a stock
# Postgres. Started automatically when DATABASE_URL is unset, or on demand
# with `./dev.sh --db`. Fully idempotent: an existing container is reused,
# never recreated, so local data survives.
# ---------------------------------------------------------------------------
PG_CONTAINER="nomos-postgres"
PG_IMAGE="pgvector/pgvector:pg17"
PG_PORT=5433
LOCAL_DATABASE_URL="postgres://nomos:nomos@localhost:${PG_PORT}/nomos?sslmode=disable"

WANT_LOCAL_DB=false
for arg in "$@"; do
  case "$arg" in
    --db) WANT_LOCAL_DB=true ;;
    *) echo "Unknown option: $arg (usage: ./dev.sh [--db])" ; exit 1 ;;
  esac
done
if [ -z "${DATABASE_URL:-}" ]; then
  WANT_LOCAL_DB=true
fi

start_local_db() {
  if ! command -v docker &>/dev/null; then
    echo "==> docker not found — skipping the local Postgres (set DATABASE_URL yourself)"
    return 0
  fi

  if [ -n "$(docker ps -q -f "name=^${PG_CONTAINER}$" 2>/dev/null)" ]; then
    echo "==> Postgres container '${PG_CONTAINER}' already running — reusing it"
  elif [ -n "$(docker ps -aq -f "name=^${PG_CONTAINER}$" 2>/dev/null)" ]; then
    echo "==> Starting existing Postgres container '${PG_CONTAINER}'..."
    docker start "$PG_CONTAINER" >/dev/null
  else
    echo "==> Creating Postgres container '${PG_CONTAINER}' (${PG_IMAGE}) on port ${PG_PORT}..."
    docker run -d --name "$PG_CONTAINER" \
      -e POSTGRES_USER=nomos -e POSTGRES_PASSWORD=nomos -e POSTGRES_DB=nomos \
      -p "${PG_PORT}:5432" \
      "$PG_IMAGE" >/dev/null
  fi

  echo -n "==> Waiting for Postgres"
  for _ in $(seq 1 60); do
    if docker exec "$PG_CONTAINER" pg_isready -U nomos -d nomos >/dev/null 2>&1; then
      echo " — ready"
      # CREATE EXTENSION needs superuser. In production the app user
      # usually cannot, so the database must be created with both
      # extensions enabled (docs/PLAN.md §15).
      docker exec "$PG_CONTAINER" psql -U nomos -d nomos -v ON_ERROR_STOP=1 \
        -c 'CREATE EXTENSION IF NOT EXISTS vector' \
        -c 'CREATE EXTENSION IF NOT EXISTS unaccent' >/dev/null \
        || echo "WARNING: could not create the vector/unaccent extensions"
      export DATABASE_URL="$LOCAL_DATABASE_URL"
      echo "==> DATABASE_URL=${LOCAL_DATABASE_URL}"
      return 0
    fi
    echo -n "."
    sleep 1
  done
  echo
  echo "WARNING: Postgres did not become ready — continuing without a database"
}

if [ "$WANT_LOCAL_DB" = true ]; then
  start_local_db
fi

# Ensure node/npm + go are available
if ! command -v npm &>/dev/null; then
  echo "Error: npm not found. Install Node.js first: https://nodejs.org"
  exit 1
fi
if ! command -v go &>/dev/null; then
  echo "Error: go not found. Install Go 1.26+: https://go.dev/dl/"
  exit 1
fi

# Resolve `air` (live-reload). Install on demand if missing.
GOBIN="$(go env GOPATH)/bin"
AIR="$(command -v air || true)"
if [ -z "$AIR" ] && [ -x "$GOBIN/air" ]; then
  AIR="$GOBIN/air"
fi
if [ -z "$AIR" ]; then
  echo "==> Installing air (Go live-reload) into $GOBIN..."
  GOTOOLCHAIN=auto go install github.com/air-verse/air@latest
  AIR="$GOBIN/air"
fi

# Install dependencies
echo "==> Installing API dependencies..."
cd "$ROOT_DIR/api"
GOTOOLCHAIN=auto go mod download

echo "==> Installing frontend dependencies..."
cd "$ROOT_DIR/front"
npm install

# Apply Better Auth schema when a database is configured. Better Auth
# owns the user/session/account/verification/twoFactor tables; the Go API and
# Casbin manage their own tables on boot.
if [ -f "$ROOT_DIR/front/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  . "$ROOT_DIR/front/.env"
  set +a
fi
# front/.env is sourced with `set -a`, so an empty DATABASE_URL there would
# shadow the container we just started. Put it back.
if [ "$WANT_LOCAL_DB" = true ] && [ -z "${DATABASE_URL:-}" ] && \
   [ -n "$(docker ps -q -f "name=^${PG_CONTAINER}$" 2>/dev/null)" ]; then
  export DATABASE_URL="$LOCAL_DATABASE_URL"
fi
if [ -n "${DATABASE_URL:-}" ]; then
  echo "==> Applying Better Auth schema (DATABASE_URL is set)..."
  npx --yes @better-auth/cli@latest migrate --config auth.config.ts -y \
    || echo "WARNING: Better Auth migrate failed — check DATABASE_URL"
else
  echo "==> DATABASE_URL not set — skipping Better Auth migrate (auth/db endpoints return 503)"
fi

# Start backend (with hot reload) and frontend in parallel
echo "==> Starting API (port 8000, hot reload) and frontend (port 5173)..."
cd "$ROOT_DIR/api"
"$AIR" &
API_PID=$!

cd "$ROOT_DIR/front"
npm run dev &
FRONT_PID=$!

trap "kill $API_PID $FRONT_PID 2>/dev/null; exit" INT TERM
wait
