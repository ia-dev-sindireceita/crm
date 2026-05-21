-- 0107_consent_record.down.sql
-- Reverse 0107_consent_record.up.sql.
--
-- Down step is documented as a developer-environment rollback path,
-- NOT a production reverse. Dropping consent_record loses every
-- LGPD grant/revoke row; the audit_log_data CHECK rollback rejects
-- future consent_grant/consent_revoke inserts. Production rollback
-- should be a forward migration that disables writes to the
-- consent_record adapter, not this down step.
--
-- Run as app_admin. Idempotent: DROP TABLE IF EXISTS.

BEGIN;

DROP TRIGGER IF EXISTS consent_record_master_ops_audit ON consent_record;

DROP TABLE IF EXISTS consent_record;

ALTER TABLE audit_log_data DROP CONSTRAINT IF EXISTS audit_log_data_event_type_check;
ALTER TABLE audit_log_data
  ADD CONSTRAINT audit_log_data_event_type_check
  CHECK (event_type IN (
    'read_pii',
    'write_contact',
    'export_csv',
    'lgpd_export',
    'lgpd_forget'
  ));

COMMIT;
