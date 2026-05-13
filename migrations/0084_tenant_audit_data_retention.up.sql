-- 0084_tenant_audit_data_retention.up.sql (re-landed from legacy 0013 per ADR 0086)
-- SIN-62252: per-tenant override for audit_log_data retention.
--
-- Default 12 months matches the LGPD baseline. Tenants on stricter
-- contracts (legal, financial) can extend up to 60 months by editing
-- this column directly via app_master_ops; the LGPD purge job reads
-- this value when sweeping a tenant's audit_log_data rows.
--
-- audit_log_security retention is NOT per-tenant — it's a 24-month
-- floor enforced at the purge-job level, identical for every tenant.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS audit_data_retention_months int NOT NULL DEFAULT 12
    CHECK (audit_data_retention_months >= 1 AND audit_data_retention_months <= 60);

COMMIT;
