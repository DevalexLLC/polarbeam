# polarbeam-server image. Build context is the repo root.
# Build is fully offline once base images are present (vendored deps only).
#
# The build stage runs on the BUILD platform and cross-compiles via
# GOOS/GOARCH — multi-arch buildx never emulates the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG VERSION=dev
ARG COMMIT=none
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY . .
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH CGO_ENABLED=0 \
    go build -mod=vendor -trimpath \
    -ldflags "-s -w \
      -X github.com/devalexllc/polarbeam/internal/version.Version=$VERSION \
      -X github.com/devalexllc/polarbeam/internal/version.Commit=$COMMIT" \
    -o /out/polarbeam-server ./cmd/polarbeam-server

FROM alpine:3.22
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="polarbeam-server" \
      org.opencontainers.image.description="PolarBEAM control plane (gRPC ingest + dashboard)" \
      org.opencontainers.image.source="https://github.com/devalexllc/polarbeam" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
# Fixed uid so volume ownership survives image rebuilds. The state volume
# inherits this ownership on first create.
RUN adduser -S -D -H -u 10001 -s /sbin/nologin polarbeam \
    && mkdir -p /var/lib/polarbeam-server \
    && chown 10001 /var/lib/polarbeam-server
COPY --from=build /out/polarbeam-server /usr/local/bin/polarbeam-server
# License + attribution notices travel with every redistribution, images
# included. /licenses is the OCI convention.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
USER 10001
# /healthz is unauthenticated by contract (httpapi tests enforce it); the
# subcommand reads listen.http from the config and skips certificate
# verification (see cmd/polarbeam-server/healthcheck.go).
#
# Exec form, and a probe that forks nothing: the shell form would fork
# /bin/sh, and the previous `wget https://…` forked BusyBox ssl_client and
# exited without reaping it. Container PID 1 is this Go binary, which never
# calls wait(2), so each check leaked one zombie onto the HOST process table
# — ~2/min, forever. Any replacement must stay fork-free.
#
# The runtime timeout stays above the probe's own deadline (5 s default) so a
# hung check reports its own error instead of being killed mid-flight.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s \
    CMD ["polarbeam-server", "healthcheck", "--config", "/etc/polarbeam/server.yaml"]
ENTRYPOINT ["polarbeam-server"]
CMD ["serve", "--config", "/etc/polarbeam/server.yaml"]
