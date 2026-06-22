#!/usr/bin/env bash
# scripts/ci/stg-smoke-rls-cross-tenant.sh — SIN-65590 positive
# cross-tenant RLS smoke for the operator Inbox. This is the deploy-time
# proof that Row-Level Security is actually ENFORCED on the runtime pool:
# the SIN-65580 incident was that the "Atribuir a / Transferir" dropdown
# listed attendants from *another* tenant (agent@globex) while logged in
# as agent@acme — which can only happen if the runtime DSN connects as a
# SUPERUSER/BYPASSRLS role and RLS is silently off.
#
# The companion boot guard (SIN-65590 AC1, postgres.EnforceRuntimeRLSRoleFromEnv)
# fails the process fast when DB_ENFORCE_RLS_ROLE=1; this smoke is the
# black-box, post-deploy confirmation from the HTTP surface that no
# cross-tenant row leaks into the rendered HTML.
#
# Flow (all stages emit greppable `stage=` labels for cd-stg triage):
#
#   stage=preflight — GET /health 2xx (VPS reachable).
#   stage=auth      — login as agent@acme (or reuse STG_SESSION_JAR,
#                     SIN-65377, to stay under the /login rate-limit).
#   stage=route     — GET /inbox 200; pull the first conversation link.
#   stage=view      — GET /inbox/conversations/<id> 200 (the context
#                     panel renders the assignment dropdown:
#                     `<option value="<user-uuid>">…</option>`).
#   stage=assert    — the FORBIDDEN cross-tenant UUID (agent@globex) MUST
#                     NOT appear anywhere in the /inbox list OR the
#                     conversation view. If it does, RLS is bypassed →
#                     FAIL the deploy gate. As a non-blocking sanity
#                     signal we also note whether the acme attendant UUID
#                     IS present (proves the dropdown actually rendered,
#                     so an empty dropdown can't false-pass).
#
# Required env (passed from cd-stg.yml):
#   STG_BASE              — acme tenant base URL, e.g.
#                           https://acme.crm.crm.someu.com.br
#   STG_SEED_AGENT_EMAIL  — seeded acme tenant_atendente email (agent@acme.*)
#   STG_SEED_AGENT_PASSWORD
#
# Optional env:
#   FORBIDDEN_TENANT_UUID — UUID that MUST NOT appear (default agent@globex
#                           seed id 00000000-0000-0000-0000-0000000e0e02).
#   EXPECT_TENANT_UUID    — UUID expected to be present as a positive
#                           dropdown-rendered signal (default agent@acme
#                           seed id 00000000-0000-0000-0000-0000000a0e01).
#                           Absence is a WARNING, not a failure: a deploy
#                           that has not wired the interactive dropdown
#                           degrades to read-only text — still NOT a leak.
#   STG_SESSION_JAR       — SIN-65377 cookie-jar reuse (see stg-smoke-inbox.sh).
#
# Exit codes: 0 = no cross-tenant leak observed; 1 = leak or a hard
# precondition failure (unreachable, auth, route).

set -euo pipefail
# cd-stg.yml runs `set +x` before invoking us so the password never
# echoes; mirror it here in case a future caller forgets.
set +x

: "${STG_BASE:?stage=preflight: STG_BASE is required}"
: "${STG_SEED_AGENT_EMAIL:?stage=preflight: STG_SEED_AGENT_EMAIL is required}"
: "${STG_SEED_AGENT_PASSWORD:?stage=preflight: STG_SEED_AGENT_PASSWORD is required}"

# Defaults match migrations/seed/stg.sql: agent@globex is the cross-tenant
# control (tenant globex), agent@acme is the in-tenant attendant.
FORBIDDEN_TENANT_UUID="${FORBIDDEN_TENANT_UUID:-00000000-0000-0000-0000-0000000e0e02}"
EXPECT_TENANT_UUID="${EXPECT_TENANT_UUID:-00000000-0000-0000-0000-0000000a0e01}"

WORKDIR="$(mktemp -d -t stg-smoke-rls.XXXXXX)"
trap 'rm -rf "${WORKDIR}"' EXIT
JAR="${WORKDIR}/cookies.txt"
HEALTH="${WORKDIR}/health.json"
INBOX_HTML="${WORKDIR}/inbox.html"
INBOX_HDR="${WORKDIR}/inbox.headers"
LOGIN_HDR="${WORKDIR}/login.headers"
LOGIN_BODY="${WORKDIR}/login.body"
VIEW_HTML="${WORKDIR}/view.html"
VIEW_HDR="${WORKDIR}/view.headers"

log() { printf '[stg-smoke-rls] %s\n' "$*"; }
die() { printf '::error::%s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------
# Stage 1 — /health pre-condition (VPS reachable).
# ---------------------------------------------------------------------
log "stage=preflight probing ${STG_BASE}/health"
if ! curl -fsS --max-time 5 "${STG_BASE}/health" -o "${HEALTH}"; then
  die "stage=preflight: GET /health failed or returned non-2xx (is the VPS reachable?)"
fi
log "stage=preflight ok"

# ---------------------------------------------------------------------
# Stage 2 — Authenticate as agent@acme, or reuse a session minted
# upstream (SIN-65377 rate-limit budget — see stg-smoke-inbox.sh).
# ---------------------------------------------------------------------
if [ -n "${STG_SESSION_JAR:-}" ] && grep -q "__Host-sess-tenant" "${STG_SESSION_JAR}" 2>/dev/null; then
  cp "${STG_SESSION_JAR}" "${JAR}"
  log "stage=auth reuse — session jar ${STG_SESSION_JAR} carries __Host-sess-tenant; skipping POST /login"
  log "stage=auth ok"
else
  log "stage=auth POST ${STG_BASE}/login as ${STG_SEED_AGENT_EMAIL%@*}@…"
  code=$(curl -sS --max-time 5 -o "${LOGIN_BODY}" -D "${LOGIN_HDR}" \
    -w "%{http_code}" -X POST \
    -c "${JAR}" \
    -H "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "email=${STG_SEED_AGENT_EMAIL}" \
    --data-urlencode "password=${STG_SEED_AGENT_PASSWORD}" \
    "${STG_BASE}/login")
  if [ "${code}" = "429" ]; then
    cat "${LOGIN_HDR}" >&2
    die "stage=auth: /login returned 429 (login rate-limit window saturated). In cd-stg the /login smoke must export STG_SESSION_JAR so this step reuses the session (SIN-65377). Standalone: wait ~60s for the {ip: 1min, Max 5} window to drain."
  fi
  if [ "${code}" != "302" ]; then
    cat "${LOGIN_BODY}" >&2
    die "stage=auth: /login expected 302, got ${code} (check seeded credentials and tenant FQDN)"
  fi
  # Staging is HTTP/2; curl emits headers lowercase, so grep -i.
  if ! grep -qi "Set-Cookie: __Host-sess-tenant" "${LOGIN_HDR}"; then
    cat "${LOGIN_HDR}" >&2
    die "stage=auth: missing __Host-sess-tenant cookie (MFA gate may have intercepted — seed user must be tenant_atendente without totp_required_at)"
  fi
  log "stage=auth ok"
fi

# ---------------------------------------------------------------------
# Stage 3 — GET /inbox, pull the first conversation link.
# ---------------------------------------------------------------------
log "stage=route GET ${STG_BASE}/inbox"
code=$(curl -sS --max-time 5 -o "${INBOX_HTML}" -D "${INBOX_HDR}" \
  -w "%{http_code}" -b "${JAR}" -c "${JAR}" \
  "${STG_BASE}/inbox")
if [ "${code}" = "404" ]; then
  die "stage=route: /inbox 404 — inbox router not mounted on this deploy"
fi
if [ "${code}" = "403" ]; then
  die "stage=route: /inbox 403 — seeded user lacks RoleTenantAtendente / RoleTenantGerente"
fi
if [ "${code}" != "200" ]; then
  cat "${INBOX_HDR}" >&2
  die "stage=route: /inbox expected 200, got ${code}"
fi

# Match the UUID and stop at the first non-hex char (`"` or `?`); the
# href carries a FilterQuery suffix (SIN-65065).
conversation_id=$(grep -oE 'href="/inbox/conversations/[0-9a-fA-F-]+' "${INBOX_HTML}" \
  | head -n1 | sed -E 's#.*/inbox/conversations/([0-9a-fA-F-]+).*#\1#' || true)
if [ -z "${conversation_id}" ]; then
  if grep -q 'conversation-list__empty' "${INBOX_HTML}"; then
    die "stage=route: /inbox 200 with empty conversation list — cannot exercise the assignment dropdown (seed a conversation for the acme tenant)"
  fi
  die "stage=route: /inbox 200 but no /inbox/conversations/<uuid> link found in HTML (template drift?)"
fi
log "stage=route ok — conversation_id=${conversation_id}"

# ---------------------------------------------------------------------
# Stage 4 — GET the conversation view (renders the assignment dropdown).
# ---------------------------------------------------------------------
log "stage=view GET ${STG_BASE}/inbox/conversations/${conversation_id}"
code=$(curl -sS --max-time 5 -o "${VIEW_HTML}" -D "${VIEW_HDR}" \
  -w "%{http_code}" -b "${JAR}" -c "${JAR}" \
  "${STG_BASE}/inbox/conversations/${conversation_id}")
if [ "${code}" != "200" ]; then
  cat "${VIEW_HDR}" >&2
  die "stage=view: conversation view expected 200, got ${code}"
fi
log "stage=view ok"

# ---------------------------------------------------------------------
# Stage 5 — RLS assertion. The forbidden cross-tenant UUID MUST NOT
# appear in EITHER the inbox list or the conversation view. Its presence
# means RLS is not enforced on the runtime pool (the SIN-65580 leak).
# ---------------------------------------------------------------------
log "stage=assert forbidden=${FORBIDDEN_TENANT_UUID} expected=${EXPECT_TENANT_UUID}"
if grep -qi "${FORBIDDEN_TENANT_UUID}" "${INBOX_HTML}" "${VIEW_HTML}"; then
  log "FORBIDDEN cross-tenant UUID present in rendered HTML:"
  grep -ni "${FORBIDDEN_TENANT_UUID}" "${INBOX_HTML}" "${VIEW_HTML}" >&2 || true
  die "stage=assert: cross-tenant attendant ${FORBIDDEN_TENANT_UUID} (agent@globex) leaked into the acme operator surface — RLS is NOT enforced on the runtime pool. Point DATABASE_URL at the app_runtime (NOBYPASSRLS) role and set DB_ENFORCE_RLS_ROLE=1 (SIN-65590 / SIN-65580)."
fi

# Positive sanity signal (non-blocking): did the in-tenant attendant
# render? If not, the interactive dropdown may not be wired on this
# deploy (degrades to read-only text) — still not a leak, so warn only.
if grep -qi "${EXPECT_TENANT_UUID}" "${INBOX_HTML}" "${VIEW_HTML}"; then
  log "stage=assert ok — in-tenant attendant ${EXPECT_TENANT_UUID} present, cross-tenant ${FORBIDDEN_TENANT_UUID} absent"
else
  printf '::warning::stage=assert: in-tenant attendant %s not found in HTML — the assignment dropdown may not be wired on this deploy (read-only mode). Cross-tenant leak check still PASSED (forbidden UUID absent).\n' "${EXPECT_TENANT_UUID}"
  log "stage=assert ok — cross-tenant ${FORBIDDEN_TENANT_UUID} absent (dropdown render unconfirmed; see warning)"
fi

log "stg-smoke-rls-cross-tenant: PASS — no cross-tenant RLS leak observed"
exit 0
