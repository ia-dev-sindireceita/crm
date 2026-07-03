#!/usr/bin/env bash
# check-compose-unbound-parity.test.sh — fixture-driven test runner for
# scripts/check-compose-unbound-parity.sh (SIN-62332).
#
# Each fixture under scripts/testdata/compose-unbound-parity/ has a known
# expected exit code. We invoke the lint with the fixture's compose.yml
# and assert the actual exit matches.
#
# Usage: scripts/check-compose-unbound-parity.test.sh
#
# Exit 0 on all-pass, 1 on any failure.

set -uo pipefail

cd "$(dirname "$0")/.."

LINT="scripts/check-compose-unbound-parity.sh"
ROOT="scripts/testdata/compose-unbound-parity"

# fixture name → expected exit code
declare -A expected=(
	[safe]=0
	[ok]=0
	[missing-unbound]=1
	[missing-dns]=1
	# SIN-66592 — additive case: unbound present but caddy pins the SERVICE
	# NAME (`dns: ["unbound"]`) instead of an IP literal. The gate must
	# reject this (the deploy-crash form) → exit 1.
	[dns-service-name]=1
)

failures=0
for name in "${!expected[@]}"; do
	want="${expected[$name]}"
	compose="${ROOT}/${name}/compose.yml"
	if [[ ! -f "$compose" ]]; then
		echo "MISSING fixture: $compose" >&2
		failures=$((failures+1))
		continue
	fi

	# capture stderr for diagnostics on failure but never let `set -e` from
	# the lint kill the test runner.
	got=0
	bash "$LINT" "$compose" >/dev/null 2>"/tmp/parity-${name}.log" || got=$?

	if [[ "$got" == "$want" ]]; then
		echo "PASS  ${name}  (exit=${got}, want=${want})"
	else
		echo "FAIL  ${name}  (exit=${got}, want=${want})"
		echo "----- stderr -----"
		cat "/tmp/parity-${name}.log"
		echo "------------------"
		failures=$((failures+1))
	fi
done

if (( failures > 0 )); then
	echo
	echo "${failures} fixture(s) failed" >&2
	exit 1
fi

# Also confirm the real composes pass in a default scan — this protects
# against the lint rotting against the production composes themselves.
if ! bash "$LINT" >/dev/null 2>/tmp/parity-default.log; then
	echo "FAIL  default-scan (deploy/compose/*) — see /tmp/parity-default.log" >&2
	cat /tmp/parity-default.log >&2
	exit 1
fi
echo "PASS  default-scan  (deploy/compose/compose*.yml)"

echo
echo "all fixtures passed"
