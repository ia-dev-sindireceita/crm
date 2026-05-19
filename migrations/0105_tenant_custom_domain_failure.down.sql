-- 0105_tenant_custom_domain_failure.down.sql
-- Rollback the failure-state columns + index added by the up migration.
-- Idempotent: drops only what 0105 introduced.

BEGIN;

DROP INDEX IF EXISTS idx_tenant_custom_domains_pending_verification;

ALTER TABLE tenant_custom_domains
    DROP COLUMN IF EXISTS failed_at,
    DROP COLUMN IF EXISTS failure_reason;

COMMIT;
