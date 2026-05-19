-- 0105_tenant_custom_domain_failure.up.sql
-- SIN-63080 Fase 5 — custom-domain DNS-poller worker.
--
-- Adds the persistent "failed" terminal state for tenant_custom_domains so
-- the verifier worker (cmd/customdomain-verifier) can give up after N
-- unsuccessful TXT-challenge attempts without thrashing DNS forever. The
-- worker tracks attempt counts in-memory and only persists when it hits
-- the cap; the row then drops out of ListPendingVerification.
--
-- Schema delta:
--   - failed_at      TIMESTAMPTZ NULLABLE  — wall-clock the worker marked
--                                             the row as failed.
--   - failure_reason TEXT NULLABLE         — short controlled-vocabulary
--                                             string (e.g. "cap_exceeded",
--                                             "token_mismatch_cap").
--
-- New partial index `idx_tenant_custom_domains_pending_verification` keeps
-- the worker's ListPendingVerification scan cheap even when the table
-- grows: the index only covers rows that are eligible for verification.
--
-- Rollback: drop the index, drop the columns. Existing rows lose the
-- failure metadata but no app behaviour regresses (the worker treats a
-- nullable failed_at as "not failed").

BEGIN;

ALTER TABLE tenant_custom_domains
    ADD COLUMN IF NOT EXISTS failed_at      TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS failure_reason TEXT;

CREATE INDEX IF NOT EXISTS idx_tenant_custom_domains_pending_verification
    ON tenant_custom_domains(created_at ASC)
    WHERE deleted_at IS NULL
      AND verified_at IS NULL
      AND failed_at IS NULL
      AND tls_paused_at IS NULL;

COMMIT;
