#!/usr/bin/env bash
# cd-stg-preflight_test.sh — SIN-63348 regression guard.
#
# Asserts that .github/workflows/cd-stg.yml carries the wrapper-version
# preflight step that detects a stale VPS wrapper before invoking
# migrate-up, AND that deploy/scripts/stg-deploy.sh still emits the
# canonical label `migrate-up` on its usage line. If a future refactor
# strips either, this test fails red so the CD pipeline cannot silently
# regress to the SIN-63347 failure mode.
#
# Runs locally and in CI via `make test-workflow`.

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
WORKFLOW="${ROOT}/.github/workflows/cd-stg.yml"
WRAPPER="${ROOT}/deploy/scripts/stg-deploy.sh"

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

[[ -f "${WORKFLOW}" ]] || fail "missing ${WORKFLOW}"
[[ -f "${WRAPPER}"  ]] || fail "missing ${WRAPPER}"

# 1. cd-stg.yml parses as YAML. We do not require actionlint here so the
#    test runs on a bare Ubuntu image; python3 ships in the standard
#    GitHub runner and on every dev box that already builds the repo.
python3 -c "import yaml,sys; yaml.safe_load(open(sys.argv[1]))" "${WORKFLOW}" \
  || fail "cd-stg.yml does not parse as YAML"

# 2. Preflight step is present with the exact canonical name the workflow
#    documents in the SIN-63348 comment block.
grep -q 'name: preflight wrapper supports migrate-up' "${WORKFLOW}" \
  || fail "preflight step missing from ${WORKFLOW} (SIN-63348)"

# 3. Preflight greps for the literal label `migrate-up`. The wrapper-side
#    label is what the workflow expects to find on the wire — if the grep
#    string is renamed without updating the wrapper, both must change
#    together.
grep -q "grep -q 'migrate-up'" "${WORKFLOW}" \
  || fail "preflight grep target 'migrate-up' missing from ${WORKFLOW}"

# 4. Step ordering: deploy via ssh -> /health smoke -> preflight ->
#    migrate-up -> /login smoke. Any reorder that puts migrate-up before
#    preflight, or preflight before /health, re-opens the SIN-63347 gap.
order=$(grep -n '^      - name:' "${WORKFLOW}" | awk -F'name: ' '{print $2}' | tr -d '\r')
expected_subseq=(
  'deploy via ssh'
  'smoke check /health'
  'preflight wrapper supports migrate-up'
  'migrate-up'
  'smoke check /login'
)
idx=0
while IFS= read -r step; do
  if [[ "${idx}" -lt "${#expected_subseq[@]}" && "${step}" == "${expected_subseq[${idx}]}" ]]; then
    idx=$((idx + 1))
  fi
done <<<"${order}"
if [[ "${idx}" -ne "${#expected_subseq[@]}" ]]; then
  printf 'observed step order:\n%s\n' "${order}" >&2
  fail "cd-stg.yml steps out of order — expected subsequence: ${expected_subseq[*]}"
fi

# 5. Wrapper still emits the canonical `migrate-up` label on its usage
#    line, otherwise the preflight grep will fail in production even when
#    the wrapper IS up-to-date.
grep -q 'usage:.*migrate-up' "${WRAPPER}" \
  || fail "${WRAPPER} usage line no longer mentions 'migrate-up' (SIN-63348 contract broken)"

# 6. bash -n on the wrapper itself catches syntax errors that would make
#    the usage branch unreachable on the VPS.
bash -n "${WRAPPER}" || fail "${WRAPPER} has bash syntax errors"

echo "OK: cd-stg preflight regression guard passed"
