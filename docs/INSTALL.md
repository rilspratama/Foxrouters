# Installation

## Quick Start (One-Liner Installer — No Compose Needed)

> Fastest path. Auto-installs Docker if missing, pulls all images, starts
> Redis + FoxRouters. No clone, no build, no docker-compose.

```bash
curl -fsSL https://raw.githubusercontent.com/rilspratama/Foxrouters/master/install.sh | bash
```

**Output:**
```
[✓] Docker found: Docker version 29.6.1
[✓] Secrets generated → /etc/foxrouters/.env
[✓] Redis started (port 6379)
[✓] FoxRouters started (port 20130)
[✓] Gateway key captured → /etc/foxrouters/gateway-key.txt
[✓] Gateway healthy: {"service":"foxrouters","status":"healthy","version":"v1.6.14"}

═══════════════════════════════════════════════════════════════
  FoxRouters installed successfully!
═══════════════════════════════════════════════════════════════

  Gateway Key:  gw-a94c7befdb14cd6d2...

  Dashboard:    http://<host-ip>:20130/dashboard
  API Base:     http://localhost:20130/v1/chat/completions
  Config:       /etc/foxrouters/.env

  Manage:
    docker logs foxrouters -f
    docker restart foxrouters
    docker stop foxrouters-redis foxrouters
═══════════════════════════════════════════════════════════════
```

**Custom ports** (optional):
```bash
FOXROUTERS_PORT=8080 REDIS_PORT=6380 bash install.sh
```

**Manage after install:**
```bash
docker logs foxrouters -f                                    # tail logs
docker restart foxrouters                                     # restart gateway
docker stop foxrouters-redis foxrouters  # stop all
docker start foxrouters-redis foxrouters  # start all
docker rm -f foxrouters foxrouters-redis  # remove (keeps data)
docker volume rm foxrouters-redis-data  # wipe data
```

**Update to a new version:**
```bash
curl -fsSL -o update.sh https://raw.githubusercontent.com/rilspratama/Foxrouters/master/update.sh
bash update.sh              # pull latest + recreate gateway (state persists in Redis)
bash update.sh --check      # check for update without pulling
bash update.sh --tag=v1.6.14 # pin to a specific tag (also works for downgrade/rollback)
```
Only the gateway container is recreated — Redis/SQLite volumes are untouched.

---

## Quick Start (Docker Compose — For Development)

> Clone + build from source. Uses `docker-compose.yml` with build context.

Open `http://localhost:20130/login`, paste the key, done.

---

## Quick Start (Docker — Build from Source)

> One command. The compose file wires `foxrouters` and `redis`
> together — no `.env` editing needed for the default stack.

```bash
git clone https://github.com/rilspratama/Foxrouters.git foxrouters && cd foxrouters

# Start stack + capture bootstrap key (first boot auto-generates admin key)
./start.sh
```

**Output:**
```
🔑 Admin Bootstrap Key
  Key:    gw-a94c7befdb14cd6d2...819edd11
  Login:  http://localhost:20130/login
  Saved:  bootstrap-key.txt (chmod 600)
```

Then open `http://localhost:20130/login`, paste the key, done.

**Other commands:**
```bash
./start.sh --status    # container + health status
./start.sh --logs      # tail logs
./start.sh --key       # show captured key
./start.sh --reset     # wipe Redis volume + regenerate key
./start.sh --stop      # stop stack
```

### When do I need to edit `.env`?

| Scenario | Edit `.env`? |
|----------|-------------|
| Default docker-compose (Redis+gw in same stack) | ❌ No — compose overrides everything |
| Custom Redis password in compose | ✅ Set `REDIS_PASSWORD` |
| Bare metal / systemd (Redis on host) | ✅ Set `REDIS_ADDR` + `REDIS_PASSWORD` |
| External Redis (managed/Cloudflare) | ✅ Set `REDIS_ADDR=host:port` + `REDIS_PASSWORD` |
| Custom port (20130 → 8080) | ✅ Set `PORT=8080` |

See [`.env.example`](./.env.example) for the full list of tunables.

---

## Quick Start (Manual)

**Prerequisites**

- Go **1.25+**
- Redis (local or remote)

```bash
# 1. Build
export PATH=$PATH:/usr/local/go/bin
go build -o foxrouters .

# 2. Configure
cp .env.example .env
$EDITOR .env

# 3. Run
./foxrouters
# → listening on :20130
```

