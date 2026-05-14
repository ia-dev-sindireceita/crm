# Runbook — Logs & Loki retention

Owning task: [SIN-62253](/SIN/issues/SIN-62253) (ADR 0004 decisão 3).
Compose overlay: `deploy/compose/compose.loki.yml`.
Loki config: `deploy/loki/loki-config.yaml`.
Promtail config: `deploy/loki/promtail-config.yaml`.

## Retention policy (LGPD scope)

- **Horizon: 30 days.** All logs older than 30d are deleted by the Loki
  compactor. There is no archive tier — once a chunk is past the horizon
  it is gone.
- **Why 30d.** ADR 0004 decisão 3 aligns Loki retention with
  `pg_dump` retention so the LGPD scope of "operational logs" stops at the
  same horizon as backups. Anything longer-lived must go to the audit log
  (separate table, separate retention, RLS-scoped per tenant).

### What is enforced where

| Knob                                         | File                            | Value |
| -------------------------------------------- | ------------------------------- | ----- |
| `limits_config.retention_period`             | `deploy/loki/loki-config.yaml`  | `720h` (30d) |
| `compactor.retention_enabled`                | `deploy/loki/loki-config.yaml`  | `true` |
| `compactor.compaction_interval`              | `deploy/loki/loki-config.yaml`  | `10m` |
| `compactor.retention_delete_delay`           | `deploy/loki/loki-config.yaml`  | `2h` |
| `limits_config.reject_old_samples_max_age`   | `deploy/loki/loki-config.yaml`  | `168h` (7d) |

Without `compactor.retention_enabled: true`, `retention_period` is
advisory only — Loki keeps the chunks forever. Verify after every config
change.

## Verify retention is active

```bash
# Check the running Loki advertises the retention we expect.
curl -sS http://loki:3100/config \
  | grep -E 'retention_period|retention_enabled' \
  | head

# Expected output (excerpt):
#   retention_enabled: true
#   retention_period: 720h
```

`/config` is exposed unauthenticated inside the compose network. It is
NOT published to the host or through Caddy — keep it that way.

## Smoke test

After `docker compose -f compose.yml -f compose.obs.yml -f compose.loki.yml up -d`:

1. **Loki readiness.**

   ```bash
   curl -sS http://loki:3100/ready
   # → ready
   ```

2. **Generate a log line** by hitting the app.

   ```bash
   curl -sS http://app:8080/healthz >/dev/null
   ```

3. **Query Loki** for the line via the JSON API.

   ```bash
   curl -sS --get http://loki:3100/loki/api/v1/query_range \
     --data-urlencode 'query={service="app"}' \
     --data-urlencode 'limit=5' \
     | jq '.data.result[0].values[0]'
   ```

4. **In Grafana** (port-forward `3000`), open Explore, pick the
   `Loki` data source, and run `{service="app"}` — the same lines
   appear with parsed JSON fields (`tenant_id`, `request_id`,
   `user_id`).

5. **Confirm retention** with the `/config` check above.

If any step fails, stop and page the on-call. Do not "just restart
Loki" — restarts mask compactor stalls.

## Incident export (must run before day 25 of an open incident)

The compactor purges chunks past 30d. If an incident is still under
investigation and the originating logs are older than ~25 days, those
logs WILL be deleted before the incident closes. Export manually.

### Alert wiring

A Prometheus alert (`LokiActiveIncidentApproachingRetention`) fires when
both of these are true:

- An incident is marked open in the incident tracker (label
  `incident_state="open"` on `incident_open_age_seconds`).
- `incident_open_age_seconds > 25 * 86400` (25 days).

Page severity: **ticket** (not page) — the export is a deliberate
manual action, not a paging emergency. The alert routes to `#ops` so
the on-call can run the export within a business day.

> The alert definition lives outside this PR — it is wired in a
> follow-up task once the incident tracker exposes the metric. Until
> then, the on-call MUST eyeball any open incident on day 25 and run
> the export manually.

### Manual export procedure

1. Identify the window. Replace `START` / `END` with ISO-8601 timestamps.

   ```bash
   START="2026-04-14T00:00:00Z"
   END="2026-04-15T00:00:00Z"
   QUERY='{service="app"}'
   ```

2. Export with `logcli` (run from a maintenance host that has network
   access to Loki on `:3100` — do NOT pipe export traffic through
   Caddy).

   ```bash
   logcli query \
     --addr=http://loki:3100 \
     --from="$START" \
     --to="$END" \
     --output=jsonl \
     --batch=5000 \
     "$QUERY" \
     > incident-${INCIDENT_ID}-${START%T*}.jsonl
   ```

3. Encrypt and ship to the incident archive bucket.

   ```bash
   age -r "$INCIDENT_ARCHIVE_PUBKEY" \
     -o "incident-${INCIDENT_ID}-${START%T*}.jsonl.age" \
     "incident-${INCIDENT_ID}-${START%T*}.jsonl"
   rclone copy \
     "incident-${INCIDENT_ID}-${START%T*}.jsonl.age" \
     b2:crm-incidents/
   rm "incident-${INCIDENT_ID}-${START%T*}.jsonl"   # plaintext gone
   ```

4. Record the export in the incident ticket (path in B2 + sha256).

## Rollback

The overlay is purely additive. To remove Loki without affecting the
core stack:

```bash
docker compose \
  -f deploy/compose/compose.yml \
  -f deploy/compose/compose.obs.yml \
  -f deploy/compose/compose.loki.yml \
  stop loki promtail
docker compose \
  -f deploy/compose/compose.yml \
  -f deploy/compose/compose.obs.yml \
  -f deploy/compose/compose.loki.yml \
  rm -f loki promtail
docker volume rm crm_loki-data
```

The Grafana datasource provisioned in
`deploy/grafana/provisioning/datasources/loki.yml` is read-only — it
appears as "Unavailable" in Grafana once Loki is gone but does not
break dashboards (no panels use Loki by default).

## Related

- ADR 0004 ([SIN-62230#document-adr-logging-and-audit](/SIN/issues/SIN-62230#document-adr-logging-and-audit)).
- Audit log (separate retention horizon) — [SIN-62230](/SIN/issues/SIN-62230).
- pg_dump retention alignment — backup runbook in `docs/deploy/`.
