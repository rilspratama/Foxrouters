#!/usr/bin/env bash
# tunnel.sh — Cloudflare Tunnel management for FoxRouters
#
# Exposes the local FoxRouters gateway (foxrouters:20130 inside the
# foxrouters-net Docker network) via a Cloudflare Tunnel. Two modes:
#
#   quick — random *.trycloudflare.com URL, no Cloudflare account needed.
#           URL changes on every restart (not persistent).
#   named — custom domain via a persistent tunnel. Requires a Cloudflare
#           account, a zone, and a pre-existing cert.pem + tunnel credentials
#           JSON at /etc/foxrouters/cloudflared/ (see NAMED SETUP below).
#
# Usage:
#   ./tunnel.sh enable [--quick|--named]   Start tunnel (default: quick).
#   ./tunnel.sh disable                    Stop + remove the tunnel container.
#   ./tunnel.sh status                     Show container state + current URL.
#   ./tunnel.sh url                        Print current tunnel URL.
#   ./tunnel.sh restart                    Restart (keeps the same mode).
#   ./tunnel.sh logs [-f]                  Tail cloudflared container logs.
#
# NAMED SETUP (once, on host):
#   1. cloudflared tunnel login
#        → writes ~/.cloudflared/cert.pem
#   2. cloudflared tunnel create foxrouters
#        → writes ~/.cloudflared/<tunnel-id>.json
#   3. Copy both to the shared config dir:
#        sudo mkdir -p /etc/foxrouters/cloudflared
#        sudo cp ~/.cloudflared/cert.pem            /etc/foxrouters/cloudflared/
#        sudo cp ~/.cloudflared/<tunnel-id>.json    /etc/foxrouters/cloudflared/
#   4. Write /etc/foxrouters/cloudflared/config.yml:
#        tunnel: <tunnel-id>
#        credentials-file: /etc/cloudflared/<tunnel-id>.json
#        ingress:
#          - hostname: gateway.example.com
#            service: http://foxrouters:20130
#          - service: http_status:404
#   5. cloudflared tunnel route dns foxrouters gateway.example.com
#   6. ./tunnel.sh enable --named
#
set -euo pipefail

# ── Config ──────────────────────────────────────────────────────────────────
CONTAINER="foxrouters-tunnel"
IMAGE="cloudflare/cloudflared:latest"
NETWORK="foxrouters-net"
UPSTREAM="http://foxrouters:20130"
CONFIG_DIR="/etc/foxrouters/cloudflared"
STATE_FILE="${CONFIG_DIR}/mode"   # remembers last-used mode for `restart`

# ── Colors ──────────────────────────────────────────────────────────────────
red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }
info()   { printf '\033[36m[i]\033[0m %s\n' "$*"; }
ok()     { printf '\033[32m[✓]\033[0m %s\n' "$*"; }
err()    { printf '\033[31m[✗]\033[0m %s\n' "$*"; }

need_docker() {
    if ! command -v docker &>/dev/null; then
        err "Docker not found. Install it first."
        exit 1
    fi
    if ! docker info &>/dev/null; then
        err "Docker daemon not running. systemctl start docker"
        exit 1
    fi
}

ensure_network() {
    if ! docker network inspect "${NETWORK}" &>/dev/null; then
        err "Docker network '${NETWORK}' not found. Run install.sh first."
        exit 1
    fi
}

is_running() {
    [[ "$(docker inspect -f '{{.State.Running}}' "${CONTAINER}" 2>/dev/null || echo false)" == "true" ]]
}

exists() {
    docker inspect "${CONTAINER}" &>/dev/null
}

save_mode() {
    mkdir -p "${CONFIG_DIR}"
    echo "$1" > "${STATE_FILE}"
}

load_mode() {
    if [[ -r "${STATE_FILE}" ]]; then
        cat "${STATE_FILE}"
    else
        echo "quick"
    fi
}

# Extract the *.trycloudflare.com URL from cloudflared logs. Quick tunnels
# print a banner like "https://<slug>.trycloudflare.com" once the tunnel is
# established — polls up to ~30s.
capture_quick_url() {
    local url=""
    for _ in $(seq 1 30); do
        url=$(docker logs "${CONTAINER}" 2>&1 \
              | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' \
              | head -1 || true)
        if [[ -n "${url}" ]]; then
            echo "${url}"
            return 0
        fi
        sleep 1
    done
    return 1
}

start_quick() {
    info "Starting quick tunnel → ${UPSTREAM}"
    docker rm -f "${CONTAINER}" 2>/dev/null || true
    docker run -d \
        --name "${CONTAINER}" \
        --network "${NETWORK}" \
        --restart unless-stopped \
        "${IMAGE}" \
        tunnel --no-autoupdate --url "${UPSTREAM}" >/dev/null
    save_mode "quick"
    ok "Container '${CONTAINER}' started (quick mode)"

    info "Waiting for tunnel URL (up to 30s)..."
    if URL=$(capture_quick_url); then
        echo ""
        bold "  Tunnel URL: ${URL}"
        echo ""
        yellow "  ⚠  Quick tunnels are ephemeral — the URL changes on every restart."
        yellow "     Use './tunnel.sh enable --named' for a persistent custom domain."
    else
        err "Could not capture tunnel URL. Check: docker logs ${CONTAINER}"
        exit 1
    fi
}

start_named() {
    info "Starting named tunnel from ${CONFIG_DIR}"
    if [[ ! -d "${CONFIG_DIR}" ]]; then
        err "Config dir ${CONFIG_DIR} missing. See NAMED SETUP in tunnel.sh."
        exit 1
    fi
    if [[ ! -f "${CONFIG_DIR}/config.yml" ]]; then
        err "${CONFIG_DIR}/config.yml missing. See NAMED SETUP in tunnel.sh."
        exit 1
    fi
    if ! ls "${CONFIG_DIR}"/*.json &>/dev/null; then
        err "No <tunnel-id>.json credentials found in ${CONFIG_DIR}."
        err "Run 'cloudflared tunnel create foxrouters' and copy the JSON here."
        exit 1
    fi

    docker rm -f "${CONTAINER}" 2>/dev/null || true
    docker run -d \
        --name "${CONTAINER}" \
        --network "${NETWORK}" \
        --restart unless-stopped \
        -v "${CONFIG_DIR}:/etc/cloudflared:ro" \
        "${IMAGE}" \
        tunnel --no-autoupdate --config /etc/cloudflared/config.yml run >/dev/null
    save_mode "named"
    ok "Container '${CONTAINER}' started (named mode)"

    # Best-effort: pull the hostname out of config.yml so the user sees the URL
    # without hunting through cloudflared logs.
    HOSTNAME=$(grep -E '^\s*-?\s*hostname:' "${CONFIG_DIR}/config.yml" \
               | head -1 | awk -F: '{print $2}' | tr -d ' ' || true)
    if [[ -n "${HOSTNAME}" ]]; then
        echo ""
        bold "  Tunnel URL: https://${HOSTNAME}"
        echo ""
    else
        info "Tunnel started. Check the hostname(s) in ${CONFIG_DIR}/config.yml"
    fi
}

cmd_enable() {
    need_docker
    ensure_network

    local mode="quick"
    case "${1:-}" in
        --quick|quick|"") mode="quick" ;;
        --named|named)    mode="named" ;;
        *) err "Unknown mode: $1 (use --quick or --named)"; exit 1 ;;
    esac

    if [[ "${mode}" == "quick" ]]; then
        start_quick
    else
        start_named
    fi
}

cmd_disable() {
    need_docker
    if ! exists; then
        info "No tunnel container present."
        return 0
    fi
    info "Stopping tunnel..."
    docker rm -f "${CONTAINER}" >/dev/null
    ok "Tunnel disabled."
}

cmd_status() {
    need_docker
    if ! exists; then
        yellow "Tunnel: not installed (container '${CONTAINER}' does not exist)"
        return 0
    fi
    if is_running; then
        green "Tunnel: RUNNING (mode: $(load_mode))"
        docker ps --filter "name=^/${CONTAINER}$" \
            --format '  container: {{.Names}}   image: {{.Image}}   status: {{.Status}}'
        cmd_url || true
    else
        yellow "Tunnel: STOPPED (container exists but not running)"
    fi
}

cmd_url() {
    need_docker
    if ! exists; then
        err "No tunnel container. Run: ./tunnel.sh enable"
        return 1
    fi
    local mode
    mode=$(load_mode)
    if [[ "${mode}" == "quick" ]]; then
        local url
        url=$(docker logs "${CONTAINER}" 2>&1 \
              | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' \
              | tail -1 || true)
        if [[ -n "${url}" ]]; then
            echo "${url}"
        else
            err "Quick tunnel URL not yet available. Check: docker logs ${CONTAINER}"
            return 1
        fi
    else
        # Named mode — parse the first hostname from config.yml.
        if [[ -f "${CONFIG_DIR}/config.yml" ]]; then
            local host
            host=$(grep -E '^\s*-?\s*hostname:' "${CONFIG_DIR}/config.yml" \
                   | head -1 | awk -F: '{print $2}' | tr -d ' ')
            if [[ -n "${host}" ]]; then
                echo "https://${host}"
                return 0
            fi
        fi
        err "Named tunnel hostname not found in ${CONFIG_DIR}/config.yml"
        return 1
    fi
}

cmd_restart() {
    need_docker
    ensure_network
    local mode
    mode=$(load_mode)
    info "Restarting tunnel in '${mode}' mode..."
    if [[ "${mode}" == "named" ]]; then
        start_named
    else
        start_quick
    fi
}

cmd_logs() {
    need_docker
    if ! exists; then
        err "No tunnel container. Run: ./tunnel.sh enable"
        exit 1
    fi
    if [[ "${1:-}" == "-f" || "${1:-}" == "--follow" ]]; then
        docker logs -f "${CONTAINER}"
    else
        docker logs --tail 100 "${CONTAINER}"
    fi
}

usage() {
    sed -n '2,25p' "$0"
    exit 1
}

# ── Dispatch ────────────────────────────────────────────────────────────────
case "${1:-}" in
    enable)  shift; cmd_enable  "${1:-}" ;;
    disable) cmd_disable ;;
    status)  cmd_status ;;
    url)     cmd_url ;;
    restart) cmd_restart ;;
    logs)    shift; cmd_logs "${1:-}" ;;
    -h|--help|help|"") usage ;;
    *) err "Unknown command: $1"; usage ;;
esac
