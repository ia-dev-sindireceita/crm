#!/usr/bin/env bash
# check-deploy-artifacts-parity.sh — SIN-66621 / ADR 0111 (SIN-66600) C6 gate.
#
# Ticket 1/4 (SIN-66619) taught the Dockerfile to COPY the staging deploy
# topology (compose / caddy / unbound / scripts-minio) into the cosign-signed
# image under a stable /deploy prefix. Ticket 2/4 (SIN-66620) teaches the
# deploy wrapper to `docker cp` /deploy out of the just-verified image and
# install it on the host. That chain only holds if the bytes the image
# carries are IDENTICAL to the repo sources — otherwise a Dockerfile COPY
# path could silently drift from source and staging would run stale infra.
#
# This gate turns "repo == image == host" from *trusted* into *checked*: it
# asserts every carried artifact under the image's /deploy tree is
# byte-identical to its repo source, that none are missing, and that the
# image carries no /deploy file the parity manifest does not know about
# (which would mean a new Dockerfile COPY landed without updating this gate).
#
# The docker build + extraction lives in CI
# (.github/workflows/deploy-artifacts-image-parity.yml): it builds the
# crm-server target, `docker cp CID:/deploy/.` into a temp dir, and hands
# that directory to this script. Decoupling the byte comparison from docker
# keeps this logic hermetically self-testable without a daemon — see
# scripts/check-deploy-artifacts-parity.test.sh.
#
# Usage:
#   scripts/check-deploy-artifacts-parity.sh <extracted-deploy-dir> [repo-root]
#
# <extracted-deploy-dir> is the directory holding the CONTENTS of the image's
# /deploy (i.e. the target of `docker cp CID:/deploy/. <dir>`), so it contains
# compose.stg.yml, caddy/, scripts/. [repo-root] defaults to the git top-level.
#
# Exit codes:
#   0  every carried artifact is present and byte-identical to its repo source
#   1  parity violation — a missing, mismatched, or unexpected/extra /deploy file
#   2  usage error / a manifest repo source is missing (gate misconfiguration)

set -uo pipefail

log() { echo "$@" >&2; }

usage() {
	log "usage: $0 <extracted-deploy-dir> [repo-root]"
	exit 2
}

EXTRACTED="${1:-}"
[[ -n "$EXTRACTED" ]] || usage
if [[ ! -d "$EXTRACTED" ]]; then
	log "FAIL: extracted /deploy dir '${EXTRACTED}' is not a directory"
	exit 2
fi
EXTRACTED="${EXTRACTED%/}"

REPO_ROOT="${2:-}"
if [[ -z "$REPO_ROOT" ]]; then
	REPO_ROOT="$(git -C "$(dirname "$0")" rev-parse --show-toplevel 2>/dev/null || true)"
fi
[[ -n "$REPO_ROOT" ]] || REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
REPO_ROOT="${REPO_ROOT%/}"
if [[ ! -d "$REPO_ROOT" ]]; then
	log "FAIL: repo root '${REPO_ROOT}' is not a directory"
	exit 2
fi

# ---------------------------------------------------------------------------
# Parity manifest — MUST mirror the `COPY … /deploy/…` block in the Dockerfile
# (SIN-66619). Left side = repo source, right side = path under the image's
# /deploy. Single files are listed explicitly; whole-directory COPYs are
# expressed as tree rules and expanded by walking the repo source below.
#
# Keep this in lockstep with the Dockerfile. The "unexpected extra image
# file" check at the end fails the gate if the Dockerfile grows a new
# /deploy COPY that is not represented here, so drift is caught in both
# directions rather than silently trusted.
# ---------------------------------------------------------------------------

# repo-source  ->  image-path-relative-to-/deploy
declare -a FILE_SRC=(
	"deploy/compose/compose.stg.yml"
	"deploy/caddy/Caddyfile.stg"
	"deploy/caddy/security-headers.caddy"
	"infra/caddy/unbound.conf"
)
declare -a FILE_DST=(
	"compose.stg.yml"
	"caddy/Caddyfile.stg"
	"caddy/security-headers.caddy"
	"caddy/unbound.conf"
)

# Whole-directory carries: repo-source-dir -> image-dir-relative-to-/deploy.
# Every regular file under the repo dir is expected at the mirrored image path.
declare -a TREE_SRC=(
	"scripts/minio"
)
declare -a TREE_DST=(
	"scripts/minio"
)

# Build the expected map (image-relpath -> repo-source-relpath) by expanding
# the manifest. A missing repo source is a gate misconfiguration (exit 2),
# distinct from a parity violation (exit 1).
declare -A EXPECT=()

add_expected() {
	local src="$1" dst="$2"
	if [[ ! -f "${REPO_ROOT}/${src}" ]]; then
		log "MISCONFIG: manifest repo source '${src}' does not exist under ${REPO_ROOT}"
		exit 2
	fi
	EXPECT["$dst"]="$src"
}

for i in "${!FILE_SRC[@]}"; do
	add_expected "${FILE_SRC[$i]}" "${FILE_DST[$i]}"
done

for i in "${!TREE_SRC[@]}"; do
	tsrc="${TREE_SRC[$i]}"
	tdst="${TREE_DST[$i]}"
	if [[ ! -d "${REPO_ROOT}/${tsrc}" ]]; then
		log "MISCONFIG: manifest repo tree '${tsrc}' does not exist under ${REPO_ROOT}"
		exit 2
	fi
	while IFS= read -r -d '' abs; do
		rel="${abs#"${REPO_ROOT}/${tsrc}/"}"
		add_expected "${tsrc}/${rel}" "${tdst}/${rel}"
	done < <(find "${REPO_ROOT}/${tsrc}" -type f -print0)
done

if [[ ${#EXPECT[@]} -eq 0 ]]; then
	log "MISCONFIG: parity manifest is empty"
	exit 2
fi

fail=0

# 1) Every expected artifact must exist in the image and match byte-for-byte.
for dst in "${!EXPECT[@]}"; do
	src="${EXPECT[$dst]}"
	img="${EXTRACTED}/${dst}"
	repo="${REPO_ROOT}/${src}"
	if [[ ! -f "$img" ]]; then
		log "FAIL: /deploy/${dst} MISSING from image (expected carry of repo ${src})"
		fail=1
		continue
	fi
	if cmp -s "$repo" "$img"; then
		log "OK:   /deploy/${dst} == ${src}"
	else
		log "FAIL: /deploy/${dst} BYTE MISMATCH vs repo ${src} (Dockerfile COPY drifted from source)"
		fail=1
	fi
done

# 2) The image must carry NO /deploy file the manifest does not expect. A new
#    file here means a Dockerfile COPY was added without updating this gate.
while IFS= read -r -d '' abs; do
	rel="${abs#"${EXTRACTED}/"}"
	if [[ -z "${EXPECT[$rel]+x}" ]]; then
		log "FAIL: image carries /deploy/${rel} which is NOT in the parity manifest (add it to this gate + confirm the Dockerfile COPY)"
		fail=1
	fi
done < <(find "$EXTRACTED" -type f -print0)

if (( fail == 0 )); then
	log "deploy-artifacts parity OK — ${#EXPECT[@]} carried artifact(s) byte-identical to repo"
fi
exit "$fail"
