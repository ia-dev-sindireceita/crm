# ADR 0103 — LGPD article-18 (acesso + eliminação): export bundle + tombstone delete + per-tenant retention

- Status: Accepted
- Date: 2026-05-21
- Deciders: CTO
- Drives: [SIN-63188](/SIN/issues/SIN-63188) (this ADR — Fase 6 PR6 doc gate)
- Ratifies: shipped/in-flight in [SIN-63185](/SIN/issues/SIN-63185) (Fase 6 PR2 — `ConsentRegistry` ledger, merged in PR #223, commit `6b1a8ba`), [SIN-63186](/SIN/issues/SIN-63186) (Fase 6 PR3 — `/admin/lgpd/export` + `/admin/lgpd/delete` + retention worker, PR #225 in review), [SIN-63191](/SIN/issues/SIN-63191) (Fase 6 PR4 — HTMX privacy & data-controls UI + DPO display)
- Builds on: [ADR 0083](./0083-app-runtime-vs-master-ops.md) (split-role grants), [ADR 0084](./0084-supply-chain.md) §retention (audit retention floor), [ADR 0090](./0090-rbac-matrix.md) (`defaultPIIActions` for the export operator),
  migration 0083 (audit_log_security/data split), migration 0084 (`tenants.audit_data_retention_months`)
- Related: [SIN-62354](/SIN/issues/SIN-62354) (DPA markdown bundle), [SIN-62355](/SIN/issues/SIN-62355) (staging FQDN — DPO endpoint reachability)
- Lenses: **LGPD compliance**, **Secure-by-default API**, **Reversibility & blast radius**

> **Numbering note.** The original Fase 6 PR6 task ([SIN-63188](/SIN/issues/SIN-63188))
> referenced `ADR-0071`. That slot was already taken by [ADR 0071](./0071-postgres-roles.md)
> (Fase 0 split-role baseline). This ADR takes the next free index above
> [ADR 0102](./0102-2fa-totp-recovery-codes.md).

## Context

Brazil's LGPD (Lei 13.709/2018) article 18 grants every "titular de
dados" (data subject — typically the end-customer whose messages and
contact PII land in a tenant's inbox) two rights this platform must
honour on a fixed timeline:

- **Acesso (§18 II)** — a portable copy of every record relating to
  the subject, in a machine-readable format, within **15 days** of
  the request.
- **Eliminação (§18 VI / §16)** — irrevocable deletion of the
  subject's personal data, except where another legal basis
  preserves it (contractual obligation, regulatory retention, fraud
  detection).

The Fase-0 baseline ([SIN-62220](/SIN/issues/SIN-62220) F22/F25) flagged
this as the largest non-shipped gap before Fase 6. The plan splits
the LGPD surface across three PRs:

- **PR2 / ConsentRegistry** ([SIN-63185](/SIN/issues/SIN-63185)) — a
  generic consent ledger covering `terms_of_service`, `privacy_policy`,
  marketing, analytics cookies. One row per (subject, purpose, version).
- **PR3 / Export+Delete** ([SIN-63186](/SIN/issues/SIN-63186)) — the
  `/admin/lgpd/{export,delete}` operator UI, the LGPD retention worker,
  and the tombstone schema.
- **PR4 / Privacy UI** ([SIN-63191](/SIN/issues/SIN-63191)) — the
  end-user-facing privacy controls, DPO display, and printable
  privacy-policy view.

This ADR records the three correlated decisions across that split
so the implementation PRs can land independently without each
re-inventing the contract.

## Decision

### D1 — Export bundle: per-subject ZIP, streamed, signed, idempotent

`POST /admin/lgpd/export` enqueues a job; `GET /admin/lgpd/export/{id}`
streams a ZIP. The bundle layout is fixed and machine-readable:

```
export-{subject_id}-{ulid}/
├── manifest.json           — request meta, generator version, sha256
├── contact.json            — subject row (denormalised tenant fields)
├── messages/*.json         — per-thread message history, paginated
├── consents.json           — ConsentRegistry rows for this subject
├── ai-consent.json         — ai_policy_consent rows (ADR 0042/0101)
├── attachments/<sha>.bin   — original attachment bytes by sha256
└── audit-summary.json      — counts only (audit logs are NOT exported)
```

Rationale for each invariant:

- **Per-subject scope.** The export is keyed to a single (`tenant_id`,
  `contact_id`) pair. Cross-tenant export would require master-ops
  authority (the export operator is tenant-scoped per ADR 0090
  `defaultPIIActions`).
- **Streaming ZIP, no buffer-to-disk.** The job writes the archive
  to S3-compatible storage directly (`io.Pipe` → ZIP writer → object
  PUT) so a 50k-message export does not blow the worker memory
  budget. Backpressure on the ZIP writer is the natural
  rate-limit.
- **Idempotency via partial-unique index.** `lgpd_export_request`
  has a `UNIQUE (tenant_id, subject_id) WHERE state IN ('queued',
  'running')` constraint — re-clicking "export" while a job is
  in-flight returns the existing request rather than enqueuing a
  duplicate. Completed exports are addressable by ULID, never by
  reuse.
- **Signed download URL.** The completed bundle is served via a
  signed URL valid for **15 minutes** from issue. The download
  attempt itself is audited (`event_type='lgpd_export'` in
  `audit_log_data`), not the issue.
- **Audit-summary, not audit-export.** Exporting the security audit
  log would leak operator names and IPs to the subject. We export
  *counts* (number of message reads, number of consent grants) so
  the subject can corroborate "this platform stored N messages
  about me" without learning who-read-what.

### D2 — Delete: tombstone + irreversible PII scrub, never row-DELETE

`POST /admin/lgpd/delete` runs in one transaction (migration 0107 /
0108 in PR3, subject to renumbering):

```
BEGIN;
-- 1. Tombstone the subject (idempotent under partial-unique index).
INSERT INTO lgpd_deletion_request (tenant_id, subject_id, requested_by, …)
  VALUES (…)
  ON CONFLICT DO NOTHING;
-- 2. Replace PII columns with deterministic redactions.
UPDATE contacts
   SET full_name      = 'REDACTED',
       phone_e164     = 'REDACTED',
       email          = NULL,
       extra_fields   = '{}'::jsonb,
       lgpd_deleted_at = now()
 WHERE id = $1 AND tenant_id = $2;
-- 3. Scrub messages: body → 'REDACTED', metadata → '{}'.
UPDATE messages SET body = 'REDACTED', metadata = '{}'::jsonb
 WHERE contact_id = $1 AND tenant_id = $2;
-- 4. Detach attachments (orphan-by-CAS, swept by GC).
UPDATE message_attachments SET contact_id = NULL
 WHERE contact_id = $1 AND tenant_id = $2;
-- 5. Append the audit row.
INSERT INTO audit_log_data (tenant_id, actor_user_id, event_type, target)
  VALUES (…, 'lgpd_forget', '{"subject_id": "…"}');
COMMIT;
```

Three invariants are non-negotiable:

- **No `DELETE` on the row.** We replace, never row-DELETE, because
  every other table (messages, attachments, audit) holds an FK to
  `contacts(id)`. Cascading deletes across an active tenant would
  break billing reconciliation and produce gaps in the audit trail.
  The tombstone is the source-of-truth that this subject is "gone";
  application reads filter on `lgpd_deleted_at IS NULL`.
- **Audit row in `audit_log_data`, not `audit_log_security`.**
  `lgpd_forget` is a data-event (the operator touched PII to scrub
  it). It lands in the data ledger, which is the same ledger the
  LGPD retention job sweeps — so a future request can demonstrate
  "this subject was deleted, and the audit of that deletion was
  itself purged on its own retention timeline".
- **Tombstone is irreversible.** There is no undo button. A wrong
  delete must be discussed with the master operator and replayed
  from backup; this is intentional friction (the master-ops audit
  log records the replay) because a "soft delete" with restore-on-
  demand defeats the LGPD §16 promise.

### D3 — Retention: per-tenant override on `tenants.audit_data_retention_months`

Default 12 months on `audit_log_data` (ADR 0084 §retention). A
tenant in a regulated vertical (e.g. CVM-supervised broker)
declares a longer floor via:

```
ALTER TABLE tenants
  ALTER COLUMN audit_data_retention_months SET DEFAULT 12;
```

then per-tenant overrides via the master-ops settings UI. The
**security** ledger floor is fixed at **24 months** and not
overridable (ADR 0084) — that holds even if a tenant cancels.

The **LGPD retention worker** (`cmd/lgpd-retention-worker`, shipped
in PR3) runs daily under cron and:

1. Reads `tenants.audit_data_retention_months` for each active tenant.
2. Computes `cutoff = now() - months`.
3. `DELETE FROM audit_log_data WHERE tenant_id = $1 AND occurred_at < $2 RETURNING count`
   (RLS allows the master-ops role to delete cross-tenant; the
   `master_ops_audit_trigger` records the sweep).
4. Emits a Prometheus histogram per tenant: `lgpd_retention_purged_rows{tenant_id}`.

The worker **does not** touch `audit_log_security` — that ledger
is regulated retention, swept on a separate (longer) cadence by
ADR 0085 §sweep.

### D4 — DPO contact: per-tenant, mandatory before first export

Migration 0108 (subject to renumbering) adds four nullable columns
to `tenants`:

```
ALTER TABLE tenants
  ADD COLUMN dpo_name      text,
  ADD COLUMN dpo_email     text,
  ADD COLUMN dpo_phone     text,
  ADD COLUMN privacy_policy_url text;
```

A tenant CANNOT submit `POST /admin/lgpd/export` until the DPO
columns are non-null. The HTMX wizard in PR4 nudges the operator
to fill them on first visit; the LGPD operator returns HTTP 422
`dpo_required` with the wizard fragment otherwise.

`dpo_email` MUST be reachable on every export bundle's `manifest.json`
so the data subject has a non-platform path to challenge the
contents (article 18 § VII).

### D5 — Rate limit: tenant-scoped, 10 req/min/tenant

Both `/admin/lgpd/export` and `/admin/lgpd/delete` carry the
`lgpd_admin` rate policy (ADR 0073 D4 sliding-window pattern):

- 10 requests per minute per tenant_id (not per-IP — admin
  workflows are often shared NAT).
- 100 requests per hour per tenant_id.
- No per-user cap (a tenant may have multiple LGPD operators
  filing in parallel during a data-subject sweep).

Exceeded budgets return 429 with `Retry-After`.

## Consequences

**Positive.**

- LGPD article 18 compliance is implementable in **one tenant
  config screen** (DPO fields) plus the two `/admin/lgpd/*` routes.
  The operator UX is "click subject → click Export → download
  ZIP" — no off-platform spreadsheet round-trip.
- The retention worker is one-binary, one-cron, one-table — easy
  to GC and easy to audit. The Prometheus histogram is the
  long-tail metric (any tenant outside the median is the audit
  team's first question).
- DPO contact is a hard prerequisite, not a polite nudge —
  data-subject challenges always have a non-platform path.

**Negative / costs.**

- Re-implementing the export every time the data model grows
  (new column on `contacts`, new feature with PII) is a manual
  drift surface. Mitigation: `defaultPIIActions` in ADR 0090 is
  the authoritative list; the export generator references that
  list directly so adding a field forces the export code to
  acknowledge it.
- Tombstone-instead-of-DELETE means storage cost does not
  shrink on delete (only PII is scrubbed). Acceptable — the
  storage cost of a redacted row is microscopic compared to
  attachments, which DO get CAS-orphaned and reclaimed.
- The LGPD retention worker is a privileged binary (master-ops
  role). Compromise of the worker is cross-tenant. Mitigation:
  the worker runs in a dedicated systemd unit with NoNewPrivileges
  + ProtectSystem (ADR 0084 §systemd) and emits its sweep counts
  to the master-ops audit trail.

**Residual risks (accepted).**

- A subject who is also an *operator* (rare — usually distinct
  populations) cannot self-trigger deletion of their own *operator*
  identity. They request via the master tenant's DPO. Accepted
  because operator identity is contractually retained (CLT/labour
  law records, billing).
- The 15-minute signed-URL window is server-side; a leaked URL
  inside the window leaks the bundle. Mitigation: the URL is
  fetched once (the worker invalidates the signature after the
  first GET), reducing the exposure to a single fetch.

## Alternatives considered

- **GDPR-only "right to be forgotten" semantics.** Rejected — LGPD
  §16 imposes Brazilian-specific exceptions (regulatory retention,
  contractual obligation) we must respect even on delete. The
  tombstone schema is the cleanest way to record "deleted **but**
  retained per exception" without leaking PII.
- **Per-subject S3 prefix with bucket-lifecycle.** Rejected — the
  bucket lifecycle is per-tenant (one bucket per tenant per
  region); rewriting it per-subject blows the IAM policy and the
  operational drift surface.
- **Async export with email-only delivery.** Considered. Rejected
  because email is the password-reset channel and we do not want
  a precedent of "PII bundle landed in your inbox; we hope no one
  else has it". The signed-URL pattern keeps the bundle on the
  master-secret-encrypted vault.

## What this ADR does **not** decide

- The **AI consent** semantics — covered by [ADR 0042](./0042-policy-cascade.md)
  + ai-policy-consent table (Fase 3 decisão #8). The generic
  `consent_record` table in [ADR 0101](./0101-consent-registry-generic-lgpd.md)
  is the wider ledger; both are exported on §18 II requests.
- The **DPA** (Data Processing Agreement) markdown bundle — covered
  by [SIN-62354](/SIN/issues/SIN-62354). The DPA is operator-signed
  on tenant onboarding and is OUT of the per-subject export.
- The **master-ops retention sweep cadence** — separate ADR 0085.
- The **encryption-at-rest key rotation** for the export bundle —
  covered by [ADR 0106](./0106-secrets-rotation-runbook.md).
