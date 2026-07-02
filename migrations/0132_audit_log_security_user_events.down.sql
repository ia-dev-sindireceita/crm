-- 0132_audit_log_security_user_events.down.sql
-- Reverse 0132: restore the 0129 event_type vocabulary (drop the four
-- user-management literals). Any rows carrying the new literals must be
-- removed before this runs, or the ADD CONSTRAINT fails — expected for a
-- rollback.
BEGIN;

ALTER TABLE audit_log_security
  DROP CONSTRAINT IF EXISTS audit_log_security_event_type_check;

ALTER TABLE audit_log_security
  ADD CONSTRAINT audit_log_security_event_type_check
  CHECK (event_type IN (
    'login',
    'login_fail',
    'logout',
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
    'invoice.cancelled_by_master',
    'master.session.hard_cap_hit',
    'wa_session.banned',
    'wa_session.disconnected',
    'channel.access_granted',
    'channel.access_revoked',
    'channel.restricted_changed'
  ));

COMMIT;
