#!/usr/bin/env bash
# Lightweight invariants check for the supply-chain wiring (ADR 0084 / SIN-62247).
# Catches accidental removal of the cosign sign step, the SBOM steps, the
# >=high vulnerability gate, and the Dependabot ecosystems during YAML edits.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CD="${REPO_ROOT}/.github/workflows/cd-stg.yml"
SA="${REPO_ROOT}/.github/workflows/security-alerts.yml"
DB="${REPO_ROOT}/.github/dependabot.yml"
DEPLOY="${REPO_ROOT}/deploy/scripts/stg-deploy.sh"

failures=0
fail() { echo "  FAIL: $*" >&2; failures=$((failures + 1)); }
pass() { echo "  PASS: $*"; }

require_file() {
  if [ ! -f "$1" ]; then
    fail "missing file: $1"
    return 1
  fi
}

contains() {
  local file="$1" needle="$2" label="$3"
  # `-e` + `--` keeps grep from parsing leading dashes in the needle as flags.
  if grep -qF -e "${needle}" -- "${file}"; then
    pass "${label}"
  else
    fail "${label} (looking for: ${needle})"
  fi
}

echo "==> cd-stg.yml invariants (signing + SBOM)"
if require_file "${CD}"; then
  contains "${CD}" "id-token: write"                     "OIDC permission present"
  contains "${CD}" "sigstore/cosign-installer"           "cosign installer step"
  contains "${CD}" "cosign sign --yes"                   "cosign sign step"
  contains "${CD}" "anchore/sbom-action/download-syft"   "syft installer step"
  contains "${CD}" "cyclonedx-json@1.5"                  "CycloneDX 1.5 SBOM generator"
  contains "${CD}" "spdx-json@2.3"                       "SPDX 2.3 SBOM generator"
  contains "${CD}" "--type cyclonedx"                    "cosign attest CycloneDX"
  contains "${CD}" "--type spdxjson"                     "cosign attest SPDX"
  contains "${CD}" "actions/upload-artifact"             "SBOM artifact upload"
fi

echo "==> stg-deploy.sh invariants (verify gate)"
if require_file "${DEPLOY}"; then
  contains "${DEPLOY}" "cosign"                                              "cosign reference present"
  contains "${DEPLOY}" "--certificate-identity-regexp"                       "cosign identity binding"
  contains "${DEPLOY}" "--certificate-oidc-issuer"                           "cosign issuer binding"
  contains "${DEPLOY}" "token.actions.githubusercontent.com"                 "OIDC issuer is GitHub Actions"
  # SIN-63281 — pinned to the fork (ia-dev-sindireceita/crm); the upstream
  # pin only worked while cd-stg.yml ran in the upstream repo. After the
  # 2026-05-13 fork↔upstream reconciliation, CI runs in the fork and the
  # cosign signature identifies that workflow.
  contains "${DEPLOY}" 'https://github\.com/ia-dev-sindireceita/crm/'        "identity regex pinned to fork crm repo (escaped dots)"
fi

# SIN-63281 — lockstep guard: the GHCR namespace cd-stg.yml pushes to MUST
# match the EXPECTED_REPO the on-host deploy script accepts. A drift here
# is exactly what caused the 96/100 cd-stg failures from 2026-05-16 onward
# (workflow pushed to upstream namespace it couldn't write; VPS script
# expected upstream namespace). If either side moves, both must move.
echo "==> cd-stg ↔ stg-deploy lockstep (SIN-63281 recurrence guard)"
if require_file "${CD}" && require_file "${DEPLOY}"; then
  # cd-stg.yml must push under the workflow's own repository_owner so the
  # fork's GITHUB_TOKEN can authenticate the push.
  contains "${CD}"     'IMAGE_REPO: ghcr.io/${{ github.repository_owner }}/crm' \
                                                                             "cd-stg IMAGE_REPO bound to repository_owner"
  # The VPS-side EXPECTED_REPO must accept the namespace the fork's
  # workflow actually pushes to. Both must point to ia-dev-sindireceita/crm
  # until SIN-62322 migrates to Sindireceita; if you change one, change
  # the other in the same PR.
  contains "${DEPLOY}" 'EXPECTED_REPO="ghcr.io/ia-dev-sindireceita/crm"'      "stg-deploy EXPECTED_REPO matches fork namespace"
fi

echo "==> security-alerts.yml invariants"
if require_file "${SA}"; then
  contains "${SA}" "dependabot/alerts"          "queries Dependabot alerts API"
  contains "${SA}" '"high"'                     "checks for high severity"
  contains "${SA}" '"critical"'                 "checks for critical severity"
  contains "${SA}" "exit 1"                     "fails the workflow on hits (not comment-only)"
fi

echo "==> dependabot.yml invariants"
if require_file "${DB}"; then
  contains "${DB}" 'package-ecosystem: "gomod"'           "gomod ecosystem present"
  contains "${DB}" 'package-ecosystem: "github-actions"' "github-actions ecosystem present"
  contains "${DB}" 'package-ecosystem: "docker"'          "docker ecosystem present"
  contains "${DB}" 'interval: "weekly"'                    "weekly schedule"
fi

echo
if [ "${failures}" -gt 0 ]; then
  echo "RESULT: ${failures} workflow invariant(s) failed" >&2
  exit 1
fi
echo "RESULT: all workflow invariants hold"
