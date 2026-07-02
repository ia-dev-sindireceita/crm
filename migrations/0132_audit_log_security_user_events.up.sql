-- 0132_audit_log_security_user_events.up.sql
-- SIN-66496 / security gate SIN-66494 §3 — audit vocabulary for tenant user
-- management. These actions are tenant-initiated (run as app_runtime, NOT
-- app_master_ops), so the master_ops trigger/GUC does NOT apply; the
-- actor_user_id is written explicitly by Go (audit.SplitLogger.WriteSecurity
-- with ActorUserID = session.UserID).
--
-- New event_type literals (role_change already existed):
--   * user_create      — a gerente created a tenant user (invite issued).
--   * user_deactivate  — soft-deactivation (desativar-não-deletar).
--   * user_reactivate  — a deactivated user was reactivated.
--   * password_reset   — an invite/reset token was consumed / password set.
--
-- The CHECK is DROP + re-ADD with the FULL restated vocabulary (mirrors the
-- 0129 list plus the four above). Numbering: ADR 0086 fork-only, after 0131.
--
-- Run as app_admin. Reversible: DOWN restores the 0129 vocabulary.

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
    'channel.restricted_changed',
    'user_create',
    'user_deactivate',
    'user_reactivate',
    'password_reset'
  ));

COMMIT;
