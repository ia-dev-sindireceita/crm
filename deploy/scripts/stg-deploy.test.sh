#!/usr/bin/env bash
# deploy/scripts/stg-deploy.test.sh — hermetic wrapper test for the
# image-carried carry-set extraction added to `stg-deploy.sh` in SIN-66620
# (SIN-66600 impl 2/4, ADR 0111).
#
# Style follows the SIN-65902 payment-deploy hermetic harness: the real
# `stg-deploy.sh` is driven end-to-end with `docker` and `cosign` SHIMMED on
# PATH and every host path redirected into a `mktemp -d` sandbox via ${STG_DIR}.
# NOTHING real is pulled, verified, or `compose up`-ed. The shims record every
# argv so we can assert the fail-closed contract and the no-secret-on-argv
# property WITHOUT a docker daemon — so this runs on any CI runner, before any
# real `compose up` could ever fire.
#
# Covers SecEng C5:
#   (a) a missing carry-set artifact inside the image aborts non-zero and
#       leaves the host copies untouched (never falls back to the stale copy),
#   (b) an empty artifact aborts non-zero (distinct code) with host untouched,
#   (c) a successful extraction overwrites a host-tampered copy with the
#       signed bytes,
#   (d) no secret value ever appears on any docker/cosign argv,
#   plus: the abort in (a)/(b) happens BEFORE any `compose up`.
#
# Usage: bash deploy/scripts/stg-deploy.test.sh

set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
SCRIPT="deploy/scripts/stg-deploy.sh"

# A valid ghcr digest ref (matches DIGEST_RE in the script under test).
# Declared separately from the command substitution to avoid SC2155.
IMG_DIGEST="$(printf 'a%.0s' {1..64})"
readonly IMG="ghcr.io/pericles-luz/crm@sha256:${IMG_DIGEST}"
# A fake secret planted in the sandbox .env.stg; assertion (d) proves this
# literal never reaches any docker/cosign argv.
readonly FAKE_SECRET="P0stgres-SEKRET-do-not-leak-42"

failures=0
pass() { echo "PASS  $1"; }
fail() { echo "FAIL  $1"; failures=$((failures + 1)); }

# ----------------------------------------------------------------------
# Sandbox + shim construction
# ----------------------------------------------------------------------

# make_shims <bindir> — write shim `docker` + `cosign` that log every argv to
# ${DOCKER_ARGV_LOG}/${COSIGN_ARGV_LOG}. `docker cp <c>:/deploy/. <dst>` copies
# ${FAKE_DEPLOY_SRC}/. into <dst> (empty/unset ⇒ non-zero = /deploy absent).
make_shims() {
	local bin="$1"
	mkdir -p "$bin"
	cat >"$bin/cosign" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" >>"${COSIGN_ARGV_LOG}"
echo '---' >>"${COSIGN_ARGV_LOG}"
exit 0
SH
	cat >"$bin/docker" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" >>"${DOCKER_ARGV_LOG}"
echo '---' >>"${DOCKER_ARGV_LOG}"
case "$1" in
  create)
    echo "fakecarrier0000"
    ;;
  cp)
    # $2 = fakecarrier:/deploy/.   $3 = <workdir>/
    if [[ -z "${FAKE_DEPLOY_SRC:-}" || ! -d "${FAKE_DEPLOY_SRC}" ]]; then
      exit 1
    fi
    cp -a "${FAKE_DEPLOY_SRC}/." "$3"
    ;;
  rm|compose|system|inspect|pull|push)
    : # no-op, exit 0
    ;;
  *)
    : # default no-op
    ;;
esac
exit 0
SH
	chmod +x "$bin/docker" "$bin/cosign"
}

# seed_host <stgdir> — a "provisioned but stale/tampered" host layout.
seed_host() {
	local stg="$1"
	mkdir -p "$stg/caddy" "$stg/scripts/minio"
	printf 'APP_IMAGE=ghcr.io/pericles-luz/crm@sha256:%s\nPOSTGRES_USER=stg\nPOSTGRES_PASSWORD=%s\nPOSTGRES_DB=crm\n' \
		"$(printf 'b%.0s' {1..64})" "$FAKE_SECRET" >"$stg/.env.stg"
	printf 'HOST-STALE-COMPOSE\n' >"$stg/compose.stg.yml"
	printf 'HOST-STALE-UNBOUND\n' >"$stg/caddy/unbound.conf"
	printf 'HOST-STALE-CADDYFILE\n' >"$stg/caddy/Caddyfile.stg"
	printf 'HOST-STALE-HEADERS\n' >"$stg/caddy/security-headers.caddy"
	printf 'HOST-STALE-MINIO\n' >"$stg/scripts/minio/init-quarantine.sh"
}

# seed_image <srcdir> <marker> — a fresh, signed /deploy carry-set. Every file
# is non-empty and secret-free.
seed_image() {
	local src="$1" marker="$2"
	mkdir -p "$src/caddy" "$src/scripts/minio"
	printf 'FRESH-SIGNED-COMPOSE-%s\n' "$marker" >"$src/compose.stg.yml"
	printf 'FRESH-SIGNED-CADDYFILE-%s\n' "$marker" >"$src/caddy/Caddyfile.stg"
	printf 'FRESH-SIGNED-HEADERS-%s\n' "$marker" >"$src/caddy/security-headers.caddy"
	printf 'FRESH-SIGNED-UNBOUND-%s\n' "$marker" >"$src/caddy/unbound.conf"
	printf '#!/bin/sh\necho fresh-minio-%s\n' "$marker" >"$src/scripts/minio/init-quarantine.sh"
	chmod +x "$src/scripts/minio/init-quarantine.sh"
}

# run_deploy <stgdir> <fake_deploy_src> <docker_log> <cosign_log> — invoke the
# real script's `deploy` verb fully hermetically; echoes the exit code.
run_deploy() {
	local stg="$1" src="$2" dlog="$3" clog="$4" bin
	bin="$(mktemp -d)"
	make_shims "$bin"
	SSH_ORIGINAL_COMMAND="deploy ${IMG}" \
		STG_DIR="$stg" \
		COSIGN=cosign \
		FAKE_DEPLOY_SRC="$src" \
		DOCKER_ARGV_LOG="$dlog" \
		COSIGN_ARGV_LOG="$clog" \
		PATH="$bin:$PATH" \
		bash "$SCRIPT" >/dev/null 2>&1
	local rc=$?
	rm -rf "$bin"
	echo "$rc"
}

# ----------------------------------------------------------------------
# (happy) full carry-set ⇒ exit 0, host-tampered copies overwritten, minio
#         executable preserved, compose up ran AFTER install, no secret leak.
# ----------------------------------------------------------------------
t_happy_overwrites_tampered() {
	local stg src dlog clog rc
	stg="$(mktemp -d)"; src="$(mktemp -d)"; dlog="$(mktemp)"; clog="$(mktemp)"
	seed_host "$stg"
	seed_image "$src" "HAPPY"

	rc="$(run_deploy "$stg" "$src" "$dlog" "$clog")"
	[[ "$rc" == "0" ]] || { fail "happy: exit=$rc want 0"; return; }

	# (c) host-tampered compose overwritten with signed bytes.
	grep -q '^FRESH-SIGNED-COMPOSE-HAPPY$' "$stg/compose.stg.yml" \
		|| { fail "happy: compose.stg.yml not overwritten with signed bytes"; return; }
	grep -q '^FRESH-SIGNED-UNBOUND-HAPPY$' "$stg/caddy/unbound.conf" \
		|| { fail "happy: unbound.conf not overwritten"; return; }
	grep -q '^FRESH-SIGNED-HEADERS-HAPPY$' "$stg/caddy/security-headers.caddy" \
		|| { fail "happy: security-headers.caddy not overwritten"; return; }
	grep -q 'fresh-minio-HAPPY' "$stg/scripts/minio/init-quarantine.sh" \
		|| { fail "happy: minio init script not overwritten"; return; }
	[[ -x "$stg/scripts/minio/init-quarantine.sh" ]] \
		|| { fail "happy: minio init script lost exec bit (cp -p)"; return; }

	# compose up must have run, and strictly AFTER the carry-set create/cp.
	grep -q 'compose' "$dlog" || { fail "happy: compose never invoked"; return; }
	local first_up first_cp
	first_up="$(grep -nxF 'up' "$dlog" | head -n1 | cut -d: -f1)"
	first_cp="$(grep -nE '^create$|:/deploy/\.$' "$dlog" | head -n1 | cut -d: -f1)"
	[[ -n "$first_up" && -n "$first_cp" && "$first_cp" -lt "$first_up" ]] \
		|| { fail "happy: carry-set install did not precede compose up (cp=$first_cp up=$first_up)"; return; }

	# (d) no secret literal on any docker/cosign argv.
	if grep -qF "$FAKE_SECRET" "$dlog" "$clog"; then
		fail "happy: secret leaked onto docker/cosign argv"; return
	fi
	pass "happy: overwrites tampered host copy, minio +x, compose after install, no secret leak"
}

# ----------------------------------------------------------------------
# (a) a named artifact missing inside image ⇒ exit 69, host untouched,
#     compose up never reached.
# ----------------------------------------------------------------------
t_missing_artifact_aborts() {
	local stg src dlog clog rc
	stg="$(mktemp -d)"; src="$(mktemp -d)"; dlog="$(mktemp)"; clog="$(mktemp)"
	seed_host "$stg"
	seed_image "$src" "MISS"
	rm -f "$src/caddy/unbound.conf"  # drop one carried file (simulated COPY drift)

	rc="$(run_deploy "$stg" "$src" "$dlog" "$clog")"
	[[ "$rc" == "69" ]] || { fail "missing: exit=$rc want 69"; return; }

	# host copies untouched (never fall back to stale — they stay stale, not new).
	grep -q '^HOST-STALE-COMPOSE$' "$stg/compose.stg.yml" \
		|| { fail "missing: compose.stg.yml was mutated on abort"; return; }
	grep -q '^HOST-STALE-UNBOUND$' "$stg/caddy/unbound.conf" \
		|| { fail "missing: unbound.conf was mutated on abort"; return; }
	# no compose up before the abort.
	grep -qxF 'up' "$dlog" && { fail "missing: compose up ran despite carry-set abort"; return; }
	pass "missing artifact ⇒ exit 69, host untouched, no compose up"
}

# ----------------------------------------------------------------------
# (b) an empty artifact inside image ⇒ exit 70, host untouched.
# ----------------------------------------------------------------------
t_empty_artifact_aborts() {
	local stg src dlog clog rc
	stg="$(mktemp -d)"; src="$(mktemp -d)"; dlog="$(mktemp)"; clog="$(mktemp)"
	seed_host "$stg"
	seed_image "$src" "EMPTY"
	: >"$src/caddy/security-headers.caddy"  # present but zero-length

	rc="$(run_deploy "$stg" "$src" "$dlog" "$clog")"
	[[ "$rc" == "70" ]] || { fail "empty: exit=$rc want 70"; return; }
	grep -q '^HOST-STALE-COMPOSE$' "$stg/compose.stg.yml" \
		|| { fail "empty: compose.stg.yml was mutated on abort"; return; }
	grep -q '^HOST-STALE-HEADERS$' "$stg/caddy/security-headers.caddy" \
		|| { fail "empty: security-headers.caddy was mutated on abort"; return; }
	grep -qxF 'up' "$dlog" && { fail "empty: compose up ran despite carry-set abort"; return; }
	pass "empty artifact ⇒ exit 70, host untouched, no compose up"
}

# ----------------------------------------------------------------------
# /deploy prefix entirely absent (docker cp fails) ⇒ exit 69, host untouched.
# ----------------------------------------------------------------------
t_prefix_absent_aborts() {
	local stg dlog clog rc bin
	stg="$(mktemp -d)"; dlog="$(mktemp)"; clog="$(mktemp)"
	seed_host "$stg"
	# FAKE_DEPLOY_SRC points at a non-existent dir ⇒ shim `docker cp` exits 1.
	bin="$(mktemp -d)"; make_shims "$bin"
	SSH_ORIGINAL_COMMAND="deploy ${IMG}" \
		STG_DIR="$stg" COSIGN=cosign \
		FAKE_DEPLOY_SRC="$(mktemp -u)" \
		DOCKER_ARGV_LOG="$dlog" COSIGN_ARGV_LOG="$clog" \
		PATH="$bin:$PATH" \
		bash "$SCRIPT" >/dev/null 2>&1
	rc=$?
	rm -rf "$bin"
	[[ "$rc" == "69" ]] || { fail "prefix-absent: exit=$rc want 69"; return; }
	grep -q '^HOST-STALE-COMPOSE$' "$stg/compose.stg.yml" \
		|| { fail "prefix-absent: compose.stg.yml mutated on abort"; return; }
	pass "/deploy prefix absent ⇒ exit 69, host untouched"
}

t_happy_overwrites_tampered
t_missing_artifact_aborts
t_empty_artifact_aborts
t_prefix_absent_aborts

echo
if [[ "$failures" -eq 0 ]]; then
	echo "ALL PASS"
	exit 0
fi
echo "${failures} FAILED"
exit 1
