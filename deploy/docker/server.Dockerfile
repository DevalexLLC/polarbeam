# polarbeam-server image. Build context is the repo root.
# Build is fully offline once base images are present (vendored deps only).
#
# Stages: base (runtime rootfs) → build (Go cross-compile) → dev (alpine,
# compose-dev overlay ONLY) → release (distroless, the published image).
# `release` is last so it is the DEFAULT target; `make images`, ci.yml and
# release.yml still name it explicitly. The dev overlay selects `dev`.
#
# Runtime base, named once so the digest lives in one place: the release
# stage builds FROM it and the build stage copies its passwd/group. Static
# distroless: no shell, no package manager, no libc; ships ca-certificates
# (OIDC system roots), tzdata, /etc/passwd, /tmp. Pinned by OCI index
# digest (multi-arch) because the tag carries no versions.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7 AS base

# The build stage runs on the BUILD platform and cross-compiles via
# GOOS/GOARCH — multi-arch buildx never emulates the Go compiler.
FROM --platform=$BUILDPLATFORM golang:1.27.1-alpine AS build
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
# Distroless has no RUN, so the release stage's filesystem extras are
# prepared here. The identity for uid 10001 is the exact line the alpine
# image had (uid 10001, gid 65533, home /home/polarbeam): pgx derives its
# default DB username and ~/.pgpass / service-file paths from
# user.Current(), so the entry must survive the base change, not just the
# number. The group line is skipped if the base already defines gid 65533.
COPY --from=base /etc/passwd /etc/group /out/etc/
RUN printf 'polarbeam:x:10001:65533::/home/polarbeam:/sbin/nologin\n' >> /out/etc/passwd \
    && if ! grep -q ':65533:' /out/etc/group; then printf 'polarbeam:x:65533:\n' >> /out/etc/group; fi \
    && mkdir -p /out/state

# Dev stage — compose-dev overlay ONLY, never published. The overlay's
# server entrypoint chains subcommands through /bin/sh and bootstrap.sh
# needs BusyBox, so this stays on alpine. Declared before release so the
# default target stays release.
FROM alpine:3.22 AS dev
RUN adduser -S -D -H -u 10001 -s /sbin/nologin polarbeam \
    && mkdir -p /var/lib/polarbeam-server \
    && chown 10001 /var/lib/polarbeam-server
COPY --from=build /out/polarbeam-server /usr/local/bin/polarbeam-server
# Every image carries the notices, the dev one included.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
USER 10001
# Same fork-free probe as release (see there); a stage cannot inherit it.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s \
    CMD ["/usr/local/bin/polarbeam-server", "healthcheck", "--config", "/etc/polarbeam/server.yaml"]
ENTRYPOINT ["/usr/local/bin/polarbeam-server"]
CMD ["serve", "--config", "/etc/polarbeam/server.yaml"]

FROM base AS release
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="polarbeam-server" \
      org.opencontainers.image.description="PolarBEAM control plane (gRPC ingest + dashboard)" \
      org.opencontainers.image.source="https://github.com/devalexllc/polarbeam" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
# Same passwd/group as the base plus the polarbeam entry (see build stage).
COPY --from=build /out/etc/passwd /out/etc/group /etc/
# Fixed uid so volume ownership survives image rebuilds: a fresh
# server-state volume copies this directory's owner on first create.
# 10001:0 is what `mkdir` as root + `chown 10001` produced before.
COPY --from=build --chown=10001:0 /out/state /var/lib/polarbeam-server
COPY --from=build /out/polarbeam-server /usr/local/bin/polarbeam-server
# License + attribution notices travel with every redistribution, images
# included. /licenses is the OCI convention.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
# uid only, as before: the gid (65533) and supplementary groups resolve
# through the passwd/group entries above, exactly as they did on alpine.
USER 10001
# The base sets WORKDIR /home/nonroot; the previous image ran from / and
# the config accepts relative tls/ca paths, so keep resolution identical.
WORKDIR /
# /healthz is unauthenticated by contract (httpapi tests enforce it); the
# subcommand reads listen.http from the config and skips certificate
# verification (see cmd/polarbeam-server/healthcheck.go).
#
# Exec form, and a probe that forks nothing: the shell form would fork
# /bin/sh (there is none here anyway), and the previous `wget https://…`
# forked BusyBox ssl_client and exited without reaping it. Container PID 1
# is this Go binary, which never calls wait(2), so each check leaked one
# zombie onto the HOST process table — ~2/min, forever. Any replacement
# must stay fork-free.
#
# The runtime timeout stays above the probe's own deadline (5 s default) so a
# hung check reports its own error instead of being killed mid-flight.
HEALTHCHECK --interval=30s --timeout=10s --start-period=30s \
    CMD ["/usr/local/bin/polarbeam-server", "healthcheck", "--config", "/etc/polarbeam/server.yaml"]
ENTRYPOINT ["/usr/local/bin/polarbeam-server"]
CMD ["serve", "--config", "/etc/polarbeam/server.yaml"]
