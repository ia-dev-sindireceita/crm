#!/usr/bin/env bash
# scripts/ci/stg-smoke-inbox.sh — SIN-63825 / SIN-63793 W6 staging
# smoke for the operator inbox loop. Exercises:
#
#   1. /health pre-condition: .inbox_channel_provider == "llmcustomer"
#      (proves cmd/server resolved INBOX_CHANNEL_PROVIDER correctly,
#      so a stale env on the VPS cannot false-pass the rest).
#   2. Login as the seeded tenant_atendente. The same secret pair the
#      SIN-63270 /login smoke uses is re-used so the deploy gate is
#      one credential surface, not two.
#   3. GET /inbox → at least one /inbox/conversations/<uuid> link.
#      Distinguishes "W1 router not mounted" (404) from "W2 bootstrap
#      did not seed" (200 + empty list).
#   4. GET /inbox/conversations/<uuid> → extract the CSRF meta token
#      and baseline-count `class="message-bubble msg-in"` bubbles.
#   5. POST /inbox/conversations/<uuid>/messages → 200, then poll the
#      same conversation-view route every POLL_INTERVAL_SECONDS for up
#      to POLL_TIMEOUT_SECONDS. Pass when msg-in count > baseline.
#
# Failure modes are surfaced with greppable `stage=` labels so the
# cd-stg.yml job log can be triaged at a glance:
#
#   stage=preflight   — /health unreachable or provider mismatch
#   stage=auth        — /login != 302 or session cookies missing
#   stage=route       — /inbox != 200 (W1 router not deployed)
#   stage=bootstrap   — /inbox 200 with no conversation link (W2)
#   stage=view        — /inbox/conversations/<id> != 200 or no CSRF
#   stage=send        — POST messages != 200
#   stage=dispatch    — no LLM inbound observed within timeout
#
# Required env (passed from cd-stg.yml):
#   STG_BASE             — base URL, e.g. https://acme.crm.crm.someu.com.br
#   STG_SEED_AGENT_EMAIL — seeded tenant_atendente email
#   STG_SEED_AGENT_PASSWORD
#
# Optional env (defaults match SIN-63824 llmcustomerReplyDelay budget):
#   POLL_TIMEOUT_SECONDS  — default 30
#   POLL_INTERVAL_SECONDS — default 2
#   REPLY_BODY            — outbound text; default "ping from staging smoke"
#
# The script is idempotent: the llmcustomer bootstrap reuses the same
# synthetic conversation across runs (per-tenant ledger + idempotent
# Adapter.Bootstrap, see cmd/server/inbox_wire_llmcustomer.go), and
# the POST just appends one outbound + one inbound bubble per run.
# No state cleanup is needed.

set -euo pipefail
# Belt-and-braces: cd-stg.yml runs `set +x` before invoking us so the
# password never echoes; mirror it here in case a future caller forgets.
set +x

: "${STG_BASE:?stage=preflight: STG_BASE is required}"
: "${STG_SEED_AGENT_EMAIL:?stage=preflight: STG_SEED_AGENT_EMAIL is required}"
: "${STG_SEED_AGENT_PASSWORD:?stage=preflight: STG_SEED_AGENT_PASSWORD is required}"

POLL_TIMEOUT_SECONDS="${POLL_TIMEOUT_SECONDS:-30}"
POLL_INTERVAL_SECONDS="${POLL_INTERVAL_SECONDS:-2}"
REPLY_BODY="${REPLY_BODY:-ping from staging smoke}"

WORKDIR="$(mktemp -d -t stg-smoke-inbox.XXXXXX)"
trap 'rm -rf "${WORKDIR}"' EXIT
JAR="${WORKDIR}/cookies.txt"
HEALTH="${WORKDIR}/health.json"
INBOX_HTML="${WORKDIR}/inbox.html"
INBOX_HDR="${WORKDIR}/inbox.headers"
LOGIN_HDR="${WORKDIR}/login.headers"
LOGIN_BODY="${WORKDIR}/login.body"
VIEW_HTML="${WORKDIR}/view.html"
VIEW_HDR="${WORKDIR}/view.headers"
SEND_BODY="${WORKDIR}/send.body"
SEND_HDR="${WORKDIR}/send.headers"

log() { printf '[stg-smoke-inbox] %s\n' "$*"; }
die() { printf '::error::%s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------
# Stage 1 — /health pre-condition
# ---------------------------------------------------------------------
log "stage=preflight probing ${STG_BASE}/health"
if ! curl -fsS --max-time 5 "${STG_BASE}/health" -o "${HEALTH}"; then
  die "stage=preflight: GET /health failed or returned non-2xx (is the VPS reachable?)"
fi
provider=$(jq -r '.inbox_channel_provider // ""' < "${HEALTH}")
if [ "${provider}" != "llmcustomer" ]; then
  cat "${HEALTH}" >&2
  die "stage=preflight: /health reports inbox_channel_provider=\"${provider}\", want \"llmcustomer\" (INBOX_CHANNEL_PROVIDER unset or misconfigured on staging)"
fi
log "stage=preflight ok — provider=llmcustomer"

# ---------------------------------------------------------------------
# Stage 2 — Login. Reuse the SIN-63270 cookie contract.
# ---------------------------------------------------------------------
log "stage=auth POST ${STG_BASE}/login as ${STG_SEED_AGENT_EMAIL%@*}@…"
code=$(curl -sS --max-time 5 -o "${LOGIN_BODY}" -D "${LOGIN_HDR}" \
  -w "%{http_code}" -X POST \
  -c "${JAR}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  --data-urlencode "email=${STG_SEED_AGENT_EMAIL}" \
  --data-urlencode "password=${STG_SEED_AGENT_PASSWORD}" \
  "${STG_BASE}/login")
if [ "${code}" != "302" ]; then
  cat "${LOGIN_BODY}" >&2
  die "stage=auth: /login expected 302, got ${code} (check seeded credentials and tenant FQDN)"
fi
if ! grep -q "Set-Cookie: __Host-sess-tenant" "${LOGIN_HDR}"; then
  cat "${LOGIN_HDR}" >&2
  die "stage=auth: missing __Host-sess-tenant cookie (MFA gate may have intercepted — seed user must be tenant_atendente without totp_required_at)"
fi
if ! grep -q "Set-Cookie: __Host-csrf" "${LOGIN_HDR}"; then
  cat "${LOGIN_HDR}" >&2
  die "stage=auth: missing __Host-csrf cookie"
fi
log "stage=auth ok"

# ---------------------------------------------------------------------
# Stage 3 — GET /inbox. Pull the first conversation link out of the
# server-rendered HTML and extract the CSRF meta token for the POST.
# ---------------------------------------------------------------------
log "stage=route GET ${STG_BASE}/inbox"
code=$(curl -sS --max-time 5 -o "${INBOX_HTML}" -D "${INBOX_HDR}" \
  -w "%{http_code}" -b "${JAR}" -c "${JAR}" \
  "${STG_BASE}/inbox")
if [ "${code}" = "404" ]; then
  die "stage=route: /inbox 404 — W1 inbox router not mounted on this deploy"
fi
if [ "${code}" = "403" ]; then
  die "stage=route: /inbox 403 — seeded user lacks RoleTenantAtendente / RoleTenantGerente (seed staging with tenant_atendente)"
fi
if [ "${code}" != "200" ]; then
  cat "${INBOX_HDR}" >&2
  die "stage=route: /inbox expected 200, got ${code}"
fi

# The list template renders each conversation as
#   <a … href="/inbox/conversations/<uuid>" hx-get="/inbox/conversations/<uuid>">…
# Match the first href to keep the smoke robust against future
# className changes.
conversation_id=$(grep -oE 'href="/inbox/conversations/[0-9a-fA-F-]+"' "${INBOX_HTML}" \
  | head -n1 | sed -E 's#.*/inbox/conversations/([0-9a-fA-F-]+)".*#\1#' || true)
if [ -z "${conversation_id}" ]; then
  # The empty-state template renders `<li class="conversation-list__empty">`.
  if grep -q 'conversation-list__empty' "${INBOX_HTML}"; then
    die "stage=bootstrap: /inbox 200 with empty conversation list — W2 llmcustomer bootstrap did not seed a synthetic conversation for this tenant (check inbox_wire_llmcustomer.go bootstrap-on-list decorator and INBOX_CHANNEL_PROVIDER on staging)"
  fi
  die "stage=bootstrap: /inbox 200 but no /inbox/conversations/<uuid> link found in HTML (template drift?)"
fi
log "stage=route ok — conversation_id=${conversation_id}"

# ---------------------------------------------------------------------
# Stage 4 — GET conversation view. Extract CSRF + baseline inbound
# bubble count.
# ---------------------------------------------------------------------
fetch_view() {
  local out="$1"
  local code
  code=$(curl -sS --max-time 5 -o "${out}" -D "${VIEW_HDR}" \
    -w "%{http_code}" -b "${JAR}" -c "${JAR}" \
    "${STG_BASE}/inbox/conversations/${conversation_id}")
  printf '%s' "${code}"
}

log "stage=view GET ${STG_BASE}/inbox/conversations/${conversation_id}"
code=$(fetch_view "${VIEW_HTML}")
if [ "${code}" != "200" ]; then
  cat "${VIEW_HDR}" >&2
  die "stage=view: conversation view expected 200, got ${code}"
fi

# The CSRF token is rendered as a hidden form input in the conversation
# view partial (`<input type="hidden" name="_csrf" value="…">`). The
# layout's <meta name="csrf-token" content="…"> only ships on the
# full-page first-render — the HTMX partial does not include it, so
# the hidden-input parse is the contract.
csrf=$(grep -oE '<input type="hidden" name="_csrf" value="[^"]*"' "${VIEW_HTML}" \
  | head -n1 | sed -E 's#.*value="([^"]*)".*#\1#')
if [ -z "${csrf}" ]; then
  # Fall back to the layout's <meta> for cold-page renders where the
  # right pane shipped the full shell.
  csrf=$(grep -oE '<meta name="csrf-token" content="[^"]*"' "${VIEW_HTML}" \
    | head -n1 | sed -E 's#.*content="([^"]*)".*#\1#')
fi
if [ -z "${csrf}" ]; then
  die "stage=view: could not extract CSRF token from conversation view HTML"
fi

count_inbound() {
  local file="$1"
  # message-bubble template renders `class="message-bubble msg-in"`
  # for inbound (Direction != "out") and `msg-out` for outbound.
  grep -c 'class="message-bubble msg-in"' "${file}" || true
}

baseline=$(count_inbound "${VIEW_HTML}")
log "stage=view ok — csrf=<redacted>, inbound_baseline=${baseline}"

# ---------------------------------------------------------------------
# Stage 5 — POST a reply.
# ---------------------------------------------------------------------
log "stage=send POST ${STG_BASE}/inbox/conversations/${conversation_id}/messages"
code=$(curl -sS --max-time 5 -o "${SEND_BODY}" -D "${SEND_HDR}" \
  -w "%{http_code}" -X POST \
  -b "${JAR}" -c "${JAR}" \
  -H "Content-Type: application/x-www-form-urlencoded" \
  -H "X-CSRF-Token: ${csrf}" \
  --data-urlencode "body=${REPLY_BODY}" \
  --data-urlencode "_csrf=${csrf}" \
  "${STG_BASE}/inbox/conversations/${conversation_id}/messages")
if [ "${code}" != "200" ]; then
  cat "${SEND_HDR}" >&2
  cat "${SEND_BODY}" >&2
  die "stage=send: POST messages expected 200, got ${code}"
fi
log "stage=send ok"

# ---------------------------------------------------------------------
# Stage 6 — Poll for the LLM-driven inbound.
# ---------------------------------------------------------------------
log "stage=dispatch polling for inbound (timeout=${POLL_TIMEOUT_SECONDS}s, interval=${POLL_INTERVAL_SECONDS}s)"
deadline=$(( $(date +%s) + POLL_TIMEOUT_SECONDS ))
attempt=0
while [ "$(date +%s)" -lt "${deadline}" ]; do
  attempt=$(( attempt + 1 ))
  sleep "${POLL_INTERVAL_SECONDS}"
  if ! code=$(fetch_view "${VIEW_HTML}"); then
    log "stage=dispatch attempt ${attempt}: view fetch failed transiently — retrying"
    continue
  fi
  if [ "${code}" != "200" ]; then
    log "stage=dispatch attempt ${attempt}: view returned ${code} — retrying"
    continue
  fi
  current=$(count_inbound "${VIEW_HTML}")
  log "stage=dispatch attempt ${attempt}: inbound=${current} (baseline=${baseline})"
  if [ "${current}" -gt "${baseline}" ]; then
    log "stage=dispatch ok — LLM inbound observed (delta=$(( current - baseline )))"
    exit 0
  fi
done

# Polling exhausted. Surface the last view payload to make root-cause
# easier (e.g. an InboundMessagePublisher error renders nothing).
cat "${VIEW_HTML}" >&2
die "stage=dispatch: no LLM inbound observed within ${POLL_TIMEOUT_SECONDS}s — W2 receiver / W5 selector wireup may be broken (last inbound count=${baseline}, no growth)"
