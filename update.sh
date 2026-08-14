#!/usr/bin/env bash
# update.sh — FoxRouters in-place updater (install.sh-based installs)
#
# Usage:
#   ./update.sh                 # update to latest GHCR image
#   ./update.sh --check         # check if update available (no pull)
#   ./update.sh --tag v1.6.4    # update to specific tag
#
# What it does:
#   1. Reads current install state from /etc/foxrouters/.env (install.sh layout)
#   2. Pulls new gateway image from GHCR
#   3. Compares digests — skips recreate if already latest
#   4. Recreates gateway container with SAME env/volumes/ports as install.sh
#   5. Waits for /health, prints new version
#
# Safe: Redis/ClickHouse/SQLite volumes are never touched. Gateway state
# (accounts, keys, credits) persists in Redis across updates.
#
# Requirements: install.sh-based install (docker, /etc/foxrouters/.env)

set -euo pipefail

CONFIG_DIR="/etc/foxrouters"
ENV_FILE="${CONFIG_DIR}/.env"
GATEWAY_PORT="${FOXROUTERS_PORT:-20130}"
IMAGE_DEFAULT="ghcr.io/rilspratama/foxrouters:latest"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
cyan()   { printf '\033[36m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }
info()   { printf '\033[36m[i]\033[0m %s\n' "$*"; }
ok()     { printf '\033[32m[✓]\033[0m %s\n' "$*"; }
err()    { printf '\033[31m[✗]\033[0m %s\n' "$*"; }

# ── Args ─────────────────────────────────────────────────────────────────────
CHECK_ONLY=0
IMAGE_TAG=""
for arg in "$@"; do
  case "$arg" in
    --check) CHECK_ONLY=1 ;;
    --tag=*) IMAGE_TAG="${arg#*=}" ;;
    --tag)   shift; IMAGE_TAG="${1:-}" ;;
    -h|--help)
      sed -n '2,20p' "$0"; exit 0 ;;
    *) err "unknown arg: $arg"; exit 1 ;;
  esac
done

IMAGE="${IMAGE_DEFAULT}"
if [[ -n "$IMAGE_TAG" ]]; then
  IMAGE="ghcr.io/rilspratama/foxrouters:${IMAGE_TAG}"
fi

# ── Preflight ────────────────────────────────────────────────────────────────
if ! command -v docker >/dev/null 2>&1; then
  err "docker not found"; exit 1
fi
if [[ ! -f "$ENV_FILE" ]]; then
  err "$ENV_FILE not found — is this an install.sh-based install?"
  err "For docker-compose installs, use: docker compose pull && docker compose up -d"
  exit 1
fi
if ! docker ps --format '{{.Names}}' | grep -qx foxrouters; then
  err "container 'foxrouters' not running — start it first"
  exit 1
fi

# shellcheck disable=SC1090
source "$ENV_FILE"
: "${REDIS_PASSWORD:?REDIS_PASSWORD missing in .env}"

# ── Current state ────────────────────────────────────────────────────────────
CURRENT_IMAGE="$(docker inspect foxrouters --format '{{.Config.Image}}')"
CURRENT_DIGEST="$(docker inspect foxrouters --format '{{.Image}}')"
CURRENT_VERSION="$(curl -sf --max-time 5 http://127.0.0.1:${GATEWAY_PORT}/health 2>/dev/null | grep -oP '"version":"[^"]+"' | cut -d'"' -f4 || echo '?')"

info "Current: image=${CURRENT_IMAGE} version=${CURRENT_VERSION}"
info "Target:  image=${IMAGE}"

# ── Pull ─────────────────────────────────────────────────────────────────────
if [[ "$CHECK_ONLY" == "1" ]]; then
  # Try buildx imagetools first, fallback to docker manifest inspect
  REMOTE_DIGEST="$(docker buildx imagetools inspect "$IMAGE" --format '{{.Manifest.Digest}}' 2>/dev/null || true)"
  if [[ -z "$REMOTE_DIGEST" ]]; then
    REMOTE_DIGEST="$(docker manifest inspect "$IMAGE" 2>/dev/null | grep -oP '"digest"\s*:\s*"\Ksha256:[a-f0-9]+' | head -1 || true)"
  fi
  if [[ -z "$REMOTE_DIGEST" ]]; then
    yellow "  (could not resolve remote digest — docker manifest inspect needs experimental CLI or network issue)"
    yellow "  Skipping check — run ./update.sh to pull + compare"
    exit 0
  fi
  LOCAL_DIGEST="$(docker image inspect "$IMAGE" --format '{{.RepoDigests}}' 2>/dev/null | grep -oP 'sha256:[a-f0-9]+' | head -1 || true)"
  if [[ -n "$LOCAL_DIGEST" && "$LOCAL_DIGEST" == "$REMOTE_DIGEST" ]]; then
    ok "Already up to date ($REMOTE_DIGEST)"
  else
    yellow "Update available: local=${LOCAL_DIGEST:-none} remote=$REMOTE_DIGEST"
    yellow "Run: ./update.sh"
  fi
  exit 0
fi

info "Pulling ${IMAGE}..."
docker pull "$IMAGE" >/dev/null

NEW_DIGEST="$(docker image inspect "$IMAGE" --format '{{.Id}}')"
if [[ "$NEW_DIGEST" == "$CURRENT_DIGEST" && "$CURRENT_IMAGE" == "$IMAGE" ]]; then
  ok "Already running latest image (${NEW_DIGEST:7:12}) — nothing to do"
  exit 0
fi

# ── Recreate gateway (mirror install.sh docker run flags) ───────────────────
info "Recreating gateway container..."

ENV_ARGS=(
  --env-file "$ENV_FILE"
  -e REDIS_ADDR=redis:6379
  -e LOG_BACKEND=sqlite
  -e LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db
  -e PORT=20130
)

docker rm -f foxrouters >/dev/null
docker run -d \
  --name foxrouters \
  --network foxrouters-net \
  -p "127.0.0.1:${GATEWAY_PORT}:20130" \
  -v foxrouters-sqlite-data:/var/lib/foxrouters \
  --restart unless-stopped \
  "${ENV_ARGS[@]}" \
  "$IMAGE" >/dev/null

# ── Wait for health ──────────────────────────────────────────────────────────
info "Waiting for health..."
NEW_VERSION="?"
for i in $(seq 1 30); do
  if HEALTH="$(curl -sf --max-time 2 http://127.0.0.1:${GATEWAY_PORT}/health 2>/dev/null)"; then
    NEW_VERSION="$(echo "$HEALTH" | grep -oP '"version":"[^"]+"' | cut -d'"' -f4 || echo '?')"
    ok "Healthy after ${i}s — version: ${NEW_VERSION}"
    break
  fi
  printf "."; sleep 1
done

if [[ "$NEW_VERSION" == "?" ]]; then
  err "Gateway not healthy after 30s — check: docker logs foxrouters"
  exit 1
fi

echo ""
bold "═══════════════════════════════════════════════════════════════"
green "  FoxRouters updated: ${CURRENT_VERSION} → ${NEW_VERSION}"
bold "═══════════════════════════════════════════════════════════════"
echo "  Image:      ${IMAGE}"
echo "  Dashboard:  http://127.0.0.1:${GATEWAY_PORT}/dashboard"
echo ""
