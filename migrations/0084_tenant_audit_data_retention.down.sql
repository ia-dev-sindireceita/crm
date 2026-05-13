-- 0084_tenant_audit_data_retention.down.sql (re-landed from legacy 0013 per ADR 0086)
-- Reverse of 0084_tenant_audit_data_retention.up.sql.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS audit_data_retention_months;

COMMIT;
