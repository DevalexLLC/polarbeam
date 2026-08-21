# polarbeam-agent images. Build context is the repo root.
#
# CAUTION — stage order makes `dev` the DEFAULT target (dev must follow
# release to build FROM it). Every consumer MUST name its target:
#   production/CI:  --target release
#   dev overlay:    build.target: dev
#
# The container image is the ONLY supported agent distribution. Run it
# with cap_add: [NET_RAW] (required on runtimes whose default bounding set
# drops it, e.g. podman; the binary also carries the file capability for
# runtimes that honor it).
FROM --platform=$BUILDPLATFORM golang:1.27.0-alpine AS build
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

FROM alpine:3.22 AS release
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="polarbeam-agent" \
      org.opencontainers.image.description="PolarBEAM site connectivity agent" \
      org.opencontainers.image.source="https://github.com/devalexllc/polarbeam" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
COPY --from=build /out/polarbeam-agent /usr/local/bin/polarbeam-agent
# License + attribution notices travel with every redistribution, images
# included. /licenses is the OCI convention.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
# File capability grants raw ICMP (echo fallback + traceroute + path MTU)
# to the non-root user; libcap is build-time only.
RUN apk add --no-cache libcap \
    && setcap cap_net_raw+ep /usr/local/bin/polarbeam-agent \
    && apk del libcap \
    && adduser -S -D -H -u 10001 -s /sbin/nologin polarbeam \
    && mkdir -p /var/lib/polarbeam-agent \
    && chown 10001 /var/lib/polarbeam-agent
# Entrypoint wrapper runs selfcheck before `run` (fail-loud preflight).
COPY deploy/docker/agent-release-entrypoint.sh /usr/local/bin/agent-release-entrypoint.sh
RUN chmod 0755 /usr/local/bin/agent-release-entrypoint.sh
USER 10001
ENTRYPOINT ["agent-release-entrypoint.sh"]
CMD ["run", "--config", "/etc/polarbeam/agent.yaml"]

# Dev image for the compose overlay ONLY: root + iptables/iproute2 so the
# M4 gate can inject outages in-container, and an entrypoint that enrolls
# from the bootstrap token volume. Never published.
FROM release AS dev
USER root
RUN apk add --no-cache iptables iproute2
COPY deploy/compose-dev/agent-entrypoint.sh /usr/local/bin/agent-entrypoint.sh
RUN chmod +x /usr/local/bin/agent-entrypoint.sh
ENTRYPOINT ["agent-entrypoint.sh"]
