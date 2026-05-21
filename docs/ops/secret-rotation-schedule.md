# Secret rotation schedule

Owning task: [SIN-63189](/SIN/issues/SIN-63189) (Fase 6 PR7).
Runbook: [`docs/ops/secrets-rotation.md`](./secrets-rotation.md).
Scripts: [`scripts/rotate-secret.sh`](../../scripts/rotate-secret.sh),
[`scripts/update-config-and-redeploy.sh`](../../scripts/update-config-and-redeploy.sh).

This page is the **single source of truth** for "what gets rotated
next, and when". It is meant to be loaded into the CEO's calendar so
each rotation fires on time, with the right operator awake.

Paperclip agents do **not** auto-fire rotations — humans own the
trigger so the operator is present, the new value is observed on a
hardware-secured workstation, and the secret never lives in CI/CD.

## Cadence table

| Secret                                    | Cadence    | Next rotation | Owner | Procedure section |
| ----------------------------------------- | ---------- | ------------- | ----- | ----------------- |
| DB password — `app_runtime`               | 90 days    | 2026-08-21    | CTO   | [Section 1](./secrets-rotation.md#1-db-password--app_runtime)        |
| DB password — `app_admin`                 | 90 days    | 2026-08-21    | CTO   | [Section 2](./secrets-rotation.md#2-db-password--app_admin)          |
| DB password — `app_master_ops`            | 90 days    | 2026-08-21    | CTO   | [Section 3](./secrets-rotation.md#3-db-password--app_master_ops)     |
| OpenRouter API key                        | 90 days    | 2026-08-21    | CTO   | [Section 4](./secrets-rotation.md#4-openrouter-api-key)              |
| PSP API key (Pagar.me)                    | 90 days    | 2026-08-21    | CTO   | [Section 5](./secrets-rotation.md#5-psp-api-key-pagarme)             |
| Slack webhook URL — alerts                | 180 days   | 2026-11-21    | CTO   | [Section 6](./secrets-rotation.md#6-slack-webhook-url--alerts)       |
| Campaign marker signing key (HMAC)        | 180 days   | 2026-11-21    | CTO   | [Section 7](./secrets-rotation.md#7-campaign-marker-signing-key-hmac) |
| Backup encryption key (offline ceremony)  | 365 days   | 2027-05-21    | CEO   | [Section 8](./secrets-rotation.md#8-backup-encryption-key)           |

The "Next rotation" column is computed from the SIN-63189 landing date
(2026-05-21) plus the cadence. As each rotation lands, the operator
updates this table in the same PR that bumps `audit_log_security` for
the `completed` row, so the next date stays accurate.

## Cron suggestions (Pericles' calendar)

Paste each line into Google Calendar (Custom recurrence → "by day of
month" mode). The trigger fires the reminder on the operator's phone
and creates a Paperclip ticket assigned to the named owner.

```cron
# 90-day cadence (app_runtime, app_admin, app_master_ops, openrouter, pagarme).
# Fires at 14:00 BRT (17:00 UTC) on the 21st of Feb / May / Aug / Nov.
0 17 21 2,5,8,11 *

# 180-day cadence (slack-alerts, marker:campaigns).
# Fires at 14:00 BRT (17:00 UTC) on the 21st of May and Nov.
0 17 21 5,11 *

# 365-day cadence (backup encryption — CEO ceremony, NOT scripted).
# Fires at 09:00 BRT (12:00 UTC) on May 21st.
0 12 21 5 *
```

The 21st-of-the-mid-quarter-month was picked because:

- It dodges quarter-end deploy freezes.
- It dodges Brazilian month-end billing weeks (PSP key rotations would
  otherwise risk hitting end-of-month invoicing batches).
- It aligns with the restore-drill cadence (mid-quarter) so the CTO is
  already in maintenance-window mode that week.

## "Stuck rotation" alert

The runbook ledger query (last section of
[`docs/ops/secrets-rotation.md`](./secrets-rotation.md#audit-ledger-queries-for-the-on-call))
returns one row per rotation that emitted a `started` event > 24 h ago
without a `completed` or `failed` follow-up. A Prometheus exporter
query that lifts that result to a gauge is the recommended
implementation; alert thresholds:

| Stuck for | Severity | Page who? |
| --------- | -------- | --------- |
| > 24 h    | P2       | CTO       |
| > 72 h    | P1       | CEO + CTO |

Until the exporter lands, the CTO runs the SQL by hand at the end of
each rotation week.

## Reminder protocol

When the calendar reminder fires:

1. CEO opens the Paperclip ticket created by the reminder
   (`[SIN-XXXXX] ops: rotate <secret-name>`).
2. CEO assigns the ticket to the named owner (typically the CTO).
3. Owner follows the matching section of
   [`docs/ops/secrets-rotation.md`](./secrets-rotation.md), runs the
   script, validates, and closes the ticket as `done` with the audit
   row id in the comment.
4. Owner updates the **Next rotation** date in the cadence table above
   in the same PR that records the rotation in the audit ledger
   (typically a tiny doc-only PR if no code change was needed).

## What if a rotation slips?

A slipped cadence is **not** a paging condition by itself — the
existing value is still valid until the next rotation. But every slip
shortens the "blast radius" of an undetected key leak, so:

- 1-week slip — informational. CTO acknowledges in the rotation
  ticket and re-schedules.
- 2-week slip — open a Fase 6 follow-up ticket, document the slip
  reason in the ticket body.
- 4-week slip — escalate to CEO. Treat as a SEV-3 ops finding.
