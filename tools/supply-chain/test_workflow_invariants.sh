#!/usr/bin/env bash
# Lightweight invariants check for the supply-chain wiring (ADR 0084 / SIN-62247).
# Catches accidental removal of the cosign sign step, the SBOM steps, the
# >=high vulnerability gate, and the Dependabot ecosystems during YAML edits.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CD="${REPO_ROOT}/.github/workflows/cd-stg.yml"
BACKUP="${REPO_ROOT}/.github/workflows/build-backup-image.yml"
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
  # SIN-63281 — board direction: only pericles-luz/crm may push to stg.
  # The deploy job MUST be gated on owner so the fork stops 403-ing on
  # every CI-green push. Drop in lockstep with build-backup-image.yml below.
  contains "${CD}" "github.repository_owner == 'pericles-luz'" \
                                                         "fork no-op gate present (SIN-63281)"
fi

echo "==> build-backup-image.yml invariants (fork no-op gate)"
if require_file "${BACKUP}"; then
  # Same SIN-63281 gate; lockstep with cd-stg.yml. Fork PRs still build
  # (validate-only); only push events are restricted to upstream.
  contains "${BACKUP}" "github.repository_owner == 'pericles-luz'" \
                                                         "fork no-op gate present (SIN-63281)"
fi

echo "==> stg-deploy.sh invariants (verify gate)"
if require_file "${DEPLOY}"; then
  contains "${DEPLOY}" "cosign"                                              "cosign reference present"
  contains "${DEPLOY}" "--certificate-identity-regexp"                       "cosign identity binding"
  contains "${DEPLOY}" "--certificate-oidc-issuer"                           "cosign issuer binding"
  contains "${DEPLOY}" "token.actions.githubusercontent.com"                 "OIDC issuer is GitHub Actions"
  contains "${DEPLOY}" 'https://github\.com/pericles-luz/crm/'               "identity regex pinned to crm repo (escaped dots)"
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
