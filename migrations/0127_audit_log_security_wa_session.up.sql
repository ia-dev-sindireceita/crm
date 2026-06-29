-- 0127_audit_log_security_wa_session.up.sql
-- SIN-66305 (R3 / SIN-66292, origin SIN-66260 Fase 5).
--
-- Extend the audit_log_security event_type CHECK with the two
-- WhatsApp-session terminal-transition events the inbound pump emits
-- (gate 6): 'wa_session.banned' and 'wa_session.disconnected'. STRIDE lens:
-- Repudiation + Logging/Monitoring. The reserved system principal seeded by
-- 0126 is the actor; the session's tenant is the row's tenant_id.
--
-- The full union is restated (PostgreSQL named CHECK constraints are
-- immutable — DROP + ADD is the only path); the list mirrors migration 0122
-- plus the two new literals, so a post-0127 CHECK is correct regardless of
-- which of the intermediate CHECK-extending migrations (0091/0100/0110/0112)
-- have run.
--
-- Depends on audit_log_security (migration 0083). Run as app_admin.
-- Idempotent (DROP CONSTRAINT IF EXISTS + recreate).

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
    'wa_session.disconnected'
  ));

COMMIT;
