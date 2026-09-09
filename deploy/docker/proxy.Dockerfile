# polarbeam-proxy: stock nginx with the SNI-passthrough config baked in,
# so the production compose file and the air-gap bundle need no repo
# checkout for a bind mount. Build context is the repo root.
#
# TLS is NEVER terminated here (see the template). The routed gRPC SNI is
# configurable at runtime via POLARBEAM_GRPC_SNI; the official entrypoint
# renders /etc/nginx/nginx.conf from the template at start. Operators with
# a fully custom config bind-mount their own /etc/nginx/nginx.conf AND an
# empty dir over /etc/nginx/templates (so the render doesn't overwrite it).
#
# Base image: the official alpine-slim variant of the STABLE line, pinned by
# exact version AND OCI index digest.
#   - slim: the full `-alpine` image is built FROM this one and adds only
#     the dynamic-module packages (njs, geoip, image-filter, xslt, acme) and
#     curl. The stream-only config above loads none of them; stream and
#     ssl_preread are compiled into the nginx binary itself. Dropping them
#     removes most of the image's CVE surface (libxml2/xslt/gd/tiff/curl…)
#     and nothing we use. envsubst and the docker-entrypoint.d templating
#     scripts ship in slim too.
#   - stable line: bug and security fixes only; the passthrough needs no
#     mainline features. Dependabot is told to ignore nginx minor/major
#     bumps (see .github/dependabot.yml), so the yearly stable-line move
#     (1.30 -> 1.32) is a hand bump here AND in docs/airgap-build.md.
#   - digest: the INDEX (manifest list) digest, so buildx resolves both
#     linux/amd64 and linux/arm64 from one pin. A bare tag drifts silently
#     — and can also freeze silently: the previous `nginx:1.29-alpine` pin
#     stopped receiving builds when 1.29 left mainline, carrying 36 High
#     findings for months while looking "current". Dependabot updates tag
#     and digest together; by hand:
#       docker buildx imagetools inspect nginx:<ver>-alpine-slim --format '{{.Manifest.Digest}}'
FROM nginx:1.30.4-alpine-slim@sha256:77da26c31397bf6694b4bf93275f5b40b0b120ba1b8f114264b603e592c561d6
ARG VERSION=dev
ARG COMMIT=none
LABEL org.opencontainers.image.title="polarbeam-proxy" \
      org.opencontainers.image.description="PolarBEAM edge proxy (SNI-based TCP passthrough, no TLS termination)" \
      org.opencontainers.image.source="https://github.com/devalexllc/polarbeam" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$COMMIT"
# License + attribution notices travel with every redistribution, images
# included. /licenses is the OCI convention.
# THIRD-PARTY-NOTICES covers the Go modules linked into server/agent, none of
# which ship here — carried anyway so NOTICE's "every release artifact" claim
# holds and all three images are identical in this respect.
COPY LICENSE NOTICE THIRD-PARTY-NOTICES /licenses/
COPY deploy/proxy/nginx.conf.template /etc/nginx/templates/nginx.conf.template
ENV NGINX_ENVSUBST_OUTPUT_DIR=/etc/nginx \
    POLARBEAM_GRPC_SNI=grpc.polarbeam.local
