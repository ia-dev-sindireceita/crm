#!/usr/bin/env bash
# cd-stg-preflight_test.sh — SIN-63348 + SIN-63350 regression guard.
#
# Asserts that .github/workflows/cd-stg.yml carries BOTH preflight steps
# that gate the CD pipeline against known-failure modes on the VPS:
#
#   - SIN-63350 `preflight cosign present on VPS` (BEFORE deploy via ssh)
#     detects a missing/off-PATH cosign binary on the VPS, which would
#     otherwise blow up mid-deploy with `stg-deploy: cosign not found in
#     PATH` exit 67 (cd-stg run 26334123677).
#   - SIN-63348 `preflight wrapper supports migrate-up` (BETWEEN /health
#     and migrate-up) detects a stale /opt/crm/stg/bin/deploy.sh that
#     lacks the SIN-63332 migrate-up verb (cd-stg run that produced the
#     cryptic `usage: deploy <image-ref>` exit 64).
#
# Also asserts that deploy/scripts/stg-deploy.sh keeps the canonical
# `migrate-up` and `preflight` labels on its usage line and that the
# `preflight` verb dispatch is in place. If a future refactor strips any
# of these, this test fails red so the CD pipeline cannot silently
# regress to either failure mode.
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

# 4. Step ordering: preflight cosign -> deploy via ssh -> /health smoke
#    -> preflight wrapper -> migrate-up -> /login smoke. Any reorder
#    that puts the cosign preflight AFTER deploy re-opens the SIN-63350
#    failure mode (deploy step exit 67 with no actionable remediation);
#    any reorder that puts migrate-up before the wrapper preflight, or
#    the wrapper preflight before /health, re-opens the SIN-63347 gap.
order=$(grep -n '^      - name:' "${WORKFLOW}" | awk -F'name: ' '{print $2}' | tr -d '\r')
expected_subseq=(
  'preflight cosign present on VPS'
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

# 7. SIN-63350 — cosign-preflight step is present with the exact
#    canonical name the workflow documents in its SIN-63350 comment
#    block.
grep -q 'name: preflight cosign present on VPS' "${WORKFLOW}" \
  || fail "preflight cosign step missing from ${WORKFLOW} (SIN-63350)"

# 8. SIN-63350 — the new preflight step SSHes the `preflight` verb on
#    the wrapper (no image-ref). The literal verb is what the workflow
#    expects on the wire; if the verb is renamed without updating the
#    wrapper, both must change together. A bare `"preflight"` token on
#    its own line inside the cd-stg.yml ssh invocation is the precise
#    grep target.
grep -q '"preflight"' "${WORKFLOW}" \
  || fail "preflight verb invocation '\"preflight\"' missing from ${WORKFLOW} (SIN-63350)"

# 9. SIN-63350 — wrapper usage line mentions `preflight` so the verb is
#    discoverable on the VPS via `deploy.sh` with no arguments.
grep -q 'usage:.*preflight' "${WRAPPER}" \
  || fail "${WRAPPER} usage line no longer mentions 'preflight' (SIN-63350 contract broken)"

# 10. SIN-63350 — wrapper carries the `preflight` verb dispatch branch.
#     A grep for the exact dispatch guard catches accidental removal of
#     the branch even if the usage line still mentions the verb.
grep -q '"${argv\[0\]}" == "preflight"' "${WRAPPER}" \
  || fail "${WRAPPER} no longer dispatches the 'preflight' verb (SIN-63350 contract broken)"

echo "OK: cd-stg preflight regression guard passed"
