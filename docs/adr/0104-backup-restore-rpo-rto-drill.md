# ADR 0104 — Backup/Restore: RPO 5min / RTO 60min, encrypted age vault, quarterly restore drill

- Status: Accepted
- Date: 2026-05-21
- Deciders: CTO, SecurityEngineer
- Drives: [SIN-63188](/SIN/issues/SIN-63188) (this ADR — Fase 6 PR6 doc gate)
- Ratifies: shipped in [SIN-63187](/SIN/issues/SIN-63187) (Fase 6 PR5 — restore drill script + runbook + quarterly cadence, merged in PR #226, commit `1c805db`), [SIN-62267](/SIN/issues/SIN-62267) (`backup.sh` exit-code chain + S3 head-object verify + structured journald), [SIN-62261](/SIN/issues/SIN-62261) (encrypted backup target + age key custody — shipped earlier in Phase 2)
- Builds on: [ADR 0084](./0084-supply-chain.md) §systemd-hardening (NoNewPrivileges + ProtectSystem on the backup unit), [ADR 0092](./0092-ci-postgres-credentials.md) (CI Postgres baseline that the drill restores against)
- Related: [SIN-62260](/SIN/issues/SIN-62260) (sandbox systemd smoke test, blocked behind Fase 6 backup re-land)
- Lenses: **Reversibility & blast radius**, **Defense in depth**, **Operational excellence**

> **Numbering note.** The original Fase 6 PR6 task ([SIN-63188](/SIN/issues/SIN-63188))
> referenced `ADR-0072`. That slot was already taken by [ADR 0072](./0072-rls-policies.md)
> (Fase 0 RLS policies). This ADR takes the next free index above
> [ADR 0103](./0103-lgpd-export-delete-retention.md).

## Context

Backup correctness is the strongest guarantee the platform makes:
every other safety mechanism (RLS, audit-log split, tombstone-on-delete)
is upstream of a single irreplaceable artifact — the daily Postgres
+ MinIO snapshot. A backup that "ran every night" but has never been
restored is a placebo; the [Phase-0 security review](/SIN/issues/SIN-62220#document-security-review)
F18 finding flagged this as a HIGH-severity gap.

The threat model has three independent components:

- **Data-at-rest leak.** The backup target lives on S3-compatible
  object storage operated by a third party. Compromise of that
  vendor must NOT expose unencrypted PII. → encrypted-at-rest
  with a key held in master custody.
- **Restoration capability.** A backup we have never restored is
  unknown to be good. → automated drill on a quarterly cadence,
  artifact-checksummed, alert-on-fail.
- **Recovery time objective.** The platform serves WhatsApp inbox
  traffic; an hour-long outage during business hours is a real
  customer harm. → RTO 60 min for catastrophic recovery, RPO 5 min
  for the most recent transaction loss.

Phase 2 ([SIN-62261](/SIN/issues/SIN-62261)) shipped the encrypted
backup pipeline (`backup.sh` → age encryption → S3 PUT). Phase 6
([SIN-63187](/SIN/issues/SIN-63187)) closed the verification gap
with the restore drill. This ADR records the joint RPO/RTO contract,
the key custody model, and the drill cadence so the operator
on-call has one document to reference.

## Decision

### D1 — RPO 5 min / RTO 60 min, measured by the drill

- **RPO (Recovery Point Objective) = 5 minutes.** The Postgres
  base backup runs nightly; WAL is streamed continuously to S3
  via `walg push-wal`. The 5-min figure is the maximum
  transaction-loss window: a catastrophic crash at T loses, at
  worst, transactions from `T - 5min` to `T`.
- **RTO (Recovery Time Objective) = 60 minutes.** From the
  on-call paging at `T0` to "platform serves traffic again" at
  `T0 + 60min`. Measured end-to-end during the drill (D4):
  pull-from-S3 + decrypt + restore + reconcile + cutover.

Both numbers are SLOs, not SLAs. A miss is paged but does not
auto-rollback. The drill (D4) measures both per quarter; a
quarter where the drill exceeds either is a master-ops
post-mortem, not a customer-credit event.

### D2 — Encryption at rest: age with key in master custody

`backup.sh` (shipped in [SIN-62267](/SIN/issues/SIN-62267)) pipes:

```
pg_dump --format=custom → \
  age --encrypt --recipient $(cat /etc/agnu/backup-recipients) → \
  aws s3 cp - s3://$VAULT_BUCKET/$STAMP/pg.dump.age
```

Three invariants:

- **age over PGP.** age (Filippo Valsorda) has the better
  cryptographic story (Curve25519, no parser surface), the
  smaller binary (no GPG ring), and is statically linkable into
  the backup unit. PGP rejected as legacy.
- **Recipient file, not in-memory key.** The recipients file
  lives in `/etc/agnu/backup-recipients` with mode `0640
  root:agnu-backup` so the backup unit (running as
  `agnu-backup`, ADR 0084 §systemd-hardening) can read but not
  rewrite it. Rotation of the recipient (D3) is a
  master-ops-only writable path.
- **Master holds the private key.** The decryption key is held
  in master custody (offline, hardware-token-backed where
  available) and is NEVER on the production hosts. A compromised
  backup unit can WRITE encrypted blobs but cannot READ any
  blob, current or historical.

### D3 — Key custody & rotation: two recipients, monthly rotation

The recipients file declares **two** age recipients at any time:
the current key and the previous key. Rotation cadence is
**monthly**, driven by [ADR 0106](./0106-secrets-rotation-runbook.md):

1. Master generates new key on hardware token.
2. Public key appended to `backup-recipients` (now two entries).
3. `backup.sh` next run encrypts to **both** recipients.
4. After 7 days (one weekly drill cycle), oldest recipient
   removed from the file.
5. Master archives the retired private key offline; never destroyed
   (regulatory retention).

A drill (D4) can therefore decrypt any blob in the last 30 days
with the current key; older blobs may require the archive.

### D4 — Quarterly drill: automated, artifact-checksummed, alert-on-fail

`scripts/restore-drill.sh` (shipped in [SIN-63187](/SIN/issues/SIN-63187))
executes the full restore on isolated infrastructure and writes a
verification artifact to S3:

1. **Provision a sandbox host** (cloud-init from `cloud-init/restore-drill.yaml`).
2. **Pull the newest Postgres + MinIO base backups** from the
   vault.
3. **Decrypt** with the current age recipient (`age --decrypt -i …`).
4. **Restore** via `pg_restore --clean --create --jobs=4`. Time-it.
5. **Pull the WAL chain** since the base backup and
   `pg_archivecleanup`-replay.
6. **Run the integrity battery**: row counts on the audit ledger,
   FK validation, RLS-enabled assertion, master_ops trigger
   existence.
7. **Cutover smoke**: bring up the app pointed at the restored
   DB; HTMX GET `/health` MUST return 200 with the right commit
   SHA.
8. **Emit a `restore-drill-{quarter}.json` artifact** to
   `s3://$VAULT_BUCKET/drills/`. The JSON carries: timings (RTO
   measurement), checksums of base + WAL, integrity-battery
   results, sandbox host id.
9. **Tear down** the sandbox host.

**Cadence.** Quarterly (4× per year) under cron. A drill that
exceeds RTO or fails any integrity-battery assertion pages
SecurityEngineer; the next quarter is a re-run plus a written
post-mortem on the ADR thread (this file).

**Storage cost.** Drill artifacts retain for 24 months
(matching audit_log_security floor) so a regulator query for
"prove your restore was tested in Q3" lands a single S3 GET.

### D5 — MinIO sidecar: separate backup, same vault, same drill

WhatsApp media (images, audio, video) and master_secret-encrypted
artifacts (LGPD export bundles per [ADR 0103](./0103-lgpd-export-delete-retention.md))
live in MinIO. The Phase-6 backup pipeline backs the MinIO
buckets separately:

```
mc mirror --overwrite minio/$BUCKET → \
  age --encrypt … → \
  aws s3 cp -
```

The drill (D4) restores both Postgres AND MinIO and verifies a
**cross-table consistency** invariant: every
`message_attachments.sha256` referenced by a non-tombstoned
message row MUST resolve in the restored MinIO bucket.

### D6 — Operational ownership

- **`agnu-backup` systemd user** owns the backup unit. NoNewPrivileges,
  ProtectSystem=strict, ProtectHome=true (ADR 0084).
- **SecurityEngineer** owns the drill quarterly cadence and is
  paged on every drill failure.
- **CTO** holds the age private-key custody and signs the
  recipient-rotation log.

## Consequences

**Positive.**

- F18 closed. Backup correctness is now a measured invariant on
  a quarterly cadence; the drill artifact is the durable record.
- RPO/RTO are stated, drilled, and the gap from "stated" to
  "actually achieved on a real cutover" is the artifact's
  delta — visible, paged, post-mortemed.
- Two-recipient rotation gives an always-online operational
  current key plus a graceful rollback to the prior key without
  a re-encryption sweep.
- The MinIO sidecar restore prevents the failure mode "Postgres
  came back but every WhatsApp image is a broken link", which
  is a customer-visible regression.

**Negative / costs.**

- Quarterly drill consumes a sandbox host for the duration (≈ 60
  min on the current dataset; will grow with data volume). The
  sandbox is the same vendor-tier as production — cheap on a
  quarterly cadence, would matter at daily cadence.
- Two-recipient encryption doubles the per-blob CPU. Measured
  at < 8% of the backup wall-clock — acceptable.
- The master must coordinate key generation and the
  `backup-recipients` rollout monthly. Mitigation: the rotation
  runbook in [ADR 0106](./0106-secrets-rotation-runbook.md)
  fixes the steps so the rotation is a 15-min checklist, not a
  bespoke incident.

**Residual risks (accepted).**

- A drill is one cutover; it does not exercise every restore
  path (point-in-time recovery to an arbitrary minute is on the
  documented escalation path but not drilled per quarter).
  Accepted — the WAL replay path is exercised on every drill
  (step 5), so the mechanics are warm.
- The drill runs on a sandbox host; a production-cluster
  restore hits its own infrastructure quirks. The first
  *real* recovery will discover them. Mitigation: the drill
  artifact format is identical to the production cutover
  log, so the post-mortem template applies unchanged.

## Alternatives considered

- **PITR-only (no daily base + WAL).** Rejected — base + WAL
  is the model the operators understand and the drill exercises;
  PITR-only doubles the operator-training cost without changing
  the RPO.
- **Cloud-managed Postgres backup (e.g. RDS snapshots).**
  Rejected because the platform is self-hosted on commodity
  cloud VMs; RDS would shift the architecture, and the
  cloud-vendor's snapshot is in-region — region-loss is the
  single failure mode the off-region S3 vault is built to
  survive.
- **WORM bucket lifecycle for drill artifacts.** Considered.
  Deferred — the master-ops audit log (`master_ops_audit_trigger`)
  already records artifact writes, and WORM has been
  surprisingly fragile across cloud vendors in incident reports.
  Revisit if a regulator audit asks for it.
- **Daily drill instead of quarterly.** Rejected as the floor;
  the cost (sandbox host × 4× saved) is not worth the marginal
  detection latency, given that the backup unit's exit-code
  chain ([SIN-62267](/SIN/issues/SIN-62267)) catches the
  high-probability "didn't run" failures in real time.

## What this ADR does **not** decide

- The age-key rotation **procedure** — covered by
  [ADR 0106](./0106-secrets-rotation-runbook.md).
- The Postgres backup encryption-in-transit (TLS to the S3
  endpoint) — that is in the [SIN-62267](/SIN/issues/SIN-62267)
  systemd unit and is not configurable per ADR 0084.
- The MinIO bucket policy — orthogonal, owned by the storage
  domain.
- The drill **scheduling** (cron expression) — operational
  config, lives in the master-ops repo, not this ADR.
- Point-in-time recovery operator runbook — referenced from
  [SIN-63187](/SIN/issues/SIN-63187)'s shipped runbook, not in
  scope here.
