# syntax=docker/dockerfile:1.7
# Multi-stage build for FoxRouters.
# dashboard/ is compiled into the binary via go:embed (multi-file SPA parts),
# so runtime image only needs the static binary + CA roots + wget (for
# healthcheck) + cloudflared (data plane for /api/tunnel/* — v1.6.0).

# -----------------------------------------------------------------------------
# Stage 1: builder
# -----------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

# VERSION is injected into main.Version via -ldflags. Pass via
# `docker build --build-arg VERSION=v1.2.3 .` or the CI workflow.
ARG VERSION=dev

WORKDIR /build

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source tree (respects .dockerignore).
COPY . .

# CGO_ENABLED=0 → fully static binary, safe to drop into scratch/alpine.
# -ldflags "-s -w" strips debug/symbol tables (~30% smaller).
# -X main.Version stamps the release tag into the binary.
RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=${VERSION}" -o foxrouters .

# -----------------------------------------------------------------------------
# Stage 2: cloudflared downloader
# Pulled in its own stage so the runtime layer stays free of curl/build tools.
# Pinned to a specific release for reproducibility; bump when Cloudflare
# ships a security fix (~monthly cadence).
# -----------------------------------------------------------------------------
FROM alpine:3.20 AS cloudflared

ARG CLOUDFLARED_VERSION=2025.11.1
ARG TARGETARCH

RUN apk add --no-cache curl \
 && case "${TARGETARCH}" in \
      amd64) CF_ARCH="amd64" ;; \
      arm64|aarch64) CF_ARCH="arm64" ;; \
      *) echo "unsupported arch: ${TARGETARCH}"; exit 1 ;; \
    esac \
 && curl -fsSL -o /usr/local/bin/cloudflared \
      "https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}/cloudflared-linux-${CF_ARCH}" \
 && chmod +x /usr/local/bin/cloudflared \
 && /usr/local/bin/cloudflared --version

# -----------------------------------------------------------------------------
# Stage 3: runtime
# -----------------------------------------------------------------------------
FROM alpine:3.20 AS runtime

# ca-certificates → outbound TLS (grok.com, codebuddy.ai, api.cloudflare.com)
# wget           → HEALTHCHECK probe
# libc6-compat   → cloudflared static-ish binary needs libc glue on alpine
RUN apk add --no-cache ca-certificates wget libc6-compat

# Non-root user (UID 1000) for least-privilege runtime.
RUN adduser -D -u 1000 foxrouters

WORKDIR /app

COPY --from=builder /build/foxrouters .
COPY --from=cloudflared /usr/local/bin/cloudflared /usr/local/bin/cloudflared

# cloudflared writes small caches to $HOME/.cloudflared — give it a
# writable dir owned by the non-root user.
RUN mkdir -p /home/foxrouters/.cloudflared \
 && chown -R foxrouters:foxrouters /home/foxrouters

USER foxrouters

# Point the tunnel manager at the embedded binary (also the default,
# but explicit env is nicer for operators inspecting `docker inspect`).
ENV CLOUDFLARED_PATH=/usr/local/bin/cloudflared

EXPOSE 20130

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:20130/health || exit 1

ENTRYPOINT ["/app/foxrouters"]
