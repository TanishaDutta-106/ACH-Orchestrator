# syntax=docker/dockerfile:1.4
# =============================================================================
# Multi-stage build — produces two binaries: server + worker
# Final image targets < 50MB using distroless/static
# =============================================================================

# ── Stage 1: builder ──────────────────────────────────────────────────────────
FROM golang:1.22-alpine AS builder

# Required for CGO_ENABLED=0 static builds
RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

# Cache dependency layer separately from source
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Build both binaries as fully static, stripped of debug info
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/server ./cmd/server

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -X main.version=$(git describe --tags --always 2>/dev/null || echo dev)" \
    -o /out/worker ./cmd/worker

# ── Stage 2: final ────────────────────────────────────────────────────────────
# gcr.io/distroless/static has no shell, no package manager — minimal attack surface
FROM gcr.io/distroless/static:nonroot

# Copy CA certs and timezone data from builder
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy compiled binaries
COPY --from=builder /out/server  /server
COPY --from=builder /out/worker  /worker

# Run as nonroot (uid 65532) — distroless/static:nonroot enforces this
USER nonroot:nonroot

# Default to server; override with CMD ["/ worker"] in ECS worker task def
EXPOSE 8080
CMD ["/server"]
