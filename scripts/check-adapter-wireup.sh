#!/usr/bin/env bash
# check-adapter-wireup.sh — fail when any internal/adapter/httpapi/<sub>
# package is unreachable from the production cmd/server import graph.
#
# Why this guard exists (SIN-63339, structural follow-up to SIN-63337 / F11):
#
# An internal/adapter/httpapi/<X> package can be fully implemented, unit-tested
# via httptest, and **completely orphaned** from the cmd/server import graph.
# Handler tests pass; the routes do not exist in the running binary because no
# production .go file imports the package. We have hit this class of bug twice
# in two weeks:
#
#   - SIN-63303 — /static/ FileServer unmounted in production router.
#   - SIN-63337 / F11 — tenant 2FA admin handler (/admin/2fa/*) orphaned;
#     caught by SecurityEngineer's manual re-sweep, not by CI.
#
# This script reproduces what SecurityEngineer would have to check by hand:
# walk every sub-package under internal/adapter/httpapi/* and confirm it
# appears in the closure of `go list -deps ./cmd/server/...`. That closure
# excludes test-only imports by design, so a package whose only importers are
# *_test.go files (the F11 shape) drops out and the check fails.
#
# Wireup-target helper packages (e.g. internal/adapter/httpapi/sessioncookie,
# .../csrf) are reached transitively via internal/adapter/httpapi/router.go,
# which is itself imported from cmd/server — they pass the check naturally
# without needing the allowlist.
#
# Allowlist mechanism:
#
# Some packages may be legitimately unreachable from cmd/server at a point in
# time (parked behind a feature flag, consumed only by an off-by-default
# command, etc.). They are recorded in scripts/adapter-wireup-allowlist.txt
# with a one-line comment per entry explaining why. Lines beginning with `#`
# and blank lines are ignored. Each entry is a full import path, e.g.
# `github.com/pericles-luz/crm/internal/adapter/httpapi/foo`.
#
# Out of scope:
#
# The narrow first pass only covers internal/adapter/httpapi/*. Other adapter
# trees (db, channel, message broker) have different wireup contracts and
# will be considered separately if and when value is proven (ADR 0106 §D3).

set -euo pipefail

GO="${GO:-go}"

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"

target_glob='./internal/adapter/httpapi/...'
parent_pkg='github.com/pericles-luz/crm/internal/adapter/httpapi'
prod_seed='./cmd/server/...'
allowlist_file='scripts/adapter-wireup-allowlist.txt'

# Load allowlist (full import paths, one per line; `#` comments + blanks ok).
allowlist=()
if [[ -f "${allowlist_file}" ]]; then
  while IFS= read -r raw; do
    line="${raw%%#*}"
    line="$(echo -n "${line}" | tr -d '[:space:]')"
    [[ -z "${line}" ]] && continue
    allowlist+=("${line}")
  done < "${allowlist_file}"
fi

is_allowed() {
  local pkg="$1"
  local entry
  for entry in "${allowlist[@]+"${allowlist[@]}"}"; do
    [[ "${entry}" == "${pkg}" ]] && return 0
  done
  return 1
}

# Sub-packages under internal/adapter/httpapi/ (strictly: not the parent
# package itself — router.go lives in the parent and is always wired).
mapfile -t subs < <("${GO}" list "${target_glob}" | grep -vxF "${parent_pkg}" | sort -u)

if (( ${#subs[@]} == 0 )); then
  echo "check-adapter-wireup: no sub-packages under ${target_glob} — nothing to check" >&2
  exit 0
fi

# Production closure (deps of cmd/server/...; excludes test-only imports).
prod_closure="$("${GO}" list -deps "${prod_seed}" | sort -u)"

violations=()
allowed_seen=()
for pkg in "${subs[@]}"; do
  if grep -qxF "${pkg}" <<<"${prod_closure}"; then
    continue
  fi
  if is_allowed "${pkg}"; then
    allowed_seen+=("${pkg}")
    continue
  fi
  violations+=("${pkg}")
done

if (( ${#allowed_seen[@]} > 0 )); then
  for pkg in "${allowed_seen[@]}"; do
    echo "check-adapter-wireup: allowlisted (orphan permitted): ${pkg}"
  done
fi

if (( ${#violations[@]} > 0 )); then
  {
    echo
    echo "check-adapter-wireup: FAIL — the following internal/adapter/httpapi/* packages are NOT reachable"
    echo "from cmd/server (production import graph). Their handlers will compile and unit-test cleanly, but"
    echo "the running binary will not serve any of their routes (F11 / SIN-63337 failure shape):"
    echo
    for v in "${violations[@]}"; do
      echo "  - ${v}"
    done
    cat <<'EOF'

Fix one of:

  1. Wire the package into internal/adapter/httpapi/router.go (or a cmd/server
     bootstrap file such as cmd/server/<name>_wire.go) so cmd/server transitively
     imports it.

  2. If the package is legitimately not router-mounted (shared helper consumed
     only via composition, or future-dated handler parked behind a feature flag),
     add its full import path to scripts/adapter-wireup-allowlist.txt with a
     one-line comment explaining why.

See docs/adr/0106-wireup-lint-adapter-httpapi.md for the full rationale.
EOF
  } >&2
  exit 1
fi

total="${#subs[@]}"
allow_n="${#allowed_seen[@]}"
reach_n="$(( total - allow_n ))"
if (( allow_n > 0 )); then
  echo "check-adapter-wireup: ok — ${reach_n}/${total} reachable from cmd/server, ${allow_n} allowlisted"
else
  echo "check-adapter-wireup: ok — ${total} sub-packages under internal/adapter/httpapi all reachable from cmd/server"
fi
