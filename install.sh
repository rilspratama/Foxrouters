#!/usr/bin/env bash
# install.sh — FoxRouters one-liner installer (no docker-compose needed)
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/rilspratama/Foxrouters/master/install.sh | bash
#
# Or clone + run:
#   ./install.sh
#
# What it does:
#   1. Checks/installs Docker
#   2. Creates dedicated network + volumes
#   3. Pulls and starts Redis + FoxRouters containers
#   4. Generates random Redis password + bootstraps gateway admin key
#   5. Prints gateway key + dashboard URL
#
# Log backend is SQLite (only). The ClickHouse backend was removed (Aug 2026).
# Legacy LOG_BACKEND=clickhouse values are ignored with a notice and any stale
# foxrouters-clickhouse container is cleaned up.
#
# Development mode (isolated stack, own Redis, port 20131):
#   curl -fsSL … | DEV_MODE=1 bash
#   — or after install: ./dev.sh up && ./dev.sh seed
#
# Update an existing install:
#   curl -fsSL -o update.sh https://raw.githubusercontent.com/rilspratama/Foxrouters/master/update.sh
#   bash update.sh            # update to latest
#   bash update.sh --check    # check only (no pull)
#   bash update.sh --tag=vX.Y.Z  # specific version
#
# Native binary mode (no Docker) — requires an EXISTING Redis:
#   curl -fsSL … | bash -s -- --binary
#   ./install.sh --binary --version=v1.6.14   # pin release version
#   REDIS_ADDR=host:port REDIS_PASSWORD=… ./install.sh --binary  # external Redis
# Installs the release binary + optional cloudflared + systemd service.
# Redis is NOT installed — point REDIS_ADDR / REDIS_PASSWORD at an existing
# instance (local, Docker, or remote). Re-run to upgrade (idempotent).
#
# Manage after install:
#   docker logs foxrouters -f
#   docker restart foxrouters
#   docker rm -f foxrouters foxrouters-redis   # remove (keeps volumes)
#
# Native binary mode (no Docker — Redis via apt + systemd service):
#   curl -fsSL https://raw.githubusercontent.com/rilspratama/Foxrouters/master/install.sh | bash -s -- --binary
#   ./install.sh --binary --version=v1.6.14   # pin a release; default = latest
#
set -euo pipefail

# ── Argument parsing ─────────────────────────────────────────────────────────
MODE="docker"          # docker (default) | binary
FOXROUTERS_VERSION=""  # pin release version for --binary (default latest)
for arg in "$@"; do
    case "$arg" in
        --binary) MODE="binary" ;;
        --version=*) FOXROUTERS_VERSION="${arg#*=}" ;;
    esac
done

# ── Paths/ports — defined here (used by binary mode below + Docker mode later)
GATEWAY_PORT="${FOXROUTERS_PORT:-20130}"
REDIS_PORT="${REDIS_PORT:-6379}"
CONFIG_DIR="/etc/foxrouters"
ENV_FILE="${CONFIG_DIR}/.env"
KEY_FILE="${CONFIG_DIR}/gateway-key.txt"
CF_BIN_PATH="/usr/local/bin/cloudflared"   # resolved during binary-mode install
REDIS_ADDR_FINAL="127.0.0.1:6379"          # resolved during binary-mode install
REDIS_PASS_FINAL=""                        # resolved during binary-mode install

# ── install_binary_mode: native binary install (no Docker) ─────────────────
# Redis via apt + systemd, gateway binary from GitHub Release, optional
# cloudflared for the tunnel feature. Idempotent — re-run to upgrade.
install_binary_mode() {
    bold "═══════════════════════════════════════════════════════════════"
    bold "  FoxRouters — Native Binary Install (no Docker)"
    bold "═══════════════════════════════════════════════════════════════"

    # ── OS/arch detection ─────────────────────────────────────────────────
    local os arch
    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *) err "Unsupported OS: $(uname -s) (linux/darwin only)"; exit 1 ;;
    esac
    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) err "Unsupported arch: $(uname -m)"; exit 1 ;;
    esac
    info "Target: ${os}/${arch}"

    if [[ "${os}" == "darwin" ]]; then
        err "Native install on macOS = manual only (no systemd)."
        err "Download the darwin archive and run ./foxrouters directly."
        exit 1
    fi

    # ── Root check ────────────────────────────────────────────────────────
    if [[ "$(id -u)" -ne 0 ]]; then
        err "Binary mode needs root (apt + systemd). Run as root or with sudo."
        exit 1
    fi

    mkdir -p "${CONFIG_DIR}"
    chmod 700 "${CONFIG_DIR}"

    # ── Redis (config only — user provides it) ────────────────────────────
    # No server install: FoxRouters needs an existing Redis (local, Docker,
    # or remote). Point REDIS_ADDR / REDIS_PASSWORD at it; the installer
    # only verifies reachability and writes the values into .env.
    local redis_addr="${REDIS_ADDR:-127.0.0.1:${REDIS_PORT}}"
    local redis_host="${redis_addr%%:*}"
    local redis_port="${redis_addr##*:}"

    info "Checking Redis at ${redis_addr}..."
    local ping_auth
    if command -v redis-cli &>/dev/null; then
        if [[ -n "${REDIS_PASSWORD:-}" ]]; then
            ping_auth="$(timeout 3 redis-cli -h "${redis_host}" -p "${redis_port}" \
                -a "${REDIS_PASSWORD}" ping 2>/dev/null || true)"
        else
            ping_auth="$(timeout 3 redis-cli -h "${redis_host}" -p "${redis_port}" \
                ping 2>/dev/null || true)"
        fi
    else
        yellow "redis-cli not found — skipping Redis verification (gateway will surface auth errors)"
        ping_auth="PONG"
    fi

    if [[ "${ping_auth}" != "PONG" ]]; then
        err "Redis not reachable at ${redis_addr} (down or password mismatch)."
        err "FoxRouters requires an existing Redis — provide one via:"
        err "  REDIS_ADDR=host:port   (default 127.0.0.1:6379)"
        err "  REDIS_PASSWORD=...     (omit if the instance has no password)"
        exit 1
    fi
    ok "Redis up (auth OK)"

    # Expose resolved values to the .env writer in install_binary_service
    REDIS_ADDR_FINAL="${redis_addr}"
    REDIS_PASS_FINAL="${REDIS_PASSWORD:-}"

    install_binary_download "${os}" "${arch}"
}

# ── Download release binary + verify checksum ─────────────────────────────
install_binary_download() {
    local os="$1" arch="$2"

    # Resolve release version: --version=vX.Y.Z or latest tag via GitHub API
    local ver asset_url checksums_url tmpdir archive
    if [[ -n "${FOXROUTERS_VERSION}" ]]; then
        ver="${FOXROUTERS_VERSION}"
    else
        info "Resolving latest release..."
        ver="$(curl -fsSL https://api.github.com/repos/rilspratama/Foxrouters/releases/latest \
            | grep -oE '"tag_name":\s*"[^"]+"' | head -1 | sed 's/.*"\([^"]*\)".*/\1/')"
        if [[ -z "${ver}" ]]; then
            err "Could not resolve latest release — pass --version=vX.Y.Z"
            exit 1
        fi
    fi
    ver="${ver#v}"
    info "Release: v${ver} (${os}/${arch})"

    asset_url="https://github.com/rilspratama/Foxrouters/releases/download/v${ver}/foxrouters_${ver}_${os}_${arch}.tar.gz"
    checksums_url="https://github.com/rilspratama/Foxrouters/releases/download/v${ver}/checksums.txt"

    tmpdir="$(mktemp -d)"
    local archive_name="foxrouters_${ver}_${os}_${arch}.tar.gz"
    info "Downloading ${os}/${arch} binary..."
    curl -fsSL -o "${tmpdir}/${archive_name}" "${asset_url}" \
        || { err "Download failed (asset not found? release built?)"; exit 1; }
    curl -fsSL -o "${tmpdir}/checksums.txt" "${checksums_url}" || true

    if [[ -s "${tmpdir}/checksums.txt" ]]; then
        (cd "${tmpdir}" && sha256sum -c checksums.txt --ignore-missing) \
            || { err "Checksum mismatch — aborting"; exit 1; }
        ok "Checksum verified"
    else
        yellow "No checksums.txt — skipping verification"
    fi

    tar xzf "${tmpdir}/${archive_name}" -C "${tmpdir}"
    install -m 0755 "${tmpdir}/foxrouters" /usr/local/bin/foxrouters
    rm -rf "${tmpdir}"
    # NOTE: binary has no --version flag — it would start the server. Just confirm install.
    ok "Installed: /usr/local/bin/foxrouters (v${ver})"

    # ── cloudflared (optional — tunnel feature) ────────────────────────────
    # Respect an existing install anywhere on PATH; only download if missing.
    CF_BIN_PATH="/usr/local/bin/cloudflared"   # default (script-level, used by .env)
    if command -v cloudflared &>/dev/null; then
        CF_BIN_PATH="$(command -v cloudflared)"
        ok "cloudflared found: ${CF_BIN_PATH}"
    else
        info "Installing cloudflared (tunnel feature)..."
        if curl -fsSL -o /usr/local/bin/cloudflared \
            "https://github.com/cloudflare/cloudflared/releases/latest/download/cloudflared-${os}-${arch}" \
            && chmod +x /usr/local/bin/cloudflared; then
            ok "cloudflared installed: $(/usr/local/bin/cloudflared --version 2>/dev/null | head -1)"
        else
            yellow "cloudflared install failed — tunnel disabled (install later)"
        fi
    fi

    install_binary_service "${ver}"
}

# ── systemd service + .env + start ────────────────────────────────────────
install_binary_service() {
    local ver="$1"

    # Gateway key (idempotent)
    local gw_key
    if [[ -f "${KEY_FILE}" ]]; then
        gw_key="$(cat "${KEY_FILE}")"
    else
        gw_key="gw-$(gen_hex 24)"
        printf '%s\n' "${gw_key}" > "${KEY_FILE}"
        chmod 600 "${KEY_FILE}"
    fi

    # Write .env (preserve existing values on re-run)
    {
        printf 'PORT=%s\n' "${GATEWAY_PORT}"
        printf 'GATEWAY_BIND=127.0.0.1:%s\n' "${GATEWAY_PORT}"
        printf 'REDIS_ADDR=%s\n' "${REDIS_ADDR_FINAL}"
        printf 'REDIS_PASSWORD=%s\n' "${REDIS_PASS_FINAL}"
        printf 'GATEWAY_API_KEYS=%s\n' "${gw_key}"
        printf 'LOG_BACKEND=sqlite\n'
        printf 'LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db\n'
        printf 'CLOUDFLARED_PATH=%s\n' "${CF_BIN_PATH}"
    } > "${ENV_FILE}.tmp"
    mv "${ENV_FILE}.tmp" "${ENV_FILE}"
    chmod 600 "${ENV_FILE}"
    mkdir -p /var/lib/foxrouters
    ok "Config: ${ENV_FILE}"

    # systemd unit
    cat > /etc/systemd/system/foxrouters.service <<EOF
[Unit]
Description=FoxRouters AI Gateway (Grok + CodeBuddy + Freebuff + Alibaba)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=${ENV_FILE}
Environment=HOME=/var/lib/foxrouters
ExecStart=/usr/local/bin/foxrouters
Restart=on-failure
RestartSec=3
# Hardening (mirrors the Docker image)
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/foxrouters

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable foxrouters >/dev/null 2>&1 || true
    systemctl restart foxrouters
    ok "Service started (foxrouters.service)"

    # Health check
    local i
    for i in $(seq 1 15); do
        if curl -fsS "http://127.0.0.1:${GATEWAY_PORT}/health" >/dev/null 2>&1; then
            break
        fi
        sleep 2
    done
    if curl -fsS "http://127.0.0.1:${GATEWAY_PORT}/health" >/dev/null 2>&1; then
        ok "Gateway healthy on :${GATEWAY_PORT}"
    else
        err "Gateway did not become healthy — check: journalctl -u foxrouters -n 50"
        exit 1
    fi

    # ── Summary ────────────────────────────────────────────────────────────
    echo ""
    bold "═══════════════════════════════════════════════════════════════"
    echo ""
    echo "  FoxRouters v${ver} installed — native binary"
    echo ""
    echo "  Dashboard:    http://127.0.0.1:${GATEWAY_PORT}/dashboard"
    echo "  Gateway key:  ${gw_key}"
    echo "  Config:       ${ENV_FILE}"
    echo "  Logs:         journalctl -u foxrouters -f"
    echo ""
    echo "  Manage:"
    echo "    systemctl restart foxrouters"
    echo "    systemctl stop foxrouters"
    echo "    Re-run this installer to upgrade to a newer release"
    echo ""
    echo "  Tunnel:       ./tunnel.sh enable          (quick)"
    echo "                ./tunnel.sh enable --named  (custom domain)"
    echo ""
    bold "═══════════════════════════════════════════════════════════════"
}

# ── Config ──────────────────────────────────────────────────────────────────
IMAGE_GATEWAY="${IMAGE_GATEWAY:-ghcr.io/rilspratama/foxrouters:latest}"
IMAGE_REDIS="redis:7-alpine"
NETWORK="foxrouters-net"
VOL_REDIS="foxrouters-redis-data"
VOL_SQLITE="foxrouters-sqlite-data"
TUNNEL_CONFIG_DIR="${CONFIG_DIR}/cloudflared"
TUNNEL_CONTAINER_QUICK="foxrouters-tunnel-quick"
TUNNEL_CONTAINER_NAMED="foxrouters-tunnel-named"
IMAGE_TUNNEL="cloudflare/cloudflared:latest"

# ── Colors ───────────────────────────────────────────────────────────────────
red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
bold()   { printf '\033[1m%s\033[0m\n' "$*"; }
info()   { printf '\033[36m[i]\033[0m %s\n' "$*"; }
ok()     { printf '\033[32m[✓]\033[0m %s\n' "$*"; }
err()    { printf '\033[31m[✗]\033[0m %s\n' "$*"; }

# gen_hex N — N random hex bytes (openssl preferred, urandom fallback)
gen_hex() {
    if command -v openssl &>/dev/null; then
        openssl rand -hex "$1"
    else
        head -c "$1" /dev/urandom | od -An -tx1 | tr -d ' \n'
    fi
}

# ── Native binary mode (no Docker) ─────────────────────────────────────────
if [[ "${MODE}" == "binary" ]]; then
    install_binary_mode
    exit 0
fi

# ── Step 1: Docker check/install ────────────────────────────────────────────
info "Checking Docker..."
if ! command -v docker &>/dev/null; then
    yellow "Docker not found. Installing..."
    if command -v curl &>/dev/null; then
        curl -fsSL https://get.docker.com | sh
    elif command -v wget &>/dev/null; then
        wget -qO- https://get.docker.com | sh
    else
        err "Cannot install Docker: neither curl nor wget found."
        err "Install Docker manually: https://docs.docker.com/engine/install/"
        exit 1
    fi
    systemctl start docker 2>/dev/null || service docker start 2>/dev/null || true
    systemctl enable docker 2>/dev/null || true
    ok "Docker installed"
else
    ok "Docker found: $(docker --version)"
fi

if ! docker info &>/dev/null; then
    err "Docker daemon not running. Start it with: systemctl start docker"
    exit 1
fi

# ── Step 2: Log backend ─────────────────────────────────────────────────────
# SQLite is the only backend (ClickHouse removed Aug 2026). Accept legacy
# LOG_BACKEND=clickhouse silently but always run sqlite; clean up a stale CH
# container if one exists from an older install.
if [[ -n "${LOG_BACKEND:-}" && "${LOG_BACKEND}" != "sqlite" && "${LOG_BACKEND}" != "sqlite3" ]]; then
    info "LOG_BACKEND=${LOG_BACKEND} is deprecated (ClickHouse removed) — using sqlite"
fi
LOG_BACKEND="sqlite"
if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q 'foxrouters-clickhouse'; then
    info "Removing stale foxrouters-clickhouse container (backend removed)"
    docker rm -f foxrouters-clickhouse 2>/dev/null || true
fi
ok "Log backend: sqlite"

# ── Step 3: Generate secrets ────────────────────────────────────────────────
info "Generating secrets..."
REDIS_PASSWORD=$(openssl rand -hex 16 2>/dev/null || head -c 16 /dev/urandom | xxd -p)

mkdir -p "${CONFIG_DIR}"
chmod 700 "${CONFIG_DIR}"

# Write env file (for reference / re-deploy)
cat > "${ENV_FILE}" << EOF
# FoxRouters Docker deployment — generated by install.sh
# Date: $(date -u +"%Y-%m-%dT%H:%M:%SZ")
REDIS_ADDR=redis:6379
REDIS_PASSWORD=${REDIS_PASSWORD}
REDIS_DB=0
LOG_BACKEND=sqlite
LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db
PORT=${GATEWAY_PORT}
EOF
chmod 600 "${ENV_FILE}"
ok "Secrets generated → ${ENV_FILE}"

# ── Step 4: Create network + volumes ─────────────────────────────────────────
info "Creating Docker network + volumes..."
docker network create "${NETWORK}"    2>/dev/null || ok "Network ${NETWORK} already exists"
docker volume  create "${VOL_REDIS}"  2>/dev/null || ok "Volume ${VOL_REDIS} already exists"
docker volume  create "${VOL_SQLITE}" 2>/dev/null || ok "Volume ${VOL_SQLITE} already exists"

# Fix volume ownership: gateway runs as UID 1000 (non-root).
# Without this, SQLite can't write to /var/lib/foxrouters/logs.db.
docker run --rm -v "${VOL_SQLITE}:/data" alpine chown -R 1000:1000 /data 2>/dev/null || true

# ── Step 5: Pull images ─────────────────────────────────────────────────────
info "Pulling images (this may take a few minutes on first run)..."
docker pull "${IMAGE_REDIS}"      2>&1 | tail -1
# Only pull gateway if not already present locally (avoid overwriting local builds)
if ! docker image inspect "${IMAGE_GATEWAY}" &>/dev/null; then
    docker pull "${IMAGE_GATEWAY}"    2>&1 | tail -1
else
    ok "Gateway image found locally: ${IMAGE_GATEWAY}"
fi
ok "Images pulled"

# ── Step 6: Start Redis ─────────────────────────────────────────────────────
info "Starting Redis..."
docker rm -f foxrouters-redis 2>/dev/null || true
docker run -d \
    --name foxrouters-redis \
    --network "${NETWORK}" \
    --network-alias redis \
    -p "127.0.0.1:${REDIS_PORT}:6379" \
    -v "${VOL_REDIS}:/data" \
    --restart unless-stopped \
    "${IMAGE_REDIS}" \
    redis-server --requirepass "${REDIS_PASSWORD}" --appendonly no
ok "Redis started (port ${REDIS_PORT})"

# ── Step 7: Cleanup legacy ClickHouse (backend removed Aug 2026) ────────────
if docker ps -a --format '{{.Names}}' 2>/dev/null | grep -q 'foxrouters-clickhouse'; then
    info "Removing stale foxrouters-clickhouse container (backend removed)"
    docker rm -f foxrouters-clickhouse 2>/dev/null || true
    docker volume rm foxrouters-clickhouse-data 2>/dev/null || true
fi

# ── Step 8: Wait for dependencies healthy ───────────────────────────────────
info "Waiting for Redis to be healthy..."
for i in $(seq 1 30); do
    REDIS_OK=$(docker exec foxrouters-redis redis-cli -a "${REDIS_PASSWORD}" ping 2>/dev/null || echo "FAIL")
    if [[ "${REDIS_OK}" == "PONG" ]]; then
        ok "Dependencies healthy"
        break
    fi
    printf "."
    sleep 2
    if [[ $i -eq 30 ]]; then
        err "Timeout waiting for dependencies"
        docker logs foxrouters-redis --tail 5
        exit 1
    fi
done

# ── Step 9: Start FoxRouters gateway ────────────────────────────────────────
info "Starting FoxRouters gateway..."
docker rm -f foxrouters 2>/dev/null || true
docker run -d \
    --name foxrouters \
    --network "${NETWORK}" \
    -p "127.0.0.1:${GATEWAY_PORT}:20130" \
    -v "${VOL_SQLITE}:/var/lib/foxrouters" \
    --restart unless-stopped \
    --env-file "${ENV_FILE}" \
    -e REDIS_ADDR=redis:6379 \
    -e LOG_BACKEND=sqlite \
    -e LOG_SQLITE_PATH=/var/lib/foxrouters/logs.db \
    -e PORT=20130 \
    "${IMAGE_GATEWAY}"
ok "FoxRouters started (port ${GATEWAY_PORT}, log backend: sqlite)"

# ── Step 10: Capture gateway key from Redis ─────────────────────────────────
info "Waiting for gateway to write key to Redis (up to 30s)..."
BOOTSTRAP_KEY=""
for i in $(seq 1 30); do
    EXISTING_KEY=$(docker exec foxrouters-redis redis-cli -a "${REDIS_PASSWORD}" --scan --pattern 'gw:key:*' 2>/dev/null | head -1 || true)
    if [[ -n "${EXISTING_KEY}" ]]; then
        BOOTSTRAP_KEY=$(docker exec foxrouters-redis redis-cli -a "${REDIS_PASSWORD}" hget "${EXISTING_KEY}" key 2>/dev/null | tr -d '\r\n' || true)
        if [[ -n "${BOOTSTRAP_KEY}" ]]; then
            break
        fi
    fi
    printf "."
    sleep 1
done

if [[ -z "${BOOTSTRAP_KEY}" ]]; then
    err "Gateway did not write a key to Redis within 30s."
    err "Check logs: docker logs foxrouters"
    exit 1
fi

echo "${BOOTSTRAP_KEY}" > "${KEY_FILE}"
chmod 600 "${KEY_FILE}"
ok "Gateway key captured → ${KEY_FILE}"

# ── Step 11: Health check ───────────────────────────────────────────────────
info "Health check..."
sleep 3
HEALTH=$(curl -sf "http://127.0.0.1:${GATEWAY_PORT}/health" 2>/dev/null || echo "FAIL")
if echo "${HEALTH}" | grep -q "healthy"; then
    ok "Gateway healthy: ${HEALTH}"
else
    err "Gateway not healthy yet. Check: docker logs foxrouters"
    err "Response: ${HEALTH}"
fi

# ── Step 12: Cloudflare Tunnel (optional) ───────────────────────────────────
# Priority:
#   1. TUNNEL_MODE env var (non-interactive install: quick|named|none)
#   2. Interactive prompt (only if stdin is a TTY)
#   3. Default: none (piped installs stay tunnel-free)
TUNNEL_MODE="${TUNNEL_MODE:-}"
if [[ -z "${TUNNEL_MODE}" ]]; then
    if [[ -t 0 ]]; then
        echo ""
        bold "Cloudflare Tunnel"
        echo "  Expose the gateway publicly via Cloudflare (no firewall changes)."
        echo "  [1] Quick — random *.trycloudflare.com URL (no domain, no login)"
        echo "  [2] Named — custom domain (requires prior cloudflared login + create)"
        echo "  [3] Hybrid — BOTH quick + named (quick now, add named later)"
        echo "  [4] No tunnel"
        echo ""
        read -r -p "Choice [4]: " TCHOICE || TCHOICE=""
        case "${TCHOICE}" in
            1|q|quick)   TUNNEL_MODE="quick" ;;
            2|n|named)   TUNNEL_MODE="named" ;;
            3|h|hybrid)  TUNNEL_MODE="hybrid" ;;
            *)           TUNNEL_MODE="none"  ;;
        esac
    else
        TUNNEL_MODE="none"
    fi
fi
case "${TUNNEL_MODE}" in
    none|quick|named|hybrid) ;;
    *)
        err "Unknown TUNNEL_MODE=${TUNNEL_MODE} — must be none, quick, named, or hybrid"
        exit 1
        ;;
esac

TUNNEL_URL=""
TUNNEL_CONTAINER_QUICK="foxrouters-tunnel-quick"
TUNNEL_CONTAINER_NAMED="foxrouters-tunnel-named"

start_quick_tunnel() {
    info "Starting Cloudflare quick tunnel..."
    docker pull "${IMAGE_TUNNEL}" 2>&1 | tail -1 || true
    docker rm -f "${TUNNEL_CONTAINER_QUICK}" 2>/dev/null || true
    docker run -d \
        --name "${TUNNEL_CONTAINER_QUICK}" \
        --network "${NETWORK}" \
        --restart unless-stopped \
        "${IMAGE_TUNNEL}" \
        tunnel --no-autoupdate --url "http://foxrouters:20130" >/dev/null
    info "Waiting for quick tunnel URL (up to 30s)..."
    for _ in $(seq 1 30); do
        TUNNEL_URL=$(docker logs "${TUNNEL_CONTAINER_QUICK}" 2>&1 \
            | grep -oE 'https://[a-zA-Z0-9.-]+\.trycloudflare\.com' \
            | head -1 || true)
        [[ -n "${TUNNEL_URL}" ]] && break
        sleep 1
    done
    if [[ -n "${TUNNEL_URL}" ]]; then
        ok "Quick tunnel URL: ${TUNNEL_URL}"
    else
        err "Could not capture quick tunnel URL. Check: docker logs ${TUNNEL_CONTAINER_QUICK}"
    fi
}

print_named_setup() {
    echo ""
    yellow "  Named tunnels require manual one-time setup:"
    echo "    1. cloudflared tunnel login"
    echo "    2. cloudflared tunnel create foxrouters"
    echo "    3. sudo mkdir -p ${TUNNEL_CONFIG_DIR}"
    echo "       sudo cp ~/.cloudflared/cert.pem        ${TUNNEL_CONFIG_DIR}/"
    echo "       sudo cp ~/.cloudflared/<tunnel>.json   ${TUNNEL_CONFIG_DIR}/"
    echo "    4. Write ${TUNNEL_CONFIG_DIR}/config.yml with ingress rules"
    echo "    5. cloudflared tunnel route dns foxrouters gateway.example.com"
    echo "    6. ./tunnel.sh enable --named  (or --hybrid to run both)"
    echo ""
}

mkdir -p "${TUNNEL_CONFIG_DIR}"

case "${TUNNEL_MODE}" in
    quick)
        echo "quick" > "${TUNNEL_CONFIG_DIR}/mode"
        start_quick_tunnel
        ;;
    named)
        echo "named" > "${TUNNEL_CONFIG_DIR}/mode"
        info "Named tunnel selected — auto-start skipped (needs prior setup)."
        print_named_setup
        ;;
    hybrid)
        echo "hybrid" > "${TUNNEL_CONFIG_DIR}/mode"
        info "Hybrid mode: starting quick tunnel now..."
        start_quick_tunnel
        echo ""
        info "Named tunnel: auto-start skipped (needs prior setup)."
        info "After setup, run: ./tunnel.sh enable --hybrid"
        print_named_setup
        ;;
    none)
        echo "none" > "${TUNNEL_CONFIG_DIR}/mode"
        ;;
esac

# ── Summary ─────────────────────────────────────────────────────────────────
echo ""
bold "═══════════════════════════════════════════════════════════════"
bold "  FoxRouters installed successfully!"
bold "═══════════════════════════════════════════════════════════════"
echo ""
if [[ -n "${BOOTSTRAP_KEY}" ]]; then
    green "  Gateway Key:  ${BOOTSTRAP_KEY}"
    echo ""
    yellow "  ⚠  Save this key — it won't be shown again."
    yellow "     Stored at: ${KEY_FILE}"
    echo ""
fi
echo "  Dashboard:    http://$(hostname -I 2>/dev/null | awk '{print $1}' || echo 'localhost'):${GATEWAY_PORT}/dashboard"
echo "  API Base:     http://localhost:${GATEWAY_PORT}/v1/chat/completions"
echo "  Health:       http://localhost:${GATEWAY_PORT}/health"
echo "  Log backend:  ${LOG_BACKEND}"
if [[ "${TUNNEL_MODE}" == "quick" && -n "${TUNNEL_URL}" ]]; then
    echo "  Tunnel:       ${TUNNEL_URL}  (quick — URL changes on restart)"
elif [[ "${TUNNEL_MODE}" == "named" ]]; then
    echo "  Tunnel:       named (finish setup, then ./tunnel.sh enable --named)"
elif [[ "${TUNNEL_MODE}" == "hybrid" && -n "${TUNNEL_URL}" ]]; then
    echo "  Tunnel:       ${TUNNEL_URL}  (quick active, add named via ./tunnel.sh enable --hybrid)"
else
    echo "  Tunnel:       disabled  (./tunnel.sh enable to add one later)"
fi
echo ""
echo "  Config:       ${ENV_FILE}"
echo ""

# ── Development mode notice ─────────────────────────────────────────────────
if [[ "${DEV_MODE:-0}" == "1" ]]; then
    yellow "  DEV MODE enabled — isolated dev stack available:"
    echo "    ./dev.sh build    # build foxrouters:dev image"
    echo "    ./dev.sh up       # start dev gateway (:20131) + dev redis (:6381)"
    echo "    ./dev.sh seed     # copy credentials from prod Redis (read-only)"
    echo "    ./dev.sh logs     # tail dev gateway logs"
    echo "    ./dev.sh down     # stop dev stack (add -v to wipe)"
    echo ""
    yellow "  Safety gates auto-enabled: WORKERS_DISABLED, HEALTH_PROBES_DISABLED,"
    yellow "  TOKEN_REFRESH_DISABLED (tokens zeroed in dev Redis — leak prevention)"
    echo ""
fi

echo "  Manage:"
echo "    docker logs foxrouters -f           # tail logs"
echo "    docker restart foxrouters           # restart gateway"
if [[ "${LOG_BACKEND}" == "clickhouse" ]]; then
    echo "    docker stop foxrouters-redis foxrouters-clickhouse foxrouters"
    echo "    docker start foxrouters-redis foxrouters-clickhouse foxrouters"
    echo "    docker rm -f foxrouters foxrouters-redis foxrouters-clickhouse"
else
    echo "    docker stop foxrouters-redis foxrouters"
    echo "    docker start foxrouters-redis foxrouters"
    echo "    docker rm -f foxrouters foxrouters-redis"
fi
echo ""
bold "═══════════════════════════════════════════════════════════════"
