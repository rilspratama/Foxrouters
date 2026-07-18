#!/usr/bin/env bash
# deploy.sh — FoxRouters one-command deploy (no clone needed)
#
# Usage:
#   curl -sLO https://raw.githubusercontent.com/rilspratama/Foxrouters/master/deploy.sh
#   chmod +x deploy.sh
#   ./deploy.sh
#
# Downloads docker-compose.ghcr.yml, starts stack, captures bootstrap key.

set -euo pipefail

COMPOSE_FILE="docker-compose.ghcr.yml"
COMPOSE_URL="https://raw.githubusercontent.com/rilspratama/Foxrouters/master/${COMPOSE_FILE}"
KEY_FILE="bootstrap-key.txt"
SERVICE_NAME="foxrouters"
KEY_PATTERN="gw-[a-f0-9]{64}"

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
cyan()   { printf '\033[36m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }

cd "$(dirname "$0")"

# Check docker
if ! command -v docker &>/dev/null || ! docker compose version &>/dev/null; then
  red "✗ Docker + Docker Compose v2 required."
  echo "  Install: https://docs.docker.com/get-docker"
  exit 1
fi

# Download compose file if missing
if [[ ! -f "$COMPOSE_FILE" ]]; then
  echo "Downloading ${COMPOSE_FILE}..."
  curl -sLO "$COMPOSE_URL"
fi

PORT="20130"

# If key already captured, just start + show URL
if [[ -f "$KEY_FILE" ]]; then
  yellow "ℹ Bootstrap key already captured"
  echo "  Starting stack..."
  docker compose -f "$COMPOSE_FILE" up -d 2>&1 | sed 's/^/  /'
  # Wait healthy
  printf "  Waiting for gateway "
  for i in $(seq 1 30); do
    curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1 && { green "✓"; break; }
    printf "."; sleep 1
  done
  echo ""
  bold "═══════════════════════════════════════════════════════════════"
  green "  ✓ FoxRouters is running"
  bold "═══════════════════════════════════════════════════════════════"
  echo ""
  cyan "  Login:  http://localhost:$PORT/login"
  echo ""
  yellow "  Key:    $(cat $KEY_FILE)"
  echo ""
  echo "  Commands:"
  echo "    docker compose -f $COMPOSE_FILE logs -f foxrouters   # logs"
  echo "    docker compose -f $COMPOSE_FILE down                 # stop"
  echo "    docker compose -f $COMPOSE_FILE down -v              # stop + wipe data"
  bold "═══════════════════════════════════════════════════════════════"
  exit 0
fi

# Fresh deploy — start + capture key
bold "🦊 FoxRouters Deploy (ghcr.io)"
echo ""

echo "  Pulling images + starting stack..."
docker compose -f "$COMPOSE_FILE" up -d 2>&1 | sed 's/^/  /'
echo ""

# Wait healthy
printf "  Waiting for gateway "
for i in $(seq 1 60); do
  if curl -sf "http://localhost:$PORT/health" >/dev/null 2>&1; then
    green "✓ healthy (${i}s)"
    break
  fi
  printf "."; sleep 1
done
echo ""

# Capture bootstrap key
printf "  Capturing bootstrap key"
KEY=""
for i in $(seq 1 15); do
  KEY=$(docker compose -f "$COMPOSE_FILE" logs "$SERVICE_NAME" 2>/dev/null | grep -oE "$KEY_PATTERN" | head -1)
  [[ -n "$KEY" ]] && { echo ""; break; }
  printf "."; sleep 1
done
echo ""

if [[ -z "$KEY" ]]; then
  yellow "⚠ Bootstrap key not found in logs."
  echo "  Redis may already have keys from a previous deploy."
  echo "  To reset: docker compose -f $COMPOSE_FILE down -v && rm -f $KEY_FILE && ./$0"
  exit 1
fi

# Save key
echo "$KEY" > "$KEY_FILE"
chmod 600 "$KEY_FILE"

echo ""
bold "═══════════════════════════════════════════════════════════════"
bold "  🔑 Admin Bootstrap Key"
bold "═══════════════════════════════════════════════════════════════"
echo ""
cyan "  Key:    $KEY"
echo ""
cyan "  Login:  http://localhost:$PORT/login"
echo ""
yellow "  Saved:  $KEY_FILE (chmod 600)"
echo "          Delete after first login:"
echo "            rm $KEY_FILE"
echo ""
echo "  Commands:"
echo "    docker compose -f $COMPOSE_FILE logs -f foxrouters   # logs"
echo "    docker compose -f $COMPOSE_FILE down                 # stop"
echo "    docker compose -f $COMPOSE_FILE down -v              # stop + wipe data"
bold "═══════════════════════════════════════════════════════════════"
