-- 0112_user_mfa.down.sql (renumbered from 0107 by SIN-63230)

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
    'key_rotation',
    'master.grant.issued',
    'subscription.created',
    'invoice.cancelled_by_master'
  ));

DROP TABLE IF EXISTS user_mfa_pending_session;
DROP TABLE IF EXISTS user_recovery_code;
DROP TABLE IF EXISTS user_mfa;

ALTER TABLE users DROP COLUMN IF EXISTS totp_required_at;

COMMIT;
