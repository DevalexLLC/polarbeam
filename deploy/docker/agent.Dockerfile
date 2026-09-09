# polarbeam-agent images. Build context is the repo root.
# The release build is fully offline once base images are present
# (vendored deps only, no package installs).
#
# Stages: base (runtime rootfs) → build (Go cross-compile + file
# capability) → dev (alpine, compose-dev overlay ONLY) → release (distroless,
# the published image). `release` is last so it is the DEFAULT target; `make
# images`, ci.yml and release.yml still name it explicitly. The dev overlay
# selects `dev`.
#
# The container image is the ONLY supported agent distribution. Run it with
# cap_add: [NET_RAW]. The binary carries the cap_net_raw+ep file capability,
# which is what gives uid 10001 raw ICMP at all (Docker's --cap-add raises
# the sets for root only), and the kernel refuses to exec such a binary
# when NET_RAW is outside the bounding set (podman, rootless and narrowed
# daemons) — hence the flag on every invocation there.
#
# The file capability is a security.capability xattr written by
# tools/filecap in the build stage (no libcap, no apk) and carried across
# COPY --from. That needs BuildKit (the default builder since Docker 23);
# the legacy builder drops xattrs on COPY --from. ci.yml runs the built
# image to prove the capability survived.
#
# Runtime base, named once so the digest lives in one place: the release
# stage builds FROM it and the build stage copies its passwd/group. Static
# distroless: no shell, no package manager, no libc; ships ca-certificates,
# tzdata, /etc/passwd, /tmp. Pinned by OCI index digest (multi-arch) because
# the tag carries no versions. Same image and digest as server.Dockerfile —
# bump both together.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:1c2c046bc09ed40fad370b599a0b1ae7987f55b01e247cf27a7c27cd97e5bbc7 AS base

# The build stage runs on the BUILD platform and cross-compiles via
# GOOS/GOARCH — multi-arch buildx never emulates the Go compiler. Nothing
# below runs on the target platform, so the release pipeline needs no QEMU.
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
    -o /out/polarbeam-agent ./cmd/polarbeam-agent
# cap_net_raw+ep as the 20-byte VFS_CAP_REVISION_2 record setcap would write
# (raw ICMP for the echo fallback, traceroute and path MTU as uid 10001).
# Runs natively on the build platform; the xattr is arch-independent. Any
# write or chown to the file afterwards clears it, hence last, and no
# --chown on the COPY below.
RUN CGO_ENABLED=0 go run -mod=vendor ./tools/filecap /out/polarbeam-agent
# Distroless has no RUN, so the release stage's filesystem extras are
# prepared here: the uid 10001 identity line the alpine image had (uid
# 10001, gid 65533, home /home/polarbeam) and the state directory. The
# group line is skipped if the base already defines gid 65533.
COPY --from=base /etc/passwd /etc/group /out/etc/
RUN printf 'polarbeam:x:10001:65533::/home/polarbeam:/sbin/nologin\n' >> /out/etc/passwd \
    && if ! grep -q ':65533:' /out/etc/group; then printf 'polarbeam:x:65533:\n' >> /out/etc/group; fi \
    && mkdir -p /out/state

# Dev image for the compose overlay ONLY: root + iptables/iproute2 so the
# M4 gate can inject outages in-container, and an entrypoint that enrolls
# from the bootstrap token volume. Declared before release so the default
# target stays release. Never published; its apk step is the one image
# build that still needs an Alpine package source.
FROM alpine:3.22 AS dev
RUN apk add --no-cache iptables iproute2 \
    && adduser -S -D -H -u 10001 -s /sbin/nologin polarbeam \
    && mkdir -p /var/lib/polarbeam-agent \
    && chown 10001 /var/lib/polarbeam-agent
COPY --from=build /out/polarbeam-agent /usr/local/bin/polarbeam-agent
# Every image carries the notices, the dev one included.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY deploy/compose-dev/agent-entrypoint.sh /usr/local/bin/agent-entrypoint.sh
RUN chmod +x /usr/local/bin/agent-entrypoint.sh
ENTRYPOINT ["agent-entrypoint.sh"]

FROM base AS release
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="polarbeam-agent" \
      org.opencontainers.image.description="PolarBEAM site connectivity agent" \
      org.opencontainers.image.source="https://github.com/devalexllc/polarbeam" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
# Same passwd/group as the base plus the polarbeam entry (see build stage).
COPY --from=build /out/etc/passwd /out/etc/group /etc/
# Fixed uid so volume ownership survives image rebuilds: a fresh state
# volume copies this directory's owner on first create. 10001:0 is what
# `mkdir` as root + `chown 10001` produced before.
COPY --from=build --chown=10001:0 /out/state /var/lib/polarbeam-agent
# No --chown/--chmod here: either would strip the file capability.
COPY --from=build /out/polarbeam-agent /usr/local/bin/polarbeam-agent
# License + attribution notices travel with every redistribution, images
# included. /licenses is the OCI convention.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
# uid only, as before: gid 65533 resolves through the passwd entry above.
USER 10001
# The base sets WORKDIR /home/nonroot; the previous image ran from /, so
# relative paths in the config keep resolving identically.
WORKDIR /
# `run` performs the selfcheck preflight itself (fail-loud), replacing the
# former shell entrypoint wrapper. No HEALTHCHECK: the agent exposes no
# endpoint, liveness is the control plane's offline detection, and a
# forking probe under a PID 1 that never reaps would leak zombies (see
# server.Dockerfile).
ENTRYPOINT ["/usr/local/bin/polarbeam-agent"]
CMD ["run", "--config", "/etc/polarbeam/agent.yaml"]
