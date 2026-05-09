#!/usr/bin/env bash
# check-security-headers.sh — assert the security-headers contract from
# ADR 0082 §1 (SIN-62229) against a running Caddy edge, and validate that
# Content-Security-Policy carries a per-request nonce that rotates between
# requests (PR-B3 / SIN-62285).
#
# Usage:
#   scripts/check-security-headers.sh                            # defaults below
#   scripts/check-security-headers.sh http://localhost:8080/
#   EXPECTED_HSTS_MAX_AGE=63072000 scripts/check-security-headers.sh https://...
#
# Knobs:
#   EXPECTED_HSTS_MAX_AGE   override the staging default of 300 (e.g. 63072000 in prod).
#   REQUIRE_NONCE=0         skip the CSP nonce-rotation assertion. Set this when
#                           the URL points at a static asset where the Go
#                           middleware does not run and only the Caddy literal
#                           CSP fallback is expected.
#
# Exits non-zero on the first failed assertion so it can be wired into CI smoke
# stages or run manually after staging deploys.

set -euo pipefail

URL="${1:-http://localhost:8080/}"
EXPECTED_HSTS_MAX_AGE="${EXPECTED_HSTS_MAX_AGE:-300}"
REQUIRE_NONCE="${REQUIRE_NONCE:-1}"

fetch_headers() {
	# curl -sS shows transport errors but stays quiet on success; -I uses HEAD which
	# triggers Caddy's response headers without pulling the body.
	curl -sSI "$URL"
}

HEADERS="$(fetch_headers)"

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

# extract_csp <headers-blob>  →  prints the CSP value (header line minus the field name).
extract_csp() {
	grep -i '^content-security-policy:' <<<"$1" \
		| head -n1 \
		| sed -E 's/^[Cc]ontent-[Ss]ecurity-[Pp]olicy:[[:space:]]*//' \
		| tr -d '\r'
}

# extract_nonce <csp-value>  →  prints the first nonce token, empty if absent.
# Matches the ADR 0082 §1 placeholder shape: 'nonce-<base64url>'.
extract_nonce() {
	grep -oE "nonce-[A-Za-z0-9_-]+" <<<"$1" | head -n1 || true
}

header_value "Strict-Transport-Security" "max-age=${EXPECTED_HSTS_MAX_AGE}"
header_value "Strict-Transport-Security" "includeSubDomains"
header_value "X-Content-Type-Options"     "nosniff"
header_value "X-Frame-Options"            "DENY"
header_value "Referrer-Policy"            "strict-origin-when-cross-origin"
header_value "Permissions-Policy"         "geolocation=()"
header_value "Permissions-Policy"         "interest-cohort=()"
header_value "Cross-Origin-Opener-Policy"   "same-origin"
header_value "Cross-Origin-Resource-Policy" "same-origin"
header_value "Cross-Origin-Embedder-Policy" "require-corp"

# CSP must be present on every response — backend nonce path or Caddy literal fallback.
header_present "Content-Security-Policy"
header_value   "Content-Security-Policy" "default-src 'self'"
header_value   "Content-Security-Policy" "frame-ancestors 'none'"
header_value   "Content-Security-Policy" "object-src 'none'"
header_value   "Content-Security-Policy" "base-uri 'self'"
header_value   "Content-Security-Policy" "form-action 'self'"

header_absent "Server"
header_absent "X-Powered-By"

# Nonce-rotation check: two HEADs back-to-back must yield different nonces on
# dynamic HTML routes (the Go middleware mints a fresh value per request).
# Skipped when REQUIRE_NONCE=0 — useful for static-asset URLs where the
# Caddy literal fallback (no nonce) is the expected response.
if [[ "$REQUIRE_NONCE" == "1" ]]; then
	csp1="$(extract_csp "$HEADERS")"
	nonce1="$(extract_nonce "$csp1")"

	headers2="$(fetch_headers)"
	csp2="$(extract_csp "$headers2")"
	nonce2="$(extract_nonce "$csp2")"

	if [[ -z "$nonce1" || -z "$nonce2" ]]; then
		echo "MISSING nonce in Content-Security-Policy (set REQUIRE_NONCE=0 to skip on static assets)" >&2
		echo "  request 1 csp: ${csp1}" >&2
		echo "  request 2 csp: ${csp2}" >&2
		fail=1
	elif [[ "$nonce1" == "$nonce2" ]]; then
		echo "BAD: CSP nonce did not rotate between two consecutive requests (got ${nonce1} twice)" >&2
		fail=1
	fi
fi

if [[ $fail -ne 0 ]]; then
	echo "security headers check: FAIL ($URL)" >&2
	exit 1
fi
echo "security headers check: OK ($URL, HSTS max-age=${EXPECTED_HSTS_MAX_AGE}, REQUIRE_NONCE=${REQUIRE_NONCE})"
