-- 0091_audit_log_security_authz_allow.down.sql
-- Revert: restore the migration-0083 CHECK that excludes 'authz_allow'.
-- This will fail if any audit_log_security row has event_type =
-- 'authz_allow' — that is intentional, audit ledgers do not delete
-- history. Operators MUST archive/relocate those rows before reverting.

BEGIN;

ALTER TABLE audit_log_security
  DROP CONSTRAINT IF EXISTS audit_log_security_event_type_check;

ALTER TABLE audit_log_security
  ADD CONSTRAINT audit_log_security_event_type_check
  CHECK (event_type IN (
    'login',
    'login_fail',
    '2fa_enroll',
    '2fa_verify',
    'role_change',
    'impersonation_start',
    'impersonation_stop',
    'master_grant',
    'authz_deny',
    'signature_fail',
    'key_rotation'
  ));

COMMIT;
