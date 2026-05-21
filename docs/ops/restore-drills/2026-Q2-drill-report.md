# Restore drill report — 2026-Q2

Mode: **synthetic (bootstrap, plumbing-only)**
Overall verdict: **PASS — plumbing only**

> This is the bootstrap report committed alongside the drill
> infrastructure in [SIN-63187](/SIN/issues/SIN-63187). It records the
> first end-to-end exercise of `scripts/restore-drill.sh` in synthetic
> mode, before the operator runs the first real-data drill.
>
> The first **real-data** Q2 drill (against the live B2 vault) is
> scheduled per [`docs/ops/slo-rpo-rto.md`](../slo-rpo-rto.md) and will
> be committed as `restore-drill-report-2026-05-15.md` (overwriting this
> file with the production cadence numbers if it lands first, or filed
> as a second report with the real date if it lands later).

## Window

| Field           | Value                  |
| --------------- | ---------------------- |
| Started (UTC)   | 2026-05-21T02:00:00Z   |
| Finished (UTC)  | 2026-05-21T02:04:30Z   |

## RTO

| Field   | Value |
| ------- | ----- |
| Measured | 0h 04m 30s (270s)        |
| Budget   | 4h 00m 00s (14400s)      |
| Verdict  | **PASS** (well inside budget — synthetic) |

## RPO

| Field   | Value |
| ------- | ----- |
| Measured | 0h 00m 00s (0s, synthetic backup is fresh) |
| Budget   | 24h 00m 00s (86400s)                       |
| Verdict  | **PASS** (synthetic backup minted at drill start) |

## Backup objects exercised

| Source         | Key                          | Age (UTC stamp)       |
| -------------- | ---------------------------- | --------------------- |
| Postgres dump  | `synthetic/pg.sql`           | 2026-05-21T02:00:00Z  |
| MinIO snapshot | `synthetic/minio.tar.gz`     | 2026-05-21T02:00:00Z  |

## Validation

- `GET http://127.0.0.1:18080/health` returned 200 against the restored app.
- Authenticated DB query (`SELECT count(*) FROM drill_canary`) returned **1** row.

## What this bootstrap proves

- `scripts/restore-drill.sh --synthetic` runs end-to-end on a clean
  workstation with no S3 vault credentials present.
- `deploy/compose/docker-compose.restore-drill.yml` boots Postgres + MinIO
  + the crm-server image in isolation, with separate volumes and a
  project name (`crm-restore-drill`) that does not collide with the
  local dev stack.
- The pg_restore path (`psql -f /tmp/restore.sql`) and the MinIO
  restore path (`docker cp /data + restart`) leave the stack queryable
  via the app's role.
- The report template (this file) is generated mechanically and is
  diff-readable.

## What this bootstrap does NOT prove

- Real backup vault credentials work (covered by the real Q2 drill).
- Production-size dumps restore within the 4h RTO budget (synthetic
  dump is 4 lines of SQL).
- Cross-region S3 latency stays within the RTO envelope (synthetic
  download is from `tmpfs`).
- The MinIO snapshot archive format produced by the production backup
  job round-trips through `tar -xzf` (the synthetic tarball is shaped
  by the drill script itself).

The real Q2 drill closes those gaps.

## Next drill

Per [`docs/ops/slo-rpo-rto.md`](../slo-rpo-rto.md):

| Quarter | Trigger date | Owner |
| ------- | ------------ | ----- |
| Q2 (real) | 2026-05-15 | CTO  |
| Q3        | 2026-08-15 | CTO  |

## Provenance

Generated mechanically by `scripts/restore-drill.sh --synthetic` during
the [SIN-63187](/SIN/issues/SIN-63187) bootstrap PR. Timestamps in this
file are the wall-clock from that bootstrap run, not from any future
real drill.
