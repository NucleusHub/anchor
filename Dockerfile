# ── Build stage ──────────────────────────────────────────────────────────────
FROM golang:1.23-alpine AS build
WORKDIR /src

# Resolve deps first (go.sum is generated here — network is available at build).
COPY go.mod ./
RUN go mod download || true

COPY . .
RUN go mod tidy && \
    CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /anchor .

# ── Runtime stage ────────────────────────────────────────────────────────────
# Single static binary. Runs as root inside its own container so it can write the
# /data volume; it holds NO host mounts and reaches Docker only via the read-only
# socket-proxy sidecar (see deploy/docker-compose.yml).
FROM alpine:3.20
RUN apk add --no-cache ca-certificates wget
COPY --from=build /anchor /anchor
EXPOSE 8888
HEALTHCHECK --interval=10s --timeout=3s --retries=5 \
  CMD wget -qO- http://localhost:8888/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/anchor"]
