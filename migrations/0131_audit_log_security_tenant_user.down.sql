-- 0131_audit_log_security_tenant_user.down.sql
-- Reverse of 0131 up: roll the audit_log_security event_type CHECK back to
-- its 0129 union. This rejects any row still carrying a tenant.user.* event
-- type (the same fail-loud guard the other audit-vocabulary down steps use;
-- safe only on a developer rollback where no such rows exist). Run as
-- app_admin. Idempotent.

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
