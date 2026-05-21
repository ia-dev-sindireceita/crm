#!/usr/bin/env bash
# check-security-headers.sh — assert the Fase 0 security-headers contract from
# ADR 0082 §1 (SIN-62229) against a running Caddy edge.
#
# Usage:
#   scripts/check-security-headers.sh                            # defaults below
#   scripts/check-security-headers.sh http://localhost:8080/
#   EXPECTED_HSTS_MAX_AGE=63072000 scripts/check-security-headers.sh https://...
#
# Exits non-zero on the first failed assertion so it can be wired into CI smoke
# stages or run manually after staging deploys.

set -euo pipefail

URL="${1:-http://localhost:8080/}"
# Fase 6 default (SIN-63218): 1 year — matches deploy/caddy/security-headers.caddy.
# Override via `EXPECTED_HSTS_MAX_AGE=...` only when validating a soak window
# that is intentionally running a smaller max-age.
EXPECTED_HSTS_MAX_AGE="${EXPECTED_HSTS_MAX_AGE:-31536000}"
EXPECTED_HSTS_PRELOAD="${EXPECTED_HSTS_PRELOAD:-1}"

# curl -sS shows transport errors but stays quiet on success; -I uses HEAD which
# triggers Caddy's response headers without pulling the body.
HEADERS="$(curl -sSI "$URL")"

fail=0

# header_present <header-name>
header_present() {
	local name="$1"
	if ! grep -qi "^${name}:" <<<"$HEADERS"; then
		echo "MISSING: ${name}" >&2
		fail=1
	fi
}

# header_absent <header-name>
header_absent() {
	local name="$1"
	if grep -qi "^${name}:" <<<"$HEADERS"; then
		echo "PRESENT (must be stripped): ${name}" >&2
		fail=1
	fi
}

# header_value <header-name> <substring>
header_value() {
	local name="$1" want="$2" line
	line="$(grep -i "^${name}:" <<<"$HEADERS" || true)"
	if [[ -z "$line" ]]; then
		echo "MISSING: ${name}" >&2
		fail=1
		return
	fi
	if ! grep -qF "$want" <<<"$line"; then
		echo "BAD VALUE for ${name}: expected to contain '${want}', got: ${line}" >&2
		fail=1
	fi
}

header_value "Strict-Transport-Security" "max-age=${EXPECTED_HSTS_MAX_AGE}"
header_value "Strict-Transport-Security" "includeSubDomains"
if [[ "$EXPECTED_HSTS_PRELOAD" == "1" ]]; then
	# Fase 6 (SIN-63218): the `preload` flag MUST be present on the header
	# so the host stays eligible for the HSTS preload list. Submission to
	# https://hstspreload.org is one-way and gated on CTO sign-off — the
	# flag here alone does not enroll the host, it just keeps the option
	# open.
	header_value "Strict-Transport-Security" "preload"
fi
header_value "X-Content-Type-Options"     "nosniff"
header_value "X-Frame-Options"            "DENY"
header_value "Referrer-Policy"            "strict-origin-when-cross-origin"
header_value "Permissions-Policy"         "geolocation=()"
header_value "Permissions-Policy"         "interest-cohort=()"
header_value "Cross-Origin-Opener-Policy"   "same-origin"
header_value "Cross-Origin-Resource-Policy" "same-origin"
header_value "Cross-Origin-Embedder-Policy" "require-corp"

header_absent "Server"
header_absent "X-Powered-By"

if [[ $fail -ne 0 ]]; then
	echo "security headers check: FAIL ($URL)" >&2
	exit 1
fi
echo "security headers check: OK ($URL, HSTS max-age=${EXPECTED_HSTS_MAX_AGE})"
