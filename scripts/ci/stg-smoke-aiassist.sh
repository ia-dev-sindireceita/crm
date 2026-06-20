#!/usr/bin/env bash
# scripts/ci/stg-smoke-aiassist.sh — SIN-65244 staging smoke for the
# operator AI-assist feature ("Resumir + sugerir 3 respostas").
#
# The AI-assist Summarizer is wired ONLY inside the llmcustomer inbox
# branch and ONLY when OPENROUTER_API_KEY is set on the VPS (soft
# activation gate). The button + POST /inbox/conversations/{id}/ai-assist
# route mount together with the Summarizer, so the rendered conversation
# view is the source of truth for "is the feature live on this deploy".
#
# Modes:
#
#   FULL (button present + enabled): exercises the real LLM path once.
#     1. /health pre-condition: .inbox_channel_provider == "llmcustomer".
#     2. Login as the seeded tenant_atendente.
#     3. GET /inbox → first conversation link.
#     4. GET conversation view → detect the ai-assist button + CSRF.
#     5. POST /inbox/conversations/<id>/ai-assist (Origin+Referer+CSRF).
#        PASS on a summary panel (real LLM round-trip) OR on a graceful
#        tenant-config banner (policy off / no balance / consent needed /
#        transient) — those are tenant state, not deploy regressions.
#
#   DEGRADED (skip, exit 0): the feature is not live on this deploy.
#     Triggered when provider != llmcustomer, OR the conversation view
#     renders no ai-assist button (OPENROUTER_API_KEY unset), OR the
#     button is present-but-disabled (policy off). The smoke logs a
#     clear skip and exits 0 so the deploy gate is not false-blocked.
#
# Greppable stage labels for cd-stg triage:
#   stage=preflight  — /health unreachable or provider not llmcustomer
#   stage=auth       — /login != 302 or session cookies missing
#   stage=route      — /inbox or /ai-assist 404 (wireup gap)
#   stage=view       — conversation view != 200 or no CSRF
#   stage=assist     — /ai-assist 5xx (LLM/wireup fault)
#
# Required env (from cd-stg.yml):
#   STG_BASE              — base URL, e.g. https://acme.crm.crm.someu.com.br
#   STG_SEED_AGENT_EMAIL  — seeded tenant_atendente email
#   STG_SEED_AGENT_PASSWORD

set -euo pipefail
# cd-stg.yml runs `set +x` before invoking us so the password / key
# never echo; mirror it here in case a future caller forgets.
set +x

: "${STG_BASE:?stage=preflight: STG_BASE is required}"
: "${STG_SEED_AGENT_EMAIL:?stage=preflight: STG_SEED_AGENT_EMAIL is required}"
: "${STG_SEED_AGENT_PASSWORD:?stage=preflight: STG_SEED_AGENT_PASSWORD is required}"

WORKDIR="$(mktemp -d -t stg-smoke-aiassist.XXXXXX)"
trap 'rm -rf "${WORKDIR}"' EXIT
JAR="${WORKDIR}/cookies.txt"
HEALTH="${WORKDIR}/health.json"
INBOX_HTML="${WORKDIR}/inbox.html"
LOGIN_HDR="${WORKDIR}/login.headers"
LOGIN_BODY="${WORKDIR}/login.body"
VIEW_HTML="${WORKDIR}/view.html"
VIEW_HDR="${WORKDIR}/view.headers"
ASSIST_BODY="${WORKDIR}/assist.body"
ASSIST_HDR="${WORKDIR}/assist.headers"

log() { printf '[stg-smoke-aiassist] %s\n' "$*"; }
die() { printf '::error::%s\n' "$*" >&2; exit 1; }
skip() { log "$*"; log "stg-smoke-aiassist: SKIP (feature not live on this deploy)"; exit 0; }

# ---------------------------------------------------------------------
# Stage 1 — /health pre-condition.
# ---------------------------------------------------------------------
log "stage=preflight probing ${STG_BASE}/health"
if ! curl -fsS --max-time 5 "${STG_BASE}/health" -o "${HEALTH}"; then
  die "stage=preflight: GET /health failed or returned non-2xx (is the VPS reachable?)"
fi
provider=$(jq -r '.inbox_channel_provider // ""' < "${HEALTH}")
if [ "${provider}" != "llmcustomer" ]; then
  skip "stage=preflight: inbox_channel_provider=\"${provider:-unset}\" (AI-assist is wired only in the llmcustomer branch; set INBOX_CHANNEL_PROVIDER=llmcustomer + OPENROUTER_API_KEY on the VPS to enable)"
fi
log "stage=preflight ok — provider=llmcustomer"

# ---------------------------------------------------------------------
# Stage 2 — Login (reuse the SIN-63270 cookie contract).
# ---------------------------------------------------------------------
log "stage=auth POST ${STG_BASE}/login as ${STG_SEED_AGENT_EMAIL%@*}@…"
code=$(curl -sS --max-time 5 -o "${LOGIN_BODY}" -D "${LOGIN_HDR}" \
  -w "%{http_code}" -X POST -c "${JAR}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "email=${STG_SEED_AGENT_EMAIL}" \
  --data-urlencode "password=${STG_SEED_AGENT_PASSWORD}" \
  "${STG_BASE}/login")
if [ "${code}" != "302" ]; then
  cat "${LOGIN_BODY}" >&2
  die "stage=auth: /login expected 302, got ${code} (check seeded credentials and tenant FQDN)"
fi
# Staging is HTTP/2 → curl prints headers lowercase; grep -i or the
# match false-fails on a successful login (SIN-63858).
if ! grep -qi "Set-Cookie: __Host-sess-tenant" "${LOGIN_HDR}"; then
  cat "${LOGIN_HDR}" >&2
  die "stage=auth: missing __Host-sess-tenant cookie (MFA gate may have intercepted — seed user must be tenant_atendente without totp_required_at)"
fi
log "stage=auth ok"

# ---------------------------------------------------------------------
# Stage 3 — GET /inbox → first conversation link.
# ---------------------------------------------------------------------
log "stage=route GET ${STG_BASE}/inbox"
code=$(curl -sS --max-time 5 -o "${INBOX_HTML}" -w "%{http_code}" \
  -b "${JAR}" -c "${JAR}" "${STG_BASE}/inbox")
if [ "${code}" = "404" ]; then
  die "stage=route: /inbox 404 — inbox router not mounted on this deploy"
fi
if [ "${code}" != "200" ]; then
  die "stage=route: /inbox expected 200, got ${code}"
fi
conversation_id=$(grep -oE 'href="/inbox/conversations/[0-9a-fA-F-]+' "${INBOX_HTML}" \
  | head -n1 | sed -E 's#.*/inbox/conversations/([0-9a-fA-F-]+).*#\1#' || true)
if [ -z "${conversation_id}" ]; then
  skip "stage=route: /inbox 200 but no conversation seeded yet — nothing to summarise (llmcustomer bootstrap lands on the operator's first visit; re-run after the inbox smoke)"
fi
log "stage=route ok — conversation_id=${conversation_id}"

# ---------------------------------------------------------------------
# Stage 4 — Conversation view → detect the ai-assist button + CSRF.
# ---------------------------------------------------------------------
log "stage=view GET ${STG_BASE}/inbox/conversations/${conversation_id}"
code=$(curl -sS --max-time 5 -o "${VIEW_HTML}" -D "${VIEW_HDR}" -w "%{http_code}" \
  -b "${JAR}" -c "${JAR}" "${STG_BASE}/inbox/conversations/${conversation_id}")
if [ "${code}" != "200" ]; then
  cat "${VIEW_HDR}" >&2
  die "stage=view: conversation view expected 200, got ${code}"
fi

if ! grep -q 'id="ai-assist-button"' "${VIEW_HTML}"; then
  skip "stage=view: no ai-assist button rendered — OPENROUTER_API_KEY unset, Summarizer nil (soft-degrade)"
fi
# Present-but-disabled means the feature is wired but IA is off for this
# channel's policy. That is a tenant-config state, not a deploy
# regression, so skip clean.
if grep -q 'ai-assist__button--disabled' "${VIEW_HTML}"; then
  skip "stage=view: ai-assist button present but disabled — IA policy is off for this channel (wired, tenant-config gated)"
fi

csrf=$(grep -oE '<input type="hidden" name="_csrf" value="[^"]*"' "${VIEW_HTML}" \
  | head -n1 | sed -E 's#.*value="([^"]*)".*#\1#')
if [ -z "${csrf}" ]; then
  csrf=$(grep -oE '<meta name="csrf-token" content="[^"]*"' "${VIEW_HTML}" \
    | head -n1 | sed -E 's#.*content="([^"]*)".*#\1#')
fi
if [ -z "${csrf}" ]; then
  die "stage=view: ai-assist button enabled but no CSRF token in conversation view HTML"
fi
log "stage=view ok — ai-assist button enabled, csrf=<redacted>"

# ---------------------------------------------------------------------
# Stage 5 — POST /ai-assist. Origin+Referer satisfy the ADR-0073 CSRF
# allowlist (curl omits them by default → reason=csrf.origin_missing).
# ---------------------------------------------------------------------
log "stage=assist POST ${STG_BASE}/inbox/conversations/${conversation_id}/ai-assist"
code=$(curl -sS --max-time 30 -o "${ASSIST_BODY}" -D "${ASSIST_HDR}" \
  -w "%{http_code}" -X POST -b "${JAR}" -c "${JAR}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -H "X-CSRF-Token: ${csrf}" \
  -H "Origin: ${STG_BASE}" \
  -H "Referer: ${STG_BASE}/inbox/conversations/${conversation_id}" \
  --data-urlencode "channelId=" \
  --data-urlencode "teamId=" \
  --data-urlencode "_csrf=${csrf}" \
  "${STG_BASE}/inbox/conversations/${conversation_id}/ai-assist")

# A graceful banner means the route + use case are wired correctly but a
# tenant-level precondition blocked the LLM call. That is expected on a
# freshly deployed/seeded tenant and must NOT fail the deploy gate. The
# precondition states do NOT all share one HTTP status: the policy-off
# path returns the banner with **403** by design (ai_assist.go
# WriteHeader(StatusForbidden), pinned by ai_assist_test.go), while
# balance/consent/rate/unavailable degrade on 200. So scan the body for
# a known graceful marker on BOTH 200 and 403 before deciding — a 403
# is only a real CSRF/Origin/role rejection when it carries NO marker.
graceful_pass() { # $1 = observed http status (for the log line)
  for marker in 'ai-assist__banner--policy' 'ai-assist__banner--balance' 'ai-consent-modal' 'ai-assist__banner--unavailable' 'ai-assist__toast--rate'; do
    if grep -q "${marker}" "${ASSIST_BODY}"; then
      log "stage=assist ok — wired; graceful degradation (${marker}) on HTTP ${1} — tenant-config/precondition state, not a deploy regression"
      log "stg-smoke-aiassist: PASS (wired; precondition banner ${marker})"
      exit 0
    fi
  done
}

case "${code}" in
  404)
    die "stage=route: /ai-assist 404 — button rendered but route not mounted (wireup gap: Summarizer reached the template but not Routes())" ;;
  5*)
    cat "${ASSIST_BODY}" >&2
    die "stage=assist: /ai-assist ${code} — LLM/wireup fault (check OPENROUTER_API_KEY validity + adapter logs)" ;;
  403)
    # Policy-off is the deny-by-default state (no ai_policy row → AIEnabled
    # false): the handler returns the policy banner with 403. Accept that
    # as a graceful degrade; only a 403 WITHOUT a marker is a true
    # CSRF/Origin/role rejection.
    graceful_pass "${code}"
    cat "${ASSIST_HDR}" "${ASSIST_BODY}" >&2
    die "stage=assist: /ai-assist 403 with no graceful banner — CSRF/authz rejected (Origin/Referer or role gate)" ;;
  200)
    if grep -q 'ai-assist__result' "${ASSIST_BODY}"; then
      log "stage=assist ok — summary panel rendered (real LLM round-trip exercised)"
      log "stg-smoke-aiassist: PASS (full — LLM path exercised)"
      exit 0
    fi
    graceful_pass "${code}"
    cat "${ASSIST_BODY}" >&2
    die "stage=assist: /ai-assist 200 but response is neither a summary panel nor a known graceful banner (template drift?)" ;;
  *)
    cat "${ASSIST_BODY}" >&2
    die "stage=assist: /ai-assist unexpected status ${code}" ;;
esac
