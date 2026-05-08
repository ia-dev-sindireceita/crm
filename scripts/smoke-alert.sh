#!/usr/bin/env bash
# SIN-62218 smoke-alert: synthetically trip the RLSMissDetected alert
# in stg and verify it reaches the Slack webhook (via the in-network
# sentinel receiver).
#
# Exit codes:
#   0  — alert delivered to webhook within $TIMEOUT seconds
#   1  — anything else (target down, alertmanager down, timeout)
#
# Required env:
#   APP_URL                       e.g. http://localhost:8080
#   ALERTMANAGER_URL              e.g. http://localhost:9093
#   SMOKE_ALERT_WEBHOOK_PROBE_URL  e.g. http://localhost:9094  — sentinel that records last payload
#
# Optional env:
#   TIMEOUT                       seconds, default 60

set -euo pipefail

log() { printf '[smoke-alert] %s\n' "$*" >&2; }

: "${APP_URL:?APP_URL is required, e.g. http://localhost:8080}"
: "${ALERTMANAGER_URL:?ALERTMANAGER_URL is required, e.g. http://localhost:9093}"
: "${SMOKE_ALERT_WEBHOOK_PROBE_URL:?SMOKE_ALERT_WEBHOOK_PROBE_URL is required (sentinel /last)}"
TIMEOUT="${TIMEOUT:-60}"

log "preflight: checking alertmanager health at ${ALERTMANAGER_URL}/-/healthy"
if ! curl --fail --silent --show-error --max-time 5 -o /dev/null "${ALERTMANAGER_URL}/-/healthy"; then
  log "alertmanager /-/healthy is unreachable; aborting"
  exit 1
fi

log "preflight: checking probe sentinel /healthz"
if ! curl --fail --silent --show-error --max-time 5 -o /dev/null "${SMOKE_ALERT_WEBHOOK_PROBE_URL}/healthz"; then
  log "probe sentinel /healthz is unreachable; aborting"
  exit 1
fi

# Snapshot last-event id BEFORE we trip — so we can prove a NEW one
# arrived rather than misreading a stale payload.
before_count=$(curl --fail --silent --show-error --max-time 5 "${SMOKE_ALERT_WEBHOOK_PROBE_URL}/count" || echo "0")
log "before count: ${before_count}"

log "tripping rls_misses_total via POST ${APP_URL}/internal/test-alert"
if ! curl --fail --silent --show-error --max-time 5 -X POST -o /dev/null "${APP_URL}/internal/test-alert"; then
  log "POST /internal/test-alert failed (is the binary built with -tags test?); aborting"
  exit 1
fi

deadline=$((SECONDS + TIMEOUT))
log "polling sentinel ${SMOKE_ALERT_WEBHOOK_PROBE_URL}/count every 2s, deadline ${TIMEOUT}s"
while (( SECONDS < deadline )); do
  current_count=$(curl --fail --silent --show-error --max-time 5 "${SMOKE_ALERT_WEBHOOK_PROBE_URL}/count" 2>/dev/null || echo "${before_count}")
  if [[ "${current_count}" != "${before_count}" ]]; then
    log "alert delivered (count: ${before_count} -> ${current_count})"
    exit 0
  fi
  sleep 2
done

log "timed out after ${TIMEOUT}s — alert never reached the sentinel"
exit 1
