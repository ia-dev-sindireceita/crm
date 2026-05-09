-- 0013_tenant_audit_data_retention.down.sql
-- Reverse of 0013_tenant_audit_data_retention.up.sql.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  DROP COLUMN IF EXISTS audit_data_retention_months;

COMMIT;
