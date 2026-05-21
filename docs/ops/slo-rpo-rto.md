# SLOs — RPO, RTO, and the quarterly restore drill cadence

Owning task: [SIN-63187](/SIN/issues/SIN-63187) (Fase 6 PR5).
Related runbook: [`docs/ops/restore-drill-runbook.md`](./restore-drill-runbook.md).
Drill reports archive: [`docs/ops/restore-drills/`](./restore-drills/).

This page documents the recovery SLOs for the CRM as a system, the
quarterly cadence used to prove them, and the cron suggestion the CEO
loads into the calendar to fire the drill on time.

## SLOs

| SLO | Budget | What it means | How we measure it |
| --- | ------ | ------------- | ----------------- |
| **RPO** — Recovery Point Objective | ≤ **24h** | At most 24 hours of customer data may be lost in a worst-case full-vault restore. | `now − LastModified` of the newest pg_dump in the backup vault, sampled at drill time. |
| **RTO** — Recovery Time Objective  | ≤ **4h**  | Once an operator starts the documented restore, the CRM must be back to a verified working state in under 4 hours.        | Wall-clock from `restore-drill.sh` start to `GET /health` returning 200 against the restored app, plus authenticated DB query OK.   |

The 24h / 4h numbers are the regulatory floor we committed to during
Fase 6 planning (LGPD scope + tenant SLA). Tightening them is a
product/CEO decision; loosening them requires a re-issued ADR.

## Drill cadence — quarterly

We exercise the full restore pipeline **four times a year**. Each drill
produces a dated report under `docs/ops/restore-drills/` and is committed
to `main` via a PR that closes a single Paperclip ticket scoped to that
quarter. The CTO triages any RTO/RPO breach found by the drill into the
next Fase 6 follow-up wave.

| Quarter | Trigger date  | Window               | Owner          |
| ------- | ------------- | -------------------- | -------------- |
| Q1      | 2026-02-15    | Mon–Fri, business hr | CTO            |
| Q2      | 2026-05-15    | Mon–Fri, business hr | CTO            |
| Q3      | 2026-08-15    | Mon–Fri, business hr | CTO            |
| Q4      | 2026-11-15    | Mon–Fri, business hr | CTO            |

The 15th-of-the-mid-quarter-month was picked because:

- It dodges quarter-end deploy freezes.
- It dodges Brazilian month-end billing weeks.
- It leaves at least 6 weeks of runway before the next regulatory
  reporting deadline if the drill finds a gap.

## Cron suggestion (Pericles' calendar)

Paperclip agents **do not auto-fire** this drill — humans own the
trigger so the right operator is present and the result can be
explained to a regulator. The CEO loads the following cron expression
into the personal calendar (Google Calendar → repeat: custom → "by
day of month" mode):

```
# At 09:00 BRT (12:00 UTC) on the 15th of February, May, August, November.
0 12 15 2,5,8,11 *
```

The reminder fires the day-of and creates a Paperclip ticket
`[SIN-XXXXX] ops: <YYYY>-Q<n> restore drill` assigned to the CTO. The
CTO then dispatches the drill per the runbook and files the report.

Why not a Paperclip cron routine: the drill consumes real S3 read
budget and burns ~30 minutes of operator attention every quarter. Both
are tracked spends; both deserve a human "go" before the work happens.
The CI smoke (synthetic mode) runs as a `workflow_dispatch` gate so we
can prove the script still parses in between drills — see
`.github/workflows/restore-drill.yml`.

## Reading the historical record

Reports under `docs/ops/restore-drills/` follow the template:
- Mode (synthetic vs real)
- Overall verdict
- Started / finished UTC stamps
- RTO measured vs budget
- RPO measured vs budget
- Backup keys exercised (with vault LastModified stamps)
- Validation summary

Reports are append-only. If a drill is rerun in the same quarter
because the first run failed, file a second report with a `-rerun`
suffix and link both from the closing PR description. Never edit a
historical report to make it look retroactively green — auditors look
at the raw history.

## Related

- Runbook: [`docs/ops/restore-drill-runbook.md`](./restore-drill-runbook.md)
- Backup write-side (separate repo): [SIN-62261](/SIN/issues/SIN-62261),
  [SIN-62267](/SIN/issues/SIN-62267)
- Restore playbook for agnu-api: [SIN-62626](/SIN/issues/SIN-62626)
