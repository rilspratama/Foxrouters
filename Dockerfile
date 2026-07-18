# syntax=docker/dockerfile:1.7
# Multi-stage build for FoxRouters.
# dashboard.html is compiled into the binary via go:embed, so runtime image
# only needs the static binary + CA roots + wget (for healthcheck).

# -----------------------------------------------------------------------------
# Stage 1: builder
# -----------------------------------------------------------------------------
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source tree (respects .dockerignore).
COPY . .

# CGO_ENABLED=0 → fully static binary, safe to drop into scratch/alpine.
# -ldflags "-s -w" strips debug/symbol tables (~30% smaller).
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o foxrouters .

# -----------------------------------------------------------------------------
# Stage 2: runtime
# -----------------------------------------------------------------------------
FROM alpine:3.20 AS runtime

# ca-certificates → outbound TLS (grok.com, codebuddy.ai)
# wget           → HEALTHCHECK probe
RUN apk add --no-cache ca-certificates wget

# Non-root user (UID 1000) for least-privilege runtime.
RUN adduser -D -u 1000 foxrouters

WORKDIR /app

COPY --from=builder /build/foxrouters .

USER foxrouters

EXPOSE 20130

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q --spider http://localhost:20130/health || exit 1

ENTRYPOINT ["/app/foxrouters"]
