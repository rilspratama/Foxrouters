#!/usr/bin/env bash
# start.sh — FoxRouters one-command deploy
#
# Usage:
#   ./start.sh            # start stack, capture bootstrap key if first boot
#   ./start.sh --reset    # wipe Redis volume + restart (re-bootstraps new key)
#   ./start.sh --stop     # stop stack
#   ./start.sh --status   # show status
#   ./start.sh --logs     # tail logs
#
# Bootstrap key is captured from container logs on first boot and saved to
# ./bootstrap-key.txt (chmod 600). On subsequent starts, Redis already has
# the key — start.sh just shows the login URL.
#
# Requirements: docker, docker compose

set -euo pipefail

COMPOSE_FILE="docker-compose.yml"
KEY_FILE="bootstrap-key.txt"
SERVICE_NAME="foxrouters"
KEY_PATTERN="gw-[a-f0-9]{64}"
KEY_LOG_PATTERN="Key: (gw-[a-f0-9]{64})"

cd "$(dirname "$0")"

# Colors
red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
cyan()   { printf '\033[36m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }

usage() {
  cat <<EOF
$(bold "FoxRouters Deploy Script")

$(bold "Usage:")
  ./start.sh              Start stack, capture bootstrap key on first boot
  ./start.sh --reset      Wipe Redis volume + restart (generates new bootstrap key)
  ./start.sh --stop       Stop stack
  ./start.sh --status     Show container status
  ./start.sh --logs       Tail live logs
  ./start.sh --key        Show current bootstrap key (if captured)
  ./start.sh --help       Show this help

$(bold "Files:")
  $KEY_FILE       Bootstrap admin key (chmod 600, gitignored)
  $COMPOSE_FILE   Docker Compose config
  .env            Environment variables (copy from .env.example)

$(bold "First-time setup:")
  cp .env.example .env   # then edit .env with real values
  ./start.sh             # starts Redis + ClickHouse + Gateway, captures key
  cat $KEY_FILE          # → gw-xxxxxxxxxxxx...
  # Open http://localhost:20130/login, paste key
EOF
}

check_prereqs() {
  if ! command -v docker &>/dev/null; then
    red "✗ docker not found. Install: https://docs.docker.com/get-docker"
    exit 1
  fi
  if ! docker compose version &>/dev/null; then
    red "✗ docker compose not found (v2+ required)."
    exit 1
  fi
  if [[ ! -f "$COMPOSE_FILE" ]]; then
    red "✗ $COMPOSE_FILE not found. Run this script from the project root."
    exit 1
  fi
  # .env is required by docker-compose env_file directive, but for the
  # default stack (Redis+CH+gw in compose) it can be empty — compose
  # overrides REDIS_ADDR/CLICKHOUSE_ADDR/PORT in the environment: block.
  if [[ ! -f ".env" ]]; then
    yellow "ℹ .env not found — creating empty one (defaults work for docker-compose)"
    touch .env
  fi
}

get_port() {
  # Extract PORT from .env or default 20130
  local port
  port=$(grep -E "^PORT=" .env 2>/dev/null | cut -d= -f2 | tr -d ' "' || true)
  echo "${port:-20130}"
}

extract_key_from_logs() {
  # Parse bootstrap key from gateway container logs
  # Format: "║  Key: gw-<64 hex>"
  docker compose logs "$SERVICE_NAME" 2>/dev/null \
    | grep -oE "$KEY_PATTERN" \
    | head -1
}

wait_for_healthy() {
  local port="$1"
  local max=30
  local i=0
  printf "  Waiting for gateway on :%s " "$port"
  while (( i < max )); do
    if curl -sf "http://localhost:$port/health" >/dev/null 2>&1; then
      green "✓ healthy"
      return 0
    fi
    printf "."
    sleep 1
    ((i++))
  done
  red "✗ timeout (gateway not responding after ${max}s)"
  return 1
}

cmd_start() {
  check_prereqs
  local port
  port=$(get_port)

  bold "🦊 FoxRouters Deploy"
  echo "  Port: $port"
  echo ""

  # If key already captured, just start + show URL
  if [[ -f "$KEY_FILE" ]]; then
    yellow "ℹ Bootstrap key already captured ($KEY_FILE)"
    echo "  Starting stack..."
    docker compose up -d 2>&1 | sed 's/^/  /'
    wait_for_healthy "$port" || return 1
    echo ""
    show_login_url "$port"
    return 0
  fi

  # Fresh start — need to capture bootstrap key
  echo "  Starting stack (first boot — will capture bootstrap key)..."
  docker compose up -d 2>&1 | sed 's/^/  /'
  echo ""

  # Wait for gateway to be healthy
  wait_for_healthy "$port" || {
    red "Gateway failed to start. Check logs:"
    docker compose logs "$SERVICE_NAME" 2>&1 | tail -20 | sed 's/^/  /'
    return 1
  }

  # Capture bootstrap key from logs
  echo ""
  printf "  Capturing bootstrap key from logs..."
  local key=""
  local max_wait=15
  local i=0
  while (( i < max_wait )); do
    key=$(extract_key_from_logs)
    if [[ -n "$key" ]]; then
      break
    fi
    printf "."
    sleep 1
    ((i++))
  done
  echo ""

  if [[ -z "$key" ]]; then
    yellow "⚠ Bootstrap key not found in logs."
    yellow "  Possible reasons:"
    echo "    - Redis volume already had keys from previous deploy"
    echo "    - GATEWAY_AUTH_DISABLE=1 is set (no key needed)"
    echo ""
    yellow "  To reset: ./start.sh --reset"
    return 1
  fi

  # Save key to file (chmod 600)
  echo "$key" > "$KEY_FILE"
  chmod 600 "$KEY_FILE"

  echo ""
  green "✓ Bootstrap key captured!"
  echo ""
  show_key "$port" "$key"
}

show_key() {
  local port="$1" key="$2"
  bold "═══════════════════════════════════════════════════════════════"
  bold "  🔑 Admin Bootstrap Key"
  bold "═══════════════════════════════════════════════════════════════"
  echo ""
  cyan "  Key:    $key"
  echo ""
  cyan "  Login:  http://localhost:$port/login"
  echo ""
  yellow "  Saved:  $KEY_FILE (chmod 600)"
  echo "          Delete after first login (Redis persists it):"
  echo "            rm $KEY_FILE"
  echo ""
  bold "═══════════════════════════════════════════════════════════════"
}

show_login_url() {
  local port="$1"
  echo ""
  bold "═══════════════════════════════════════════════════════════════"
  green "  ✓ FoxRouters is running"
  bold "═══════════════════════════════════════════════════════════════"
  echo ""
  cyan "  Login:  http://localhost:$port/login"
  echo ""
  yellow "  Key file: $KEY_FILE"
  echo "    cat $KEY_FILE    # view key"
  echo ""
  echo "  Commands:"
  echo "    ./start.sh --status    # check containers"
  echo "    ./start.sh --logs      # tail logs"
  echo "    ./start.sh --stop      # stop stack"
  bold "═══════════════════════════════════════════════════════════════"
}

cmd_reset() {
  bold "🔄 Resetting FoxRouters (wipes Redis + regenerates key)"
  echo ""
  yellow "⚠ This will:"
  echo "  - Stop all containers"
  echo "  - Delete Redis volume (all keys, accounts, history wiped)"
  echo "  - Delete local $KEY_FILE"
  echo "  - Restart with fresh bootstrap key"
  echo ""
  read -rp "Continue? [y/N] " confirm
  if [[ "$confirm" != "y" && "$confirm" != "Y" ]]; then
    echo "Aborted."
    exit 0
  fi

  docker compose down -v 2>&1 | sed 's/^/  /'
  rm -f "$KEY_FILE"
  echo ""
  green "✓ Reset complete. Starting fresh..."
  echo ""
  cmd_start
}

cmd_stop() {
  echo "Stopping FoxRouters..."
  docker compose down 2>&1 | sed 's/^/  /'
  green "✓ Stopped (volumes preserved)."
  echo "  Restart: ./start.sh"
}

cmd_status() {
  bold "FoxRouters Status"
  echo ""
  docker compose ps 2>&1 | sed 's/^/  /'
  local port
  port=$(get_port)
  echo ""
  if curl -sf "http://localhost:$port/health" >/dev/null 2>&1; then
    echo -n "  Health: "
    green "✓ healthy"
    curl -s "http://localhost:$port/health" | python3 -c "
import sys,json
d=json.load(sys.stdin)
print(f'  Service: {d.get(\"service\",\"?\")} v{d.get(\"version\",\"?\")} ({d.get(\"status\",\"?\")})')
" 2>/dev/null || true
  else
    echo -n "  Health: "
    red "✗ not responding"
  fi
  echo ""
  if [[ -f "$KEY_FILE" ]]; then
    yellow "  Key: $KEY_FILE (captured)"
  else
    yellow "  Key: not captured (run ./start.sh or ./start.sh --reset)"
  fi
}

cmd_logs() {
  docker compose logs -f --tail=50 "$SERVICE_NAME"
}

cmd_key() {
  if [[ -f "$KEY_FILE" ]]; then
    echo "Bootstrap key (from $KEY_FILE):"
    cat "$KEY_FILE"
  else
    yellow "⚠ No captured key. Run ./start.sh to capture from first boot."
    echo ""
    echo "Or extract manually from container logs:"
    echo "  docker compose logs foxrouters | grep 'Key: gw-'"
    exit 1
  fi
}

# ─── Main ──────────────────────────────────────────────────────────
case "${1:-start}" in
  start|"")      cmd_start ;;
  --reset|reset) cmd_reset ;;
  --stop|stop)   cmd_stop ;;
  --status|status) cmd_status ;;
  --logs|logs)   cmd_logs ;;
  --key|key)     cmd_key ;;
  --help|-h|help) usage ;;
  *)
    red "Unknown command: $1"
    usage
    exit 1
    ;;
esac
