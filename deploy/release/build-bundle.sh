#!/bin/sh
# Assemble the air-gapped control-plane install bundle: one docker-load
# image tarball (server, proxy, agent) + the production compose file +
# config examples + docs + SHA256SUMS.
#
# The TimescaleDB image is deliberately NOT bundled: releases redistribute
# only PolarBEAM's own artifacts, never third-party images (which also
# kept every bundle >1 GiB). Offline operators transfer it themselves —
# the exact pinned reference is recorded in the bundle's
# TIMESCALEDB-IMAGE file, derived from the production compose file so the
# two cannot drift; docs/install.md documents the transfer.
#
# Usage: deploy/release/build-bundle.sh <version> [arch]
#   version  image tag to bundle (e.g. v1.0.0); must exist locally or be
#            pullable from $REGISTRY
#   arch     amd64 (default) | arm64
#
# Offline-friendly: local images are used as-is; pulling only happens for
# images that are absent. Fails loudly rather than assembling a partial
# bundle.
set -eu

VERSION="${1:?usage: build-bundle.sh <version> [arch]}"
ARCH="${2:-amd64}"
REGISTRY="${REGISTRY:-ghcr.io/devalexllc}"

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
NAME="polarbeam-${VERSION}-${ARCH}-bundle"
OUT="${ROOT}/dist/${NAME}"

if [ -e "$OUT" ] || [ -e "${OUT}.tar.gz" ]; then
    echo "error: ${OUT}(.tar.gz) already exists — remove it first (refusing to overwrite)" >&2
    exit 1
fi

# The third-party DB image operators must supply; the tag comes from the
# compose file (the single source of truth), fail loud if the pin moved.
TSDB_IMAGE="$(awk '$1 == "image:" && $2 ~ /^timescale\// { print $2; exit }' \
    "${ROOT}/deploy/compose/docker-compose.yml")"
[ -n "$TSDB_IMAGE" ] || {
    echo "error: no timescale/* image pin found in deploy/compose/docker-compose.yml" >&2
    exit 1
}

# The tag is rolling, so the bundle records an immutable digest-qualified
# reference — offline installs must be able to fetch the exact bytes this
# release was assembled against. Prefer the local image's RepoDigest (the
# fully-offline rebuild path never touches the registry); otherwise ask
# the registry for the manifest digest.
if docker image inspect "$TSDB_IMAGE" >/dev/null 2>&1; then
    TSDB_PINNED="$(docker image inspect --format '{{index .RepoDigests 0}}' "$TSDB_IMAGE" 2>/dev/null || true)"
else
    d="$(docker buildx imagetools inspect "$TSDB_IMAGE" --format '{{.Manifest.Digest}}' 2>/dev/null || true)"
    [ -n "$d" ] && TSDB_PINNED="${TSDB_IMAGE}@${d}" || TSDB_PINNED=""
fi
[ -n "$TSDB_PINNED" ] || {
    echo "error: could not resolve ${TSDB_IMAGE} to a digest (need the image locally or registry access)" >&2
    exit 1
}

IMAGES="${REGISTRY}/polarbeam-server:${VERSION} \
${REGISTRY}/polarbeam-agent:${VERSION} \
${REGISTRY}/polarbeam-proxy:${VERSION}"

for img in $IMAGES; do
    if docker image inspect "$img" >/dev/null 2>&1; then
        echo "bundle: using local image $img"
    else
        echo "bundle: pulling $img (linux/${ARCH})"
        docker pull --platform "linux/${ARCH}" "$img"
    fi
done

mkdir -p "${OUT}/images"
echo "bundle: docker save → images/polarbeam-images-${VERSION}-${ARCH}.tar"
# shellcheck disable=SC2086
docker save -o "${OUT}/images/polarbeam-images-${VERSION}-${ARCH}.tar" $IMAGES

cp "${ROOT}/deploy/compose/docker-compose.yml" "${OUT}/"
cp "${ROOT}/deploy/compose/server.example.yaml" "${OUT}/"
cp "${ROOT}/deploy/compose/env.example" "${OUT}/"
cp "${ROOT}/docs/install.md" "${OUT}/"
# The license and attribution notices travel with every redistribution
# (AGPL-3.0 conveyance terms + third-party license obligations).
cp "${ROOT}/LICENSE" "${ROOT}/NOTICE" "${ROOT}/THIRD-PARTY-NOTICES" "${OUT}/"
printf '%s\n' "$VERSION" > "${OUT}/VERSION"
# Machine-readable, digest-qualified pointer for transfer tooling:
#   docker pull "$(cat TIMESCALEDB-IMAGE)"
printf '%s\n' "$TSDB_PINNED" > "${OUT}/TIMESCALEDB-IMAGE"

(cd "$OUT" && find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS)
tar -C "${ROOT}/dist" -czf "${OUT}.tar.gz" "$NAME"
echo "bundle: wrote ${OUT}.tar.gz"
echo "bundle: NOTE — ${TSDB_IMAGE} is deliberately not included; offline installs transfer ${TSDB_PINNED} separately (see install.md)"
sha256sum "${OUT}.tar.gz"
