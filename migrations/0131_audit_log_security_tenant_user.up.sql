-- 0131_audit_log_security_tenant_user.up.sql
-- SIN-66499 (parent SIN-66492) — tenant user-management surface.
--
-- Extend the audit_log_security event_type CHECK with the three tenant
-- user-management privilege events the /settings/users surface emits:
--   * 'tenant.user.created'      — a gerente created a user. Target carries
--                                  {user_id, email, role}.
--   * 'tenant.user.role_changed' — a user's role was changed. Target carries
--                                  {user_id, from, to}.
--   * 'tenant.user.deactivated'  — a user was deactivated (soft-delete).
--                                  Target carries {user_id, role}.
--
-- OWASP A09 (logging/monitoring failures) + least-privilege observability:
-- creating a user, changing its role, and deactivating it are privilege
-- changes that must leave a tamper-evident trail. The actor is the
-- authenticated gerente (the routes are gated to RoleTenantGerente);
-- tenant_id is the acting gerente's tenant.
--
-- The full union is restated (PostgreSQL named CHECK constraints are
-- immutable — DROP + ADD is the only path); the list mirrors migration 0129
-- plus the three new literals. Depends on audit_log_security (migration
-- 0083). Run as app_admin. Idempotent (DROP CONSTRAINT IF EXISTS +
-- recreate). Backward-compatible: the constraint only widens the accepted
-- set, so existing rows and pre-0131 writers are unaffected.

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
    'tenant.user.created',
    'tenant.user.role_changed',
    'tenant.user.deactivated'
  ));

COMMIT;
