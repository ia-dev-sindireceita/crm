# ADR 0089 — Message retention policy: tenant defaults, hard-delete vs anonymisation, LGPD individual right-to-erasure

- Status: accepted
- Date: 2026-05-14
- Deciders: CTO
- Tickets: [SIN-62723](/SIN/issues/SIN-62723) (this ADR), [SIN-62193](/SIN/issues/SIN-62193) (Fase 1 parent),
  [SIN-62253](/SIN/issues/SIN-62253) (Loki/Promtail retention 30d — log axis, distinct from message data),
  [SIN-62224](/SIN/issues/SIN-62224) (ADR 0075 — `raw_event` 30d partition retention).

## Context

Fase 1 ([SIN-62193](/SIN/issues/SIN-62193)) ships the first persistent
record of customer-side conversation data: `Message`, `Conversation`, and
`Contact` rows. These rows are PII under LGPD:

- `Contact.identifier` is a phone number (WhatsApp). PII.
- `Message.body` contains free-form text from the end customer. PII;
  may contain CPF, address, financial data depending on what the customer
  wrote.
- `Conversation` metadata (timestamps, assigned user) is operational
  metadata about a data subject. LGPD treats it as PII in context.

Two retention axes are already decided **and out of scope here**:

- **Application logs** ([SIN-62253](/SIN/issues/SIN-62253)). Loki/Promtail
  carries 30d retention with the redacting `slog` handler in front. The
  ADR 0004 §3 overlay already exists.
- **`raw_event` storage** (ADR 0075 §3). Webhook envelopes are stored 30d
  via daily partition + `DROP PARTITION`. Distinct from message bodies
  because `raw_event` is forensic artifact (transport-layer), not the
  inbox domain record.

What is **not yet decided** — and what this ADR closes — is the retention
of the **domain-side** message data: how long `Message`, `Conversation`,
and `Contact` rows live, what happens when retention expires, and how
LGPD's individual right to erasure interacts with the default policy.

Legal context (LGPD, Lei 13.709/2018) requires:

- A documented retention period proportional to the purpose of processing
  (Art. 16). "Forever, because we might want it" is non-compliant.
- A mechanism for the data subject to request erasure (Art. 18 VI). The
  controller has 15 days to respond.
- The data must be deleted, anonymised, or blocked when the purpose is
  fulfilled (Art. 16) — anonymisation is a valid alternative to deletion
  *if* the anonymisation is irreversible.

The CEO and the legal counterpart have not yet picked a stance on
"deletion vs anonymisation by default." This ADR proposes a stance — hard
delete by default, anonymise on operator opt-in — and documents the
rationale so legal review can land on this artifact and either accept it
or amend it explicitly.

## Decision

### D1 — Default retention windows (per tenant, mutable by master)

| Aggregate              | Default retention | Stored as PII?               |
| ---------------------- | ----------------- | ---------------------------- |
| `Message.body`         | 18 months         | Yes (free-form text)         |
| `Message` metadata     | 36 months         | Yes (operational metadata)   |
| `Conversation`         | 36 months         | Yes (operational metadata)   |
| `Contact.identifier`   | 36 months         | Yes (phone / channel id)     |
| `Contact` metadata     | 36 months         | Yes                          |
| `raw_event`            | 30 days           | Yes (envelope) — ADR 0075    |
| `inbound_message_dedup`| 18 months         | No (dedup index, no payload) |
| `webhook_idempotency`  | 30 days           | No (dedup hash)              |

Retention is **per tenant**, configurable by the tenant's master via the
master/admin UI (UI is a follow-up issue; this ADR fixes the schema and
default). The tenant may **lower** the retention below default; raising
it above default requires CTO approval (default is the *security maximum*
unless explicitly justified).

Why 18 months on `Message.body` and 36 months on metadata:

- 18 months covers a full operational year plus a 6-month dispute /
  customer-service review window. Operationally enough for inbox
  history search.
- 36 months on metadata supports analytical "how did this account behave
  over time" without holding the actual content of every message that
  long. Splitting the two reduces exposure on the larger PII surface
  (free-form text) by half-life-ing it faster.
- Both windows are below typical commercial "10 years" defaults seen in
  legacy CRMs. LGPD Art. 16 requires proportionality; we err toward
  shorter. Shorter retention compounds well with the smaller blast
  radius of any future breach.

### D2 — Hard-delete by default; anonymisation as a tenant opt-in

When a row crosses its retention horizon, the **default action is
hard-delete** (`DELETE FROM message WHERE created_at < now() - INTERVAL
'18 months'`). The row is gone from Postgres; backups beyond 30 days
expire by the backup retention policy (separate ADR pending — referenced
in §"Out of scope").

**Anonymisation** (replacing PII fields with non-PII placeholders while
keeping the row's `id` and analytical fields) is available as a per-tenant
opt-in:

```sql
-- On anonymise (instead of DELETE):
UPDATE message
   SET body        = '',
       body_format = 'redacted',
       redacted_at = now()
 WHERE id = $1;

UPDATE contact
   SET identifier  = 'redacted:' || encode(sha256(identifier::bytea), 'hex'),
       display_name = 'redacted',
       redacted_at = now()
 WHERE id = $1;
```

Anonymisation is **irreversible** by design. The `redacted_at` column is
how the system distinguishes "this row was anonymised at expiry" from "this
row is fresh and happens to have an empty body."

Why hard-delete is the default:

- Anonymisation that keeps `id` and `created_at` plus a hashed identifier
  is *pseudonymous*, not anonymous in the LGPD-Art-12 sense. A motivated
  adversary with auxiliary data (e.g., the phone number list) can
  re-identify rows. Hard-delete removes that risk.
- Storage cost of anonymised rows is non-zero. The dataset of a 5-year-old
  active tenant would grow indefinitely under "anonymise everything."
- Hard-delete simplifies the LGPD-erasure path (D3) — the same code path
  serves both routine expiry and on-demand erasure.

Why anonymisation is offered as an opt-in:

- Some tenants want long-tail analytics (cohort retention, channel
  performance) that need `id`-level continuity beyond message-body
  retention. Anonymising preserves the analytical schema without keeping
  the content.
- The opt-in must be explicit (per tenant, recorded in the `tenants`
  table with `retention_mode ∈ {'hard_delete', 'anonymise'}`). Default is
  `hard_delete`. Changing to `anonymise` is logged as a master action
  with the master's id and timestamp.

### D3 — LGPD individual right to erasure

A data subject (the end customer) can request erasure of their data. The
flow:

1. Request lands via the tenant's customer-service path (out of scope:
   ADR does not mandate a specific intake channel — phone, form, email).
   The tenant operator triggers erasure via the master/admin UI.
2. The system performs a **hard-delete by identifier**, scoped to the
   requesting tenant:

   ```sql
   BEGIN;
   DELETE FROM message
    WHERE conversation_id IN (
      SELECT id FROM conversation
       WHERE contact_id IN (
         SELECT id FROM contact
          WHERE tenant_id = $tenant
            AND identifier = $identifier_or_lookup_hash
       )
    );
   DELETE FROM conversation
    WHERE contact_id IN (
      SELECT id FROM contact
       WHERE tenant_id = $tenant
         AND identifier = $identifier_or_lookup_hash
    );
   DELETE FROM contact
    WHERE tenant_id = $tenant
      AND identifier = $identifier_or_lookup_hash;
   INSERT INTO erasure_audit (tenant_id, identifier_hash, performed_at, performed_by_master_id)
        VALUES ($tenant, sha256($identifier)::bytea, now(), $master);
   COMMIT;
   ```

3. `raw_event` rows for the same `(tenant_id, channel)` and matching
   `channel_external_id`s are **also deleted** by the same TX. This
   crosses the transport/domain boundary intentionally for LGPD
   compliance — the right-to-erasure does not respect our architectural
   layering, and the audit trail in `erasure_audit` documents the
   override.

4. `inbound_message_dedup` rows survive (no PII; just the `wamid` index).
   This is intentional: if the data subject's contact reaches us again,
   we still recognise the carrier-side dedup id so we don't double-process
   a redelivery. The `message_id` foreign-key column becomes a stale
   reference, which is handled at consumer time by gracefully treating
   "dedup row exists but `message_id` not found" as "already processed."

5. The SLA from request to completion is **5 business days**, well below
   LGPD's 15-day mandate. The system measures and reports this via
   `erasure_audit` timestamps.

**Backups.** Backups taken before erasure still contain the data until the
backup itself expires (max 30 days for our recovery-point policy, to be
ratified in a separate ADR — see §"Out of scope"). The erasure response to
the data subject explicitly states this 30-day backup tail. LGPD Art.
16 §1 permits retention "for the regular exercise of rights in judicial,
administrative or arbitration proceedings" — the backup tail is a
proportional necessity for disaster recovery.

### D4 — Implementation surface

- `internal/retention` (new package) owns the retention domain logic. It
  defines a `Policy` aggregate (per-tenant retention windows + mode) and
  a `Sweeper` port. Lint-isolated from `database/sql` per hexagonal lens.
- `internal/adapter/db/postgres/retention_sweeper.go` implements the
  port. Runs daily at 03:00 America/Sao_Paulo (after the wallet
  reconciler from ADR 0088). Boring stdlib `time.Ticker`, `SELECT FOR
  UPDATE SKIP LOCKED` for safety against concurrent sweepers (only one
  is expected; the lock is defensive).
- LGPD-erasure endpoint:
  `POST /api/master/tenants/:tenantId/erasure { "contactIdentifier": "..." }`.
  Auth: master role only, audit-logged via `erasure_audit`. Idempotent
  by `(tenant_id, identifier_hash)` — re-running has no effect once the
  contact is gone.
- Metrics: `retention_swept_total{tenant_id, table, mode}`,
  `retention_swept_duration_seconds`, `lgpd_erasure_total{tenant_id}`,
  `lgpd_erasure_duration_seconds`. Per-tenant labels are LGPD-safe
  because tenant_id ≠ data subject.

### D5 — Relationship to Loki/Promtail (ADR 0004 §3)

ADR 0004 §3 (overlay landed in [SIN-62253](/SIN/issues/SIN-62253)) sets
**application log** retention at 30 days. That covers the
`structuredLog` and `audit` log streams in Loki — those are observability
records about request handling, not the message bodies themselves.

The redacting `slog` handler from [SIN-62255](/SIN/issues/SIN-62255)
strips PII fields (`tenant_id` pre-HMAC, `webhook_token`, `raw_payload`,
`Authorization`, and field-level redactors) **before** log records hit
Loki. The 30-day log retention is therefore an additional safety net,
not the primary PII control.

This ADR's message-data retention (D1) is **independent** of log
retention. Logs talking about message processing exist for 30 days;
the message itself exists for 18 months (default). When a data subject
requests erasure, the message is deleted (D3) but logs from 17 months
ago that referenced the redacted-pre-Loki message stay in Loki until
their own 30-day window expires — those logs do not contain PII by
construction.

### D6 — Migration / opt-in / consent

Fase 1 has **no production tenants yet**. Schema for `tenants.retention_*`
columns and the `erasure_audit` table lands in the inbox migration batch
(child of [SIN-62193](/SIN/issues/SIN-62193)). On tenant creation, the
defaults from D1 are inserted automatically.

Existing tenants in staging are reset on schema land (`TRUNCATE` is
acceptable in staging; no LGPD impact because no real customer data).

Consent capture from end customers (Art. 7 LGPD) is out of scope for this
ADR — the tenant operator is responsible for obtaining customer consent
on their side, which is a contractual matter (Terms of Service) outside
the platform. The platform documents the retention policy so the tenant's
ToS can reference it.

## Consequences

Positive:

- LGPD Art. 16 and Art. 18 VI compliance is documented and implemented,
  not just asserted. Audit-ready trail in `erasure_audit`.
- Hard-delete-by-default minimises blast radius on any future breach: a
  compromised DB snapshot from 25 months ago has no message bodies from
  25 months ago (they expired at 18m).
- Storage growth is bounded by the retention window, not unbounded.
  Capacity planning for a 5-year-old tenant only needs to budget for 18
  months of message bodies plus 36 months of metadata.
- The same `Sweeper` code path serves routine expiry and on-demand
  erasure with mode flags — simpler than two parallel pipelines.

Negative / costs:

- Hard-delete loses analytical depth on message bodies beyond 18 months.
  Tenants who want long-tail content analytics must opt in to
  anonymisation **explicitly** and accept the pseudonymity caveat.
- Configurable per-tenant retention adds a column on `tenants` and a
  conditional in the sweeper. Boring code, but non-zero complexity.
- Daily sweep on a large tenant's `message` table is a non-trivial
  delete. We use partitioned `message` by month (decided in the inbox
  migration ADR — out of scope here) so the sweep can `DROP PARTITION`
  instead of `DELETE`. Anonymise mode still requires `UPDATE` per row
  and is slower.

Risk residual:

- **Pseudonymous anonymisation re-identification risk** (Art. 12 LGPD).
  Mitigated by hard-delete default + explicit opt-in for anonymise +
  hashing the identifier on anonymisation. Tenants opting in are
  informed.
- **Backup tail.** 30-day backup tail for restored data subjects. Stated
  to the data subject at erasure time; LGPD-permissible as Art. 16 §1
  proportional retention.
- **Tenant misconfiguration.** A tenant master sets retention to 0d
  by mistake — the sweep deletes everything. Mitigation: master UI
  warning at < 30 days, CTO approval gate at < 7 days (out of scope:
  UI ADR).

## Alternatives considered

### Option B — Anonymise by default, never hard-delete

Replace PII fields at expiry, keep all row ids forever.

Rejected because:

- Storage grows linearly with tenant age forever. LGPD Art. 16's
  proportionality test arguably fails (we keep more than we need).
- Pseudonymity invites re-identification attacks; hard-delete is the
  safer default.
- Most tenants do not need long-tail row continuity. Pushing the
  cheaper-for-them-and-us default (hard-delete) and offering anonymise
  as opt-in keeps the cheap path the common path.

### Option C — Hard-delete only, no anonymisation offered

Drop anonymisation from the policy entirely. Tenants who want long-tail
analytics need to ETL to their own warehouse before expiry.

Rejected because:

- Tenants we expect to onboard (e.g., a tax-services firm) want at
  least row-id continuity for cohort analysis without ETL infrastructure.
  Anonymise-as-opt-in is a low-cost concession; refusing it forces
  tenants to build their own pipeline or to leave for a CRM that
  supports retention longer.
- LGPD does not require us to refuse anonymisation — it allows both.

### Option D — One uniform retention period (e.g., 36 months for everything)

Skip the split between message body (18m) and metadata (36m). Treat all
PII identically.

Rejected because:

- Message body is the highest-PII surface (free-form text from end
  customers, may contain anything). Halving its retention vs metadata
  is the cheapest reduction in breach blast-radius we can buy.
- Lens **least privilege.** The retention window is the privilege; less
  is less.

### Option E — Loki log retention covers it

Argue that ADR 0004 §3 already handles retention via logs, so domain
retention is unnecessary.

Rejected because logs are not the source of truth for messages. The
inbox aggregate is. Logs about messages exist 30 days; messages
themselves exist 18 months. ADR 0004 §3 and this ADR cover **different
axes**.

## Lenses cited

- **Secure-by-default.** Hard-delete is the conservative default;
  anonymisation requires explicit opt-in.
- **Least privilege.** Retention windows are the minimum that supports
  routine operations (18m bodies, 36m metadata, 30d transport envelopes).
- **Reversibility & blast radius.** Shorter retention reduces what a
  breach can leak. Hard-delete is irreversible at the row level; backups
  expire on their own 30-day window.
- **Defense in depth.** Domain retention (this ADR) + log retention (ADR
  0004 §3) + Loki redaction (SIN-62255) compose into three independent
  reduction paths for PII surface area.
- **Boring technology budget.** Daily `time.Ticker` sweeper, `DROP
  PARTITION` for message bodies, vanilla `DELETE` for the rest.
  Postgres-only; no event sourcing, no soft-delete columns with TTL.
- **Hexagonal / ports & adapters.** `internal/retention` declares the
  `Sweeper` port; the Postgres adapter implements it.

## Rollback

If 18 months on `Message.body` turns out to be too aggressive (e.g.,
legal counsel asks for 24 months after a specific incident), the rollback
is a config change: bump the default in `internal/retention` constants
and amend this ADR. Already-deleted rows do not come back; the change
is forward-only. Per-tenant overrides let an affected tenant keep
historical data going forward without re-amending the ADR.

If hard-delete-by-default is challenged by legal as inadequate (e.g.,
requirement to keep an immutable audit trail for some regulated tenant
class), the rollback is to extend D2 with a third mode
(`retention_mode='preserve'`) that suspends sweeping. That is an ADR
amendment, not a code rewrite; the `mode` column already supports it.

## Out of scope

- **Backup retention policy.** A separate ADR (not yet written) sets
  backup retention at 30 days and documents disaster-recovery semantics.
  This ADR assumes that ADR exists and references it by 30-day tail.
- **Master/admin UI for retention configuration.** Owned by a UI follow-up
  issue. This ADR fixes schema and defaults; UI design is separate.
- **Customer-side consent capture (Art. 7 LGPD).** Tenant responsibility
  via their own ToS. Platform documents the retention policy so tenants
  can reference it.
- **`Conversation` archival / closed-state migration.** A future product
  decision (e.g., move closed conversations to cold storage at 6 months).
  This ADR only fixes deletion at 36 months; intermediate cold-storage
  tiers are separate work.
- **Regulated-tenant exceptions.** If we ever onboard a regulated vertical
  with mandatory longer retention (e.g., financial-sector 10-year audit
  rule), the `retention_mode='preserve'` extension referenced in
  Rollback handles it via per-tenant override, with an ADR amendment.
