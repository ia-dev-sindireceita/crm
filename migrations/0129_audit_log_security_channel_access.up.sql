-- 0129_audit_log_security_channel_access.up.sql
-- SIN-66405 (from SIN-66378 P3 security review, SIN-66392 / PR #446).
--
-- Extend the audit_log_security event_type CHECK with the three
-- per-channel access-change privilege events the channel-management admin
-- surface (internal/web/channels) emits: 'channel.access_granted',
-- 'channel.access_revoked' and 'channel.restricted_changed'. OWASP A09
-- (logging/monitoring failures) + least-privilege observability: grant /
-- revoke and the open↔restricted flip are privilege changes that must
-- leave a tamper-evident trail. The actor is the authenticated gerente;
-- tenant_id is the channel's tenant.
--
-- The full union is restated (PostgreSQL named CHECK constraints are
-- immutable — DROP + ADD is the only path); the list mirrors migration
-- 0127 plus the three new literals, so a post-0129 CHECK is correct
-- regardless of which of the intermediate CHECK-extending migrations have
-- run.
--
-- Depends on audit_log_security (migration 0083). Run as app_admin.
-- Idempotent (DROP CONSTRAINT IF EXISTS + recreate). Backward-compatible:
-- the constraint only widens the accepted set, so existing rows and
-- pre-0129 writers are unaffected; the down step narrows it back.

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
