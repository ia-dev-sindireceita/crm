-- 0110_audit_log_security_logout.down.sql
-- Revert: restore the pre-PR6 CHECK clause (the migration-0107_user_mfa
-- shape that omits 'logout'). Will fail if any audit_log_security row
-- already carries event_type='logout' — that is intentional, audit
-- ledgers do not delete history. Operators MUST archive/relocate those
-- rows before reverting.

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
    '2fa_required',
    '2fa_recovery_used',
    '2fa_recovery_regenerated',
    'role_change',
    'impersonation_start',
    'impersonation_stop',
    'master_grant',
    'authz_deny',
    'authz_allow',
    'signature_fail',
    'key_rotation',
    'master.grant.issued',
    'subscription.created',
    'invoice.cancelled_by_master'
  ));

COMMIT;
