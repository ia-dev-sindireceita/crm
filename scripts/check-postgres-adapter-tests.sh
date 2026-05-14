#!/usr/bin/env bash
# check-postgres-adapter-tests.sh — fail if any *_test.go file lives under
# a subpackage of internal/adapter/db/postgres/.
#
# Why this guard exists (SIN-62750):
#
# All adapter tests under internal/adapter/db/postgres/ MUST live in the
# parent postgres_test package, not in a subpackage_test package. `go test
# -race ./...` starts every package's test binary in parallel, and each
# binary that calls testpg.Start() bootstraps the SHARED Postgres cluster
# (CI TEST_DATABASE_URL) by ALTERing the app_admin / app_runtime /
# app_master_ops role passwords to its own per-process value. Two binaries
# racing on that ALTER yield SQLSTATE 28P01 (password authentication
# failed) for whichever bootstrap got overwritten — the deterministic CI
# failure pattern observed when PR #80 placed wallet tests in
# internal/adapter/db/postgres/wallet/*_test.go.
#
# The precedent is internal/adapter/db/postgres/contacts_adapter_test.go
# (commit 7d9cf39, SIN-62726). New adapter packages follow the same shape:
# adapter code in a subpackage, tests in the parent postgres_test package.
#
# Usage: scripts/check-postgres-adapter-tests.sh
# Exits 0 if no violations are found; exits 1 with the offending paths.

set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
target_dir="${root}/internal/adapter/db/postgres"

if [[ ! -d "${target_dir}" ]]; then
  echo "check-postgres-adapter-tests: directory not found: ${target_dir}" >&2
  exit 2
fi

# -mindepth 2 excludes ${target_dir}/*_test.go (the parent package) and
# only matches subpackage test files like
# ${target_dir}/wallet/wallet_test.go.
violations=$(find "${target_dir}" -mindepth 2 -name '*_test.go' -type f -print)

if [[ -n "${violations}" ]]; then
  echo "check-postgres-adapter-tests: forbidden subpackage *_test.go files found:" >&2
  echo "${violations}" | sed 's|^|  |' >&2
  cat <<'EOF' >&2

These tests must live in the parent postgres_test package — see
internal/adapter/db/postgres/contacts_adapter_test.go (SIN-62726) and
internal/adapter/db/postgres/wallet_adapter_test.go (SIN-62750) for the
canonical pattern. Move the tests up one directory and switch the file
package declaration to `package postgres_test`.
EOF
  exit 1
fi

echo "check-postgres-adapter-tests: ok (no subpackage *_test.go files under internal/adapter/db/postgres)"
