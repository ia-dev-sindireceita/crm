-- 0100_audit_log_security_billing_events.down.sql
-- Inverse of 0100: restore the 0091 CHECK clause that excludes the
-- billing/wallet event types added in C8. Run as app_admin.
--
-- The migration is idempotent: any existing rows with the new event
-- types will violate the restored constraint, so callers MUST purge
-- those rows before running the down migration. The intentional
-- inability to roll back over real audit data is the trade-off for
-- keeping audit_log_security wire-stable.

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
    'authz_allow',
    'signature_fail',
    'key_rotation'
  ));

COMMIT;
