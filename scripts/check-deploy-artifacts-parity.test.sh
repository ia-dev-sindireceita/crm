#!/usr/bin/env bash
# check-deploy-artifacts-parity.test.sh — hermetic self-test for
# scripts/check-deploy-artifacts-parity.sh (SIN-66621).
#
# The parity gate compares an *extracted* image /deploy tree against the repo
# sources. That comparison needs no docker daemon, so this test synthesizes
# the extracted tree on disk (from the real repo sources) and mutates it to
# reproduce each failure mode, asserting the gate's exit code each time:
#
#   good     : extracted tree byte-identical to repo sources          -> exit 0
#   mismatch : one carried file's bytes differ from repo source        -> exit 1
#   missing  : one carried file absent from the extracted tree         -> exit 1
#   extra    : extracted tree carries a /deploy file not in manifest   -> exit 1
#   badarg   : extracted-dir argument is not a directory               -> exit 2
#
# Usage: scripts/check-deploy-artifacts-parity.test.sh
# Exit 0 on all-pass, 1 on any failure.

set -uo pipefail

cd "$(dirname "$0")/.."
REPO_ROOT="$(pwd)"
GATE="scripts/check-deploy-artifacts-parity.sh"

# The manifest (must mirror the gate). Kept here as (repo-src, image-dst)
# pairs so the test builds a faithful "extracted /deploy" from real bytes.
declare -a SRC=(
	"deploy/compose/compose.stg.yml"
	"deploy/caddy/Caddyfile.stg"
	"deploy/caddy/security-headers.caddy"
	"infra/caddy/unbound.conf"
	"scripts/minio/init-quarantine.sh"
)
declare -a DST=(
	"compose.stg.yml"
	"caddy/Caddyfile.stg"
	"caddy/security-headers.caddy"
	"caddy/unbound.conf"
	"scripts/minio/init-quarantine.sh"
)

# build_extracted <dir> — populate <dir> as a faithful image /deploy tree.
build_extracted() {
	local dir="$1"
	local i src dst
	for i in "${!SRC[@]}"; do
		src="${SRC[$i]}"
		dst="${DST[$i]}"
		mkdir -p "${dir}/$(dirname "$dst")"
		cp "${REPO_ROOT}/${src}" "${dir}/${dst}"
	done
}

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

failures=0
run_case() {
	local name="$1" want="$2" dir="$3"
	local got=0
	bash "$GATE" "$dir" "$REPO_ROOT" >/dev/null 2>"${TMP}/${name}.log" || got=$?
	if [[ "$got" == "$want" ]]; then
		echo "PASS  ${name}  (exit=${got}, want=${want})"
	else
		echo "FAIL  ${name}  (exit=${got}, want=${want})"
		echo "----- stderr -----"
		cat "${TMP}/${name}.log"
		echo "------------------"
		failures=$((failures + 1))
	fi
}

# good — faithful extraction passes.
GOOD="${TMP}/good"
build_extracted "$GOOD"
run_case good 0 "$GOOD"

# mismatch — flip a byte in one carried file.
MISMATCH="${TMP}/mismatch"
build_extracted "$MISMATCH"
printf '\n# drift injected by parity self-test\n' >>"${MISMATCH}/caddy/unbound.conf"
run_case mismatch 1 "$MISMATCH"

# missing — drop a carried file entirely.
MISSING="${TMP}/missing"
build_extracted "$MISSING"
rm -f "${MISSING}/compose.stg.yml"
run_case missing 1 "$MISSING"

# extra — image carries a /deploy file the manifest never declared.
EXTRA="${TMP}/extra"
build_extracted "$EXTRA"
mkdir -p "${EXTRA}/caddy"
printf 'rogue\n' >"${EXTRA}/caddy/rogue.conf"
run_case extra 1 "$EXTRA"

# badarg — extracted dir does not exist -> usage/misconfig exit 2.
run_case badarg 2 "${TMP}/does-not-exist"

if (( failures > 0 )); then
	echo
	echo "${failures} case(s) failed" >&2
	exit 1
fi

echo
echo "all deploy-artifacts parity cases passed"
