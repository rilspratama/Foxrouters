#!/usr/bin/env bash
# dev.sh — FoxRouters isolated dev workflow
#
# WHY: `docker compose up --build` from a dev session REPLACES the production
# container (compose detects config/image change -> recreate -> if interrupted
# the new container is left stuck in `Created` and prod goes down).
#
# This script never touches the prod stack. It runs a FULLY ISOLATED dev
# stack with its own Redis (separate container, volume, port) and its own
# sqlite log DB:
#
#   dev redis  : foxrouters-dev-redis   (127.0.0.1:6381, volume foxrouters-dev-redis-data)
#   dev gateway: foxrouters-dev         (127.0.0.1:20131, volume foxrouters-dev-sqlite)
#   prod       : foxrouters-redis (6379) + foxrouters (20130)  ← NEVER touched
#
# Because dev uses its own Redis, the background workers (token refresh,
# credit sync, re-enable, health probe) are SAFE to run — they mutate the
# dev Redis, never the prod one. No rotating-refresh-token races, no shared
# disable state.
#
# NOTE: dev Redis starts EMPTY — you must seed accounts/keys if you want to
# test against real credentials. Prod data is NOT shared.
#
# Usage:
#   ./dev.sh            # build + up (both dev redis + dev gateway)
#   ./dev.sh build      # just rebuild the dev image
#   ./dev.sh up         # run/restart dev stack (redis + gateway)
#   ./dev.sh down       # stop + remove dev stack (prod untouched)
#   ./dev.sh logs       # tail dev gateway logs
#   ./dev.sh test       # go test -race + go vet (on host)
#
# Dev gateway:  http://127.0.0.1:20131/dashboard  (auth disabled)
# Prod gateway: http://127.0.0.1:20130  (never touched by this script)

set -euo pipefail

DEV_IMAGE="foxrouters:dev"
DEV_NAME="foxrouters-dev"
DEV_REDIS_NAME="foxrouters-dev-redis"
DEV_PORT="20131"
DEV_REDIS_PORT="6381"
NETWORK="foxrouters-dev-net"
SQLITE_VOL="foxrouters-dev-sqlite"
REDIS_VOL="foxrouters-dev-redis-data"
DEV_REDIS_PASS="foxrouters-dev-redis-pass"
ENV_FILE=".env"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
cyan()   { printf '\033[36m%s\033[0m\n' "$*"; }

cd "$(dirname "$0")"

ensure_network() {
  docker network inspect "$NETWORK" >/dev/null 2>&1 || {
    yellow "network $NETWORK not found — creating"
    docker network create "$NETWORK" >/dev/null
  }
}

dev_redis_up() {
  ensure_network
  if docker ps -a --format '{{.Names}}' | grep -qx "$DEV_REDIS_NAME"; then
    docker rm -f "$DEV_REDIS_NAME" >/dev/null 2>&1 || true
  fi
  docker volume create "$REDIS_VOL" >/dev/null 2>&1 || true
  cyan "═══ Starting dev redis ($DEV_REDIS_NAME → :$DEV_REDIS_PORT) ═══"
  docker run -d --name "$DEV_REDIS_NAME" \
    --network "$NETWORK" \
    -p "127.0.0.1:${DEV_REDIS_PORT}:6379" \
    -v "$REDIS_VOL:/data" \
    redis:7-alpine \
    redis-server --requirepass "$DEV_REDIS_PASS" --appendonly no >/dev/null
  # wait for redis ping
  for i in $(seq 1 15); do
    if docker exec "$DEV_REDIS_NAME" redis-cli -a "$DEV_REDIS_PASS" ping 2>/dev/null | grep -q PONG; then
      green "✓ dev redis ready (${i}s)"
      return 0
    fi
    sleep 1
  done
  red "✗ dev redis not ready — docker logs $DEV_REDIS_NAME"
  return 1
}

build() {
  cyan "═══ Build dev image ($DEV_IMAGE) ═══"
  docker build -t "$DEV_IMAGE" --build-arg VERSION=dev .
  green "✓ dev image built"
}

up() {
  dev_redis_up
  # kill any stale dev gateway container
  if docker ps -a --format '{{.Names}}' | grep -qx "$DEV_NAME"; then
    docker rm -f "$DEV_NAME" >/dev/null 2>&1 || true
  fi
  # ensure sqlite volume exists + owned by UID 1000 (foxrouters)
  docker volume create "$SQLITE_VOL" >/dev/null 2>&1 || true
  docker run --rm -v "$SQLITE_VOL:/var/lib/foxrouters" alpine \
    chown -R 1000:1000 /var/lib/foxrouters >/dev/null 2>&1 || true

  cyan "═══ Starting dev gateway ($DEV_NAME → :$DEV_PORT) ═══"
  docker run -d --name "$DEV_NAME" \
    --network "$NETWORK" \
    -p "127.0.0.1:${DEV_PORT}:20130" \
    -v "$SQLITE_VOL:/var/lib/foxrouters" \
    -e "PORT=20130" \
    -e "REDIS_ADDR=$DEV_REDIS_NAME:6379" \
    -e "REDIS_PASSWORD=$DEV_REDIS_PASS" \
    -e "LOG_BACKEND=sqlite" \
    -e "LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db" \
    -e "GATEWAY_AUTH_DISABLE=1" \
    -e "WORKERS_DISABLED=1" \
    -e "HEALTH_PROBES_DISABLED=1" \
    -e "CB_SELECTOR_MODE=sticky" \
    "$DEV_IMAGE" >/dev/null

  green "✓ dev gateway started"
  printf "  Waiting for health "
  for i in $(seq 1 30); do
    if curl -sf "http://127.0.0.1:${DEV_PORT}/health" >/dev/null 2>&1; then
      green "✓ healthy (${i}s)"
      cyan "  Dev gateway: http://127.0.0.1:${DEV_PORT}/dashboard  (auth disabled)"
      cyan "  Dev redis:   127.0.0.1:${DEV_REDIS_PORT} (pass: $DEV_REDIS_PASS)"
      cyan "  Logs:        docker logs -f $DEV_NAME"
      return 0
    fi
    printf "."; sleep 1
  done
  red "✗ dev gateway not healthy — check logs: docker logs $DEV_NAME"
  return 1
}

down() {
  cyan "═══ Stopping dev stack ═══"
  docker rm -f "$DEV_NAME" >/dev/null 2>&1 && green "✓ gateway removed" || yellow "  (no dev gateway)"
  docker rm -f "$DEV_REDIS_NAME" >/dev/null 2>&1 && green "✓ redis removed" || yellow "  (no dev redis)"
  # volumes are kept by default (data persists); add -v to wipe them
  if [[ "${1:-}" == "-v" ]]; then
    docker volume rm "$SQLITE_VOL" "$REDIS_VOL" >/dev/null 2>&1 && green "✓ volumes wiped"
  fi
}

logs() { docker logs -f "$DEV_NAME" 2>&1; }

test_all() {
  cyan "═══ go vet + go test -race ═══"
  go vet ./...
  go test -count=1 -race -timeout 120s ./...
  green "✓ tests pass"
}

case "${1:-all}" in
  build)  build ;;
  up)     up ;;
  down)   down ;;
  logs)   logs ;;
  test)   test_all ;;
  all)    build && up ;;
  *)      echo "usage: $0 [build|up|down|logs|test]"; exit 1 ;;
esac
