-- 0117_audit_log_security_correlation_id.up.sql
-- SIN-63958 / master-impersonation-spec §3.1.
--
-- Adds the correlation_id column to audit_log_security so every
-- authz_allow / authz_deny / data-access row fired *during* an active
-- impersonation envelope can be linked to the master_impersonation_session
-- row that opened it. The middleware ImpersonationFromSession populates
-- the request context with the active row id; the audit writer falls
-- back to that context value when the event itself does not carry one.
--
-- correlation_id is NULL for the overwhelming majority of rows (every
-- audit event fired outside an impersonation envelope). A partial index
-- keeps the per-correlation feed query (Feed SSE handler, spec §1.4
-- third endpoint) fast without bloating the rest of the table's plan
-- cache.
--
-- The FK uses ON DELETE SET NULL so an aggressive future GC of
-- master_impersonation_session never breaks the audit row — the audit
-- trail is the source of truth for non-repudiation and outlives the
-- transient envelope.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE audit_log_security
  ADD COLUMN IF NOT EXISTS correlation_id uuid
    REFERENCES master_impersonation_session(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS audit_log_security_correlation_id_idx
  ON audit_log_security (correlation_id, occurred_at ASC)
  WHERE correlation_id IS NOT NULL;

COMMIT;
