#!/usr/bin/env bash
# Self-test for scripts/ci/check-licenses.py (SIN-66267).
#
# Proves the gate bites in both directions WITHOUT needing the full Go
# toolchain or network: it feeds crafted go-licenses-style CSV into the checker
# and asserts the exit code. The live end-to-end proof (go-licenses over the
# real module tree) runs in the license-scan.yml CI job.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CHECK="$HERE/check-licenses.py"
fails=0

expect() {
  # expect <name> <want_exit> <<<csv
  local name="$1" want="$2" got
  local input; input="$(cat)"
  printf '%s' "$input" | python3 "$CHECK" >/dev/null 2>&1
  got=$?
  if [[ "$got" == "$want" ]]; then
    echo "ok   - $name (exit $got)"
  else
    echo "FAIL - $name: want exit $want, got $got"
    fails=$((fails + 1))
  fi
}

# Clean tree: only permissive/MPL → pass (0).
expect "clean tree passes" 0 <<'CSV'
github.com/example/foo,https://x/LICENSE,Apache-2.0
go.mau.fi/whatsmeow,https://x/LICENSE,MPL-2.0
golang.org/x/crypto,https://x/LICENSE,BSD-3-Clause
CSV

# Allowlisted libsignal present, nothing else copyleft → pass (0).
expect "allowlisted libsignal passes" 0 <<'CSV'
go.mau.fi/whatsmeow,https://x/LICENSE,MPL-2.0
go.mau.fi/libsignal,https://x/LICENSE,GPL-3.0
CSV

# libsignal sub-package also covered by the allowlist prefix → pass (0).
expect "allowlist covers sub-packages" 0 <<'CSV'
go.mau.fi/libsignal/ecc,https://x/LICENSE,GPL-3.0
CSV

# A *new* GPL dep (not libsignal) → blocked (1).
expect "new GPL dep blocked" 1 <<'CSV'
github.com/evil/gpl-lib,https://x/LICENSE,GPL-3.0
CSV

# SPDX -or-later / -only variants of the family are matched → blocked (1).
expect "GPL-3.0-or-later blocked" 1 <<'CSV'
github.com/evil/lib,https://x/LICENSE,GPL-3.0-or-later
CSV
expect "GPL-2.0-only blocked" 1 <<'CSV'
github.com/evil/lib,https://x/LICENSE,GPL-2.0-only
CSV

# AGPL family blocked too (1).
expect "AGPL-3.0 blocked" 1 <<'CSV'
github.com/evil/saas,https://x/LICENSE,AGPL-3.0
CSV

# LGPL is intentionally NOT matched by this gate → pass (0).
expect "LGPL not matched" 0 <<'CSV'
github.com/some/lgpl-lib,https://x/LICENSE,LGPL-3.0
CSV

# Empty report → fail-closed (2), never a silent pass.
expect "empty report fails closed" 2 <<'CSV'
CSV

# Unknown licenses do not fail the gate (still 0), but a real GPL alongside
# them still blocks (1).
expect "unknown licenses tolerated" 0 <<'CSV'
github.com/x/y,https://x/LICENSE,Unknown
github.com/a/b,https://x/LICENSE,MIT
CSV
expect "unknown plus GPL still blocks" 1 <<'CSV'
github.com/x/y,https://x/LICENSE,Unknown
github.com/evil/lib,https://x/LICENSE,GPL-3.0
CSV

if [[ "$fails" -gt 0 ]]; then
  echo "license-gate self-test: $fails failure(s)"
  exit 1
fi
echo "license-gate self-test: all checks passed"
