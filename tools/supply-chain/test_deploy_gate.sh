#!/usr/bin/env bash
# Regression test for the cosign verify gate in deploy/scripts/stg-deploy.sh.
# ADR 0084 §Verification item 1 (SIN-62247).
#
# Strategy: build a sandbox /opt/crm/stg layout, point the script at it, and
# put fake `cosign` and `docker` on PATH. Assert:
#   - deploy succeeds when cosign verify returns 0;
#   - deploy aborts (non-zero exit, compose NEVER invoked) when cosign verify
#     returns non-zero (= unsigned or invalid signature);
#   - the script refuses an image ref that does not match the digest regex;
#   - the script refuses to start when cosign is missing from PATH.
#
# No real registry, no real cosign, no real docker is required. Run with:
#   bash tools/supply-chain/test_deploy_gate.sh

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DEPLOY_SH="${REPO_ROOT}/deploy/scripts/stg-deploy.sh"

if [ ! -x "${DEPLOY_SH}" ]; then
  echo "ERROR: ${DEPLOY_SH} not found or not executable" >&2
  exit 1
fi

work="$(mktemp -d)"
trap 'rm -rf "${work}"' EXIT
mkdir -p "${work}/bin" "${work}/stg"

# ---- fake docker -------------------------------------------------------------
# Minimal stub: any `docker compose ...` records itself; `docker system prune`
# is a no-op. We never need image inspection here because stg-deploy.sh
# doesn't resolve digests itself — it consumes the digest the runner sends.
cat > "${work}/bin/docker" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
  compose)
    echo "compose \$*" >> "${work}/compose.log"
    exit 0
    ;;
  system)
    # docker system prune -af --volumes=false
    exit 0
    ;;
esac
echo "fake docker: unhandled args: \$*" >&2
exit 99
EOF
chmod +x "${work}/bin/docker"

# ---- sandbox env -------------------------------------------------------------
# stg-deploy.sh hardcodes /opt/crm/stg, so we cannot relocate; instead we
# stub the constants by overlaying via a wrapper. Easiest path: invoke
# stg-deploy.sh with the working dir set to a fake STG_DIR-like layout AND
# patch the readonly STG_DIR on the fly via a `bash -c` wrapper.
#
# NB: we cannot just set readonly variables before sourcing — the script
# re-declares them. So we use a small wrapper that rewrites the absolute
# paths to point at our sandbox.
SANDBOX_SCRIPT="${work}/stg-deploy-sandbox.sh"
sed \
  -e "s|/opt/crm/stg|${work}/stg|g" \
  "${DEPLOY_SH}" > "${SANDBOX_SCRIPT}"
chmod +x "${SANDBOX_SCRIPT}"

# Provision the fake STG_DIR contents the script expects to find on first run.
# SIN-63281 — namespace tracks EXPECTED_REPO in deploy/scripts/stg-deploy.sh.
echo "APP_IMAGE=ghcr.io/ia-dev-sindireceita/crm@sha256:0000000000000000000000000000000000000000000000000000000000000000" \
  > "${work}/stg/.env.stg"
echo "services: {}" > "${work}/stg/compose.stg.yml"

# ---- helpers -----------------------------------------------------------------
install_cosign() {
  local exit_code="$1"
  cat > "${work}/bin/cosign" <<EOF
#!/usr/bin/env bash
echo "fake cosign \$*" >> "${work}/cosign.log"
exit ${exit_code}
EOF
  chmod +x "${work}/bin/cosign"
}

run_deploy() {
  rm -f "${work}/compose.log" "${work}/cosign.log"
  PATH="${work}/bin:${PATH}" \
    "${SANDBOX_SCRIPT}" "$@"
}

failures=0
pass() { echo "  PASS: $*"; }
fail() { echo "  FAIL: $*" >&2; failures=$((failures + 1)); }

# A valid digest-pinned ref (regex requires exactly 64 lowercase hex chars).
# SIN-63281 — namespace tracks EXPECTED_REPO in deploy/scripts/stg-deploy.sh.
GOOD_REF="ghcr.io/ia-dev-sindireceita/crm@sha256:1111111111111111111111111111111111111111111111111111111111111111"

# ---- Case 1: verify succeeds → deploy proceeds -------------------------------
echo "==> Case 1: cosign verify SUCCEEDS — deploy proceeds"
install_cosign 0
if run_deploy deploy "${GOOD_REF}" >/dev/null 2>&1; then
  pass "stg-deploy.sh exited 0 on successful verify"
else
  fail "stg-deploy.sh exited non-zero despite successful verify"
fi
if grep -q '^compose .* pull' "${work}/compose.log" 2>/dev/null && \
   grep -q '^compose .* up'   "${work}/compose.log" 2>/dev/null; then
  pass "compose pull + up were invoked"
else
  fail "compose pull/up were NOT invoked after verify success"
fi
if grep -q 'fake cosign verify' "${work}/cosign.log" 2>/dev/null; then
  pass "cosign verify was invoked"
else
  fail "cosign verify was not invoked"
fi

# ---- Case 2: verify fails → deploy blocked -----------------------------------
echo "==> Case 2: cosign verify FAILS — deploy blocked, compose never called"
install_cosign 1
if run_deploy deploy "${GOOD_REF}" >/dev/null 2>&1; then
  fail "stg-deploy.sh exited 0 despite failed verify (gate is broken!)"
else
  pass "stg-deploy.sh exited non-zero on failed verify"
fi
if [ -s "${work}/compose.log" ] && \
   grep -qE '^compose .* (pull|up)' "${work}/compose.log"; then
  fail "compose pull/up was invoked despite verify failure"
else
  pass "compose pull/up was NOT invoked (gate held)"
fi
if grep -q 'fake cosign verify' "${work}/cosign.log" 2>/dev/null; then
  pass "cosign verify was invoked before refusing"
else
  fail "cosign verify was not even called — script bypassed the gate"
fi

# ---- Case 3: cosign missing → script refuses ---------------------------------
echo "==> Case 3: cosign binary missing — script refuses to deploy"
rm -f "${work}/bin/cosign" "${work}/compose.log"
# Restrict PATH so cosign cannot be found anywhere.
if PATH="${work}/bin" "${SANDBOX_SCRIPT}" deploy "${GOOD_REF}" >/dev/null 2>&1; then
  fail "stg-deploy.sh exited 0 with cosign missing (silent bypass!)"
else
  pass "stg-deploy.sh exited non-zero with cosign missing"
fi
if [ -s "${work}/compose.log" ] && \
   grep -qE '^compose .* (pull|up)' "${work}/compose.log"; then
  fail "compose was invoked even though cosign was missing"
else
  pass "compose was NOT invoked (gate held with cosign absent)"
fi

# ---- Case 4: malformed digest → script refuses --------------------------------
echo "==> Case 4: malformed image ref — script refuses (regex gate)"
install_cosign 0
rm -f "${work}/compose.log" "${work}/cosign.log"
BAD_REF="ghcr.io/ia-dev-sindireceita/crm:floating-tag-not-digest"
if PATH="${work}/bin:${PATH}" "${SANDBOX_SCRIPT}" deploy "${BAD_REF}" >/dev/null 2>&1; then
  fail "stg-deploy.sh accepted a non-digest image ref"
else
  pass "stg-deploy.sh refused a non-digest image ref"
fi
if grep -q 'fake cosign verify' "${work}/cosign.log" 2>/dev/null; then
  fail "cosign verify ran on a malformed ref (regex gate must run first)"
else
  pass "cosign verify was NOT invoked on a malformed ref"
fi

# ---- Case 5: usage error on missing args -------------------------------------
echo "==> Case 5: missing args print usage and exit non-zero"
if PATH="${work}/bin:${PATH}" "${SANDBOX_SCRIPT}" >/dev/null 2>&1; then
  fail "stg-deploy.sh exited 0 with no args; expected usage exit"
else
  pass "stg-deploy.sh exited non-zero with no args (usage)"
fi

echo
if [ "${failures}" -gt 0 ]; then
  echo "RESULT: ${failures} failure(s)" >&2
  exit 1
fi
echo "RESULT: all deploy-gate cases passed"
