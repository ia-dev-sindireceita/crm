#!/usr/bin/env bash
# Regression test for scripts/govulncheck-sweep.sh (SIN-62251).
#
# Strategy: feed the script a hand-crafted govulncheck-shaped JSON fixture
# and assert the deterministic output of `extract-called`, `diff`,
# `group-by-library`, and the full `report` pipeline. We do not invoke real
# govulncheck here — the production govulncheck binary is exercised by the
# CI workflow in .github/workflows/govulncheck.yml, and by the actual
# routine fire (SIN-62251) in production.
#
# Run with: bash tools/supply-chain/test_govulncheck_sweep.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
SWEEP_SH="${REPO_ROOT}/scripts/govulncheck-sweep.sh"

if [[ ! -x "${SWEEP_SH}" ]]; then
  echo "ERROR: ${SWEEP_SH} not found or not executable" >&2
  exit 1
fi

if ! command -v jq >/dev/null 2>&1; then
  echo "ERROR: jq is required to run these tests" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT

failures=0
pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*" >&2; failures=$((failures + 1)); }

# ---- fixture ----------------------------------------------------------------
# Three findings:
#   GO-2025-AAA — *called* (trace[0].function set), library golang.org/x/crypto, HIGH
#   GO-2025-BBB — *called*, same library golang.org/x/crypto, MEDIUM (group test)
#   GO-2025-CCC — *called*, library github.com/foo/bar, CRITICAL
#   GO-2025-DDD — import-only (trace function empty), should be skipped
# Plus matching osv records so group-by-library can resolve severity + library.
FIXTURE="${work}/govulncheck.json"
cat > "${FIXTURE}" <<'JSON'
{"config":{"protocol_version":"v1.0.0"}}
{"osv":{"id":"GO-2025-AAA","summary":"AAA","details":"d","database_specific":{"severity":"HIGH"},"affected":[{"package":{"name":"golang.org/x/crypto","ecosystem":"Go"}}]}}
{"osv":{"id":"GO-2025-BBB","summary":"BBB","details":"d","database_specific":{"severity":"MEDIUM"},"affected":[{"package":{"name":"golang.org/x/crypto","ecosystem":"Go"}}]}}
{"osv":{"id":"GO-2025-CCC","summary":"CCC","details":"d","database_specific":{"severity":"CRITICAL"},"affected":[{"package":{"name":"github.com/foo/bar","ecosystem":"Go"}}]}}
{"osv":{"id":"GO-2025-DDD","summary":"DDD","details":"d","database_specific":{"severity":"LOW"},"affected":[{"package":{"name":"github.com/foo/baz","ecosystem":"Go"}}]}}
{"finding":{"osv":"GO-2025-AAA","trace":[{"module":"golang.org/x/crypto","function":"DoSign","package":"golang.org/x/crypto/ssh"}]}}
{"finding":{"osv":"GO-2025-BBB","trace":[{"module":"golang.org/x/crypto","function":"VerifyHandshake","package":"golang.org/x/crypto/ssh"}]}}
{"finding":{"osv":"GO-2025-CCC","trace":[{"module":"github.com/foo/bar","function":"Decode","package":"github.com/foo/bar"}]}}
{"finding":{"osv":"GO-2025-DDD","trace":[{"module":"github.com/foo/baz","function":"","package":"github.com/foo/baz"}]}}
JSON

# ---- Case 1: extract-called drops the import-only finding -------------------
echo "==> Case 1: extract-called keeps only called findings"
ids="$("${SWEEP_SH}" extract-called "${FIXTURE}")"
expected="GO-2025-AAA
GO-2025-BBB
GO-2025-CCC"
if [[ "${ids}" == "${expected}" ]]; then
  pass "called-only filter is exact"
else
  fail "extract-called mismatch; got=[${ids}] want=[${expected}]"
fi

# ---- Case 2: diff finds new + resolved ids ---------------------------------
echo "==> Case 2: diff identifies new and resolved ids vs baseline"
baseline="${work}/baseline.txt"
current="${work}/current.txt"
cat > "${baseline}" <<'EOF'
GO-2024-OLD-RESOLVED
GO-2025-AAA
EOF
"${SWEEP_SH}" extract-called "${FIXTURE}" > "${current}"
diff_out="$("${SWEEP_SH}" diff "${current}" "${baseline}")"
if grep -qF $'new\tGO-2025-BBB' <<<"${diff_out}" \
   && grep -qF $'new\tGO-2025-CCC' <<<"${diff_out}" \
   && grep -qF $'resolved\tGO-2024-OLD-RESOLVED' <<<"${diff_out}" \
   && ! grep -qF $'new\tGO-2025-AAA' <<<"${diff_out}"; then
  pass "diff correctly classifies new vs resolved"
else
  fail "diff output unexpected: ${diff_out}"
fi

# ---- Case 3: empty baseline file is treated as empty (auto-create) ---------
echo "==> Case 3: missing baseline file is treated as empty"
missing_baseline="${work}/does-not-exist.txt"
diff_out="$("${SWEEP_SH}" diff "${current}" "${missing_baseline}")"
new_count=$(grep -c $'^new\t' <<<"${diff_out}" || true)
resolved_count=$(grep -c $'^resolved\t' <<<"${diff_out}" || true)
if [[ "${new_count}" == "3" && "${resolved_count}" == "0" ]]; then
  pass "missing baseline → all current ids classified as new"
else
  fail "missing-baseline mismatch; new=${new_count} resolved=${resolved_count}"
fi

# ---- Case 4: group-by-library resolves library + severity per id -----------
echo "==> Case 4: group-by-library produces (library, id, severity) rows"
ids_file="${work}/wanted.txt"
printf 'GO-2025-BBB\nGO-2025-CCC\n' > "${ids_file}"
g="$("${SWEEP_SH}" group-by-library "${FIXTURE}" "${ids_file}")"
if grep -qF $'golang.org/x/crypto\tGO-2025-BBB\tMEDIUM' <<<"${g}" \
   && grep -qF $'github.com/foo/bar\tGO-2025-CCC\tCRITICAL' <<<"${g}"; then
  pass "library + severity resolved via osv records"
else
  fail "group-by-library output unexpected: ${g}"
fi

# ---- Case 5: report end-to-end JSON shape (with empty BASELINE) ------------
# Run report against a freshly-created empty BASELINE_FILE so all called ids
# are classified as new. We override BASELINE_FILE through a sandbox copy of
# the script (sed-rewrite) — same approach test_deploy_gate.sh uses.
echo "==> Case 5: report emits a stable JSON shape"
sandbox="${work}/govulncheck-sweep.sh"
sandbox_baseline="${work}/baseline-sandbox.txt"
: > "${sandbox_baseline}"
sed -e "s|BASELINE_FILE=.*|BASELINE_FILE=\"${sandbox_baseline}\"|" \
  "${SWEEP_SH}" > "${sandbox}"
chmod +x "${sandbox}"
report_json="$("${sandbox}" report "${FIXTURE}")"
# Validate JSON shape: top-level keys, lists, grouping rows.
if jq -e '
  (.generated_at | type == "string") and
  (.current_ids  | length == 3) and
  (.new_ids      | length == 3) and
  (.resolved_ids | length == 0) and
  (.new_findings | length >= 3) and
  (.new_findings | map(.osv) | sort | join(",") == "GO-2025-AAA,GO-2025-BBB,GO-2025-CCC") and
  (.new_findings | map(select(.osv == "GO-2025-CCC")) | .[0].severity == "CRITICAL") and
  (.new_findings | map(select(.osv == "GO-2025-AAA")) | .[0].library == "golang.org/x/crypto")
' >/dev/null <<<"${report_json}"; then
  pass "report JSON shape and field values are correct"
else
  fail "report JSON failed assertions: $(jq -c . <<<"${report_json}")"
fi

# ---- Case 6: unknown subcommand exits non-zero -----------------------------
echo "==> Case 6: unknown subcommand prints usage and exits non-zero"
if "${SWEEP_SH}" not-a-real-command >/dev/null 2>&1; then
  fail "unknown subcommand returned 0"
else
  pass "unknown subcommand returned non-zero"
fi

# ---- Case 7: validate — false-clean guard (SIN-67201) ----------------------
# A package-load / toolchain failure makes govulncheck exit non-zero while
# producing NO findings (and typically no config record on stdout). The guard
# must refuse to pass that off as a clean scan.
echo "==> Case 7: validate rejects a package-load failure (no config, exit!=0)"
loadfail="${work}/loadfail.json"
: > "${loadfail}"   # govulncheck wrote everything to stderr; stdout is empty
if "${SWEEP_SH}" validate "${loadfail}" 1 >/dev/null 2>&1; then
  fail "validate accepted an empty (no-config) scan output"
else
  pass "validate rejects empty scan output with non-zero exit"
fi

# Even with exit 0, an empty/structureless stream is not a real scan.
echo "==> Case 7b: validate rejects a structureless stream even on exit 0"
if "${SWEEP_SH}" validate "${loadfail}" 0 >/dev/null 2>&1; then
  fail "validate accepted a no-config stream on exit 0"
else
  pass "validate rejects no-config stream on exit 0"
fi

# A config record present but exit!=0 with zero findings is still a failure
# (e.g. the loader errored after emitting config). Must NOT read as clean.
echo "==> Case 7c: validate rejects config-only stream with non-zero exit"
configonly="${work}/configonly.json"
printf '%s\n' '{"config":{"protocol_version":"v1.0.0"}}' > "${configonly}"
printf '%s\n' '{"progress":{"message":"Scanning..."}}' >> "${configonly}"
if "${SWEEP_SH}" validate "${configonly}" 2 >/dev/null 2>&1; then
  fail "validate accepted non-zero exit with zero findings"
else
  pass "validate rejects non-zero exit with zero findings"
fi

# ---- Case 8: validate — legitimate scans pass ------------------------------
# Clean scan: config + progress present, exit 0, no findings → valid, clean.
echo "==> Case 8: validate accepts a clean scan (config, exit 0, no findings)"
if "${SWEEP_SH}" validate "${configonly}" 0 >/dev/null 2>&1; then
  pass "validate accepts a genuine clean scan"
else
  fail "validate rejected a legitimate clean scan"
fi

# Vulns-found scan: findings present, exit!=0 → valid; report still wanted.
echo "==> Case 8b: validate accepts a vulns-found scan (findings, exit!=0)"
if "${SWEEP_SH}" validate "${FIXTURE}" 3 >/dev/null 2>&1; then
  pass "validate accepts non-zero exit when findings are present"
else
  fail "validate rejected a legitimate vulns-found scan"
fi

# ---- Case 9: sweep end-to-end fails loud on a stubbed load failure ---------
# Put a fake `govulncheck` on PATH that mimics a toolchain/load error: it
# writes to stderr and exits non-zero with EMPTY stdout. `sweep` must exit
# non-zero and must NOT print a clean JSON report.
echo "==> Case 9: sweep exits non-zero on a simulated package-load failure"
stubdir="${work}/stub-fail"
mkdir -p "${stubdir}"
cat > "${stubdir}/govulncheck" <<'STUB'
#!/usr/bin/env bash
echo "govulncheck: loading packages: this package requires newer Go" >&2
exit 1
STUB
chmod +x "${stubdir}/govulncheck"
sweep_out="${work}/sweep-fail.out"
if PATH="${stubdir}:${PATH}" "${SWEEP_SH}" sweep >"${sweep_out}" 2>/dev/null; then
  fail "sweep returned 0 on a simulated load failure"
elif jq -e '.new_ids' >/dev/null 2>&1 <"${sweep_out}"; then
  fail "sweep emitted a (false-clean) JSON report on a load failure"
else
  pass "sweep fails loud and emits no clean report on a load failure"
fi

# ---- Case 9b: sweep end-to-end succeeds on a stubbed clean scan ------------
# Fake `govulncheck` that emits a valid clean stream (config + progress) and
# exits 0. `sweep` must exit 0 and emit a clean report.
echo "==> Case 9b: sweep succeeds and emits a clean report on a valid clean scan"
stubok="${work}/stub-ok"
mkdir -p "${stubok}"
cat > "${stubok}/govulncheck" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' '{"config":{"protocol_version":"v1.0.0"}}'
printf '%s\n' '{"progress":{"message":"Scanning your code..."}}'
exit 0
STUB
chmod +x "${stubok}/govulncheck"
sweep_ok="${work}/sweep-ok.out"
if PATH="${stubok}:${PATH}" "${SWEEP_SH}" sweep >"${sweep_ok}" 2>/dev/null \
   && jq -e '(.new_ids | length == 0) and (.current_ids | length == 0)' >/dev/null 2>&1 <"${sweep_ok}"; then
  pass "sweep emits a clean report on a genuine clean scan"
else
  fail "sweep did not emit the expected clean report: $(cat "${sweep_ok}" 2>/dev/null)"
fi

echo
if [[ "${failures}" -gt 0 ]]; then
  echo "RESULT: ${failures} failure(s)" >&2
  exit 1
fi
echo "RESULT: all govulncheck-sweep cases passed"
