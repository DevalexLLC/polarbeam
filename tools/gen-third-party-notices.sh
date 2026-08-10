#!/usr/bin/env bash
# Regenerate THIRD-PARTY-NOTICES: everything third-party that ships inside a
# PolarBEAM binary or image.
#
# The Apache-2.0 components inside it oblige a redistribution to carry
# their attribution notices (§4(d)), and the MIT/BSD/OFL licenses in the
# tree oblige reproduction of their copyright notices in binary form.
# Both are discharged by shipping this file with every artifact.
#
# FOUR input groups, because "what ships" is wider than vendor/:
#   1. Go standard library — statically linked into both binaries (BSD-3).
#   2. Go modules — vendor/.
#   3. Dashboard SPA — web/dist is embedded into polarbeam-server by
#      web/embed.go, so React et al. are redistributed inside it. Their
#      licenses come from the COMMITTED web/THIRD-PARTY-LICENSES, because
#      node_modules is gitignored and absent from the offline build;
#      `make web` regenerates it next to the dist rebuild.
#   4. Bundled webfont — web/dist/fonts ships JetBrains Mono under the OFL.
#
# Offline by construction: every input is either committed to the repo or
# part of the Go toolchain already required to build. Deterministic:
# modules come from vendor/modules.txt in its existing sorted order, license
# files are sorted within each module.
#
# Fail loud (project constraint): a vendored module with no discoverable
# license file aborts the run rather than silently shipping unattributed
# code.
#
# Run via `make notices`; CI regenerates and relies on offline-build's
# "Working tree must stay clean" step to catch drift.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT="${ROOT}/THIRD-PARTY-NOTICES"
MODULES="${ROOT}/vendor/modules.txt"

[ -f "$MODULES" ] || { echo "notices: $MODULES not found — run make vendor first" >&2; exit 1; }

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

cat >"$tmp" <<'HEADER'
PolarBEAM — third-party notices
================================

GENERATED FILE — do not edit by hand. Regenerate with `make notices`
(reads vendor/ only; no network).

PolarBEAM itself is Copyright 2026 Devalex LLC, licensed under the GNU
Affero General Public License, version 3 only — see LICENSE and NOTICE.

The polarbeam-server and polarbeam-agent binaries are statically linked.
They embed the Go standard library and the third-party Go modules listed
below; polarbeam-server additionally embeds the built dashboard, which
bundles the JavaScript packages and webfont listed at the end. Each
component's license, and its own attribution notice where one exists, is
reproduced in full.

Go modules required by go.mod but not vendored are absent from the build
and are therefore not listed, as is build-time-only tooling.

HEADER

section() {
    {
        printf '\n'
        printf '================================================================================\n'
        printf '%s\n' "$1"
        printf '================================================================================\n'
    } >>"$tmp"
}

# ---- 1. Go standard library ----------------------------------------------
# Statically linked into both binaries; BSD-3-Clause §2 requires the notice
# to accompany binary redistributions.
GOROOT="$(${GO:-go} env GOROOT 2>/dev/null || true)"
[ -n "$GOROOT" ] && [ -f "${GOROOT}/LICENSE" ] || {
    echo "notices: FATAL — cannot locate GOROOT/LICENSE (GOROOT='${GOROOT}')" >&2
    echo "notices: the Go standard library is linked into both binaries and must be attributed" >&2
    exit 1
}
section "Go standard library"
for f in LICENSE PATENTS; do
    [ -f "${GOROOT}/${f}" ] || continue
    printf '\n[go/%s]\n\n' "$f" >>"$tmp"
    cat "${GOROOT}/${f}" >>"$tmp"
done

# ---- 2. Vendored Go modules ----------------------------------------------
section "Go modules (vendor/)"

count=0
missing=()

# modules.txt lists each module as "# <path> <version>"; "## explicit ..."
# lines are annotations for the preceding module, and "=> replacement" forms
# put the effective version last.
while read -r _hash path version rest; do
    [ -n "${path:-}" ] || continue
    dir="${ROOT}/vendor/${path}"
    # A go.mod requirement whose packages are not actually imported has no
    # vendored directory — nothing of it ships, so nothing is owed.
    [ -d "$dir" ] || continue
    case "$rest" in
        *"=>"*) version="${rest##* }" ;;
    esac

    files="$(find "$dir" -type f \( -iname 'LICENSE*' -o -iname 'COPYING*' -o -iname 'NOTICE*' \) \
        ! -name '*.go' | LC_ALL=C sort)"
    if [ -z "$files" ]; then
        missing+=("$path")
        continue
    fi

    {
        printf '\n'
        printf -- '--------------------------------------------------------------------------------\n'
        printf '%s %s\n' "$path" "$version"
        printf -- '--------------------------------------------------------------------------------\n'
    } >>"$tmp"

    while IFS= read -r f; do
        printf '\n[%s]\n\n' "${f#"${ROOT}/vendor/"}" >>"$tmp"
        cat "$f" >>"$tmp"
    done <<<"$files"

    count=$((count + 1))
done < <(grep '^# ' "$MODULES")

if [ ${#missing[@]} -gt 0 ]; then
    echo "notices: FATAL — vendored module(s) with no license file:" >&2
    printf '  %s\n' "${missing[@]}" >&2
    echo "notices: attribution cannot be generated; resolve before shipping" >&2
    exit 1
fi

# ---- 3. Dashboard SPA ------------------------------------------------------
# Committed by `make web`; see the header for why this cannot read
# node_modules directly.
SPA="${ROOT}/web/THIRD-PARTY-LICENSES"
[ -f "$SPA" ] || {
    echo "notices: FATAL — ${SPA#"${ROOT}/"} is missing" >&2
    echo "notices: web/dist is embedded in polarbeam-server; run make web to regenerate it" >&2
    exit 1
}
section "Dashboard SPA (bundled into web/dist, embedded in polarbeam-server)"
# Drop the sub-file's own banner — this file already has one — by starting at
# its first package separator. Structural, not a line count: the banner's
# length is prose and has changed once already.
spa_body="$(sed -n '/^-\{80\}$/,$p' "$SPA")"
[ -n "$spa_body" ] || {
    echo "notices: FATAL — no package sections found in ${SPA#"${ROOT}/"}" >&2
    exit 1
}
printf '\n%s\n' "$spa_body" >>"$tmp"

# ---- 4. Bundled webfont ----------------------------------------------------
FONT_LICENSE="${ROOT}/web/public/fonts/OFL.txt"
[ -f "$FONT_LICENSE" ] || {
    echo "notices: FATAL — ${FONT_LICENSE#"${ROOT}/"} is missing" >&2
    echo "notices: web/dist/fonts ships JetBrains Mono and the OFL must accompany it" >&2
    exit 1
}
section "Bundled webfont"
printf '\nJetBrains Mono (web/dist/fonts/*.woff2)\n\n[web/public/fonts/OFL.txt]\n\n' >>"$tmp"
cat "$FONT_LICENSE" >>"$tmp"

mv "$tmp" "$OUT"
# mktemp creates 0600 and mv preserves it. Git only tracks the executable
# bit, so a fresh clone would check out 0644 while THIS tree stayed 0600 —
# and `COPY` into the images preserves the source mode, leaving the file
# unreadable to the non-root uid 10001 the server and agent run as.
chmod 0644 "$OUT"
trap - EXIT
echo "notices: wrote ${OUT#"${ROOT}/"} (${count} modules)"
