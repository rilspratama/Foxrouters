# CLI & Native Binary

## Install Native Binary (GitHub Release)

Each `vX.Y.Z` release ships native binaries attached to the GitHub Release
(built with GoReleaser, `CGO_ENABLED=0` — pure Go, no libc needed):

| Platform | Asset |
|---|---|
| Linux amd64 / arm64 | `foxrouters_<ver>_linux_amd64.tar.gz` / `_linux_arm64.tar.gz` |
| macOS amd64 / arm64 | `foxrouters_<ver>_darwin_amd64.tar.gz` / `_darwin_arm64.tar.gz` |
| Windows amd64 / arm64 | `foxrouters_<ver>_windows_amd64.zip` / `_windows_arm64.zip` |

```bash
# e.g. v1.6.14 on Linux amd64
curl -fsSL -o foxrouters.tar.gz \
  https://github.com/rilspratama/Foxrouters/releases/download/v1.6.14/foxrouters_v1.6.14_linux_amd64.tar.gz
tar xzf foxrouters.tar.gz
./foxrouters                     # starts on :20130 (env PORT to change)
curl -s http://127.0.0.1:20130/health
```

Verify checksums:

```bash
curl -fsSL -o checksums.txt \
  https://github.com/rilspratama/Foxrouters/releases/download/v1.6.14/checksums.txt
sha256sum -c checksums.txt --ignore-missing
```

Notes:
- Binary still expects a **Redis** instance (set `REDIS_ADDR`, default `127.0.0.1:6379`)
  and credentials via `REDIS_PASSWORD` / `GATEWAY_KEY` env — see [Configuration](#configuration).
- For a full managed stack (Redis + gateway + tunnel) use the Docker one-liner instead.

### CLI (v1.6.14+)

The binary doubles as a small CLI — no args (or `serve`) starts the server as
before; the following subcommands manage an existing install:

```bash
foxrouters version          # print version, exit (used by install.sh verify)
foxrouters config           # INTERACTIVE editor (arrow keys; REDIS_* first)
foxrouters install          # INTERACTIVE first-install wizard (port + Redis)
foxrouters config list      # print config (secrets masked)
foxrouters config get KEY   # print one value (e.g. PORT) — shell-friendly
foxrouters config set KEY VALUE   # set/update a value (atomic rewrite, 0600)
foxrouters update           # self-update from the latest GitHub release
foxrouters update --tag=vX.Y.Z    # update to a specific release
foxrouters health           # probe http://127.0.0.1:PORT/health
foxrouters help             # full usage
```

- `config` (no args) opens an **interactive editor** (TTY required): ↑/↓ navigate, Enter edit, `a` add, `d` delete, `q` quit — raw-mode (`golang.org/x/term`), secrets typed with masked echo, full-screen redraw, every change writes the file immediately. Non-TTY (piped) falls back to an error pointing at `config list/get/set`. Non-interactive: `config list` (masked), `config get KEY`, `config set KEY VALUE`. Override path with `FOXROUTERS_ENV`.
- `update` downloads the release archive for the current `GOOS`/`GOARCH`,
  verifies its SHA-256 against `checksums.txt`, atomically replaces the running
  binary, and restarts the service — but only when the `foxrouters.service`
  systemd unit is **active** (Docker/dev deployments are left untouched; restart
  manually). Point `FOXROUTERS_GH_API` at a mirror for air-gapped installs.

