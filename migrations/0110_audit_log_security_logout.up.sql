-- 0110_audit_log_security_logout.up.sql
-- SIN-63188 / Fase 6 PR6: extend audit_log_security.event_type CHECK so
-- the tenant /logout and master /m/logout handlers can persist a
-- terminal logout event alongside the existing login row.
--
-- One new event type joins the controlled vocabulary:
--
--   * logout — the cookie-bearing principal explicitly logged out
--              (POST /logout for tenants, GET/POST /m/logout for master)
--              OR the activity middleware tore down a stale session
--              (idle/hard timeout). Target jsonb carries
--              {session_id, audience: "tenant"|"master", reason:
--              "user_initiated"|"idle_timeout"|"hard_cap"|"role_change"}.
--
-- audit_log_security retention (24+ months, never purged by LGPD) is
-- already in place from migration 0083 and is unchanged by this
-- migration. RLS / GRANT / index definitions on the table are also
-- preserved.
--
-- Numbering note: 0110 was the next free index after the Fase 6
-- siblings landed on main. PR1 and PR2 originally both claimed 0107
-- and were renumbered by SIN-63230 to 0112_user_mfa and
-- 0113_consent_record; slot 0107 now belongs to PR3
-- (0107_lgpd_deletion_request + 0108_tenants_dpo_settings); PR4 is
-- 0109_tenants_privacy_policy_markdown.
--
-- Idempotent (DROP CONSTRAINT IF EXISTS + recreate). Run as app_admin.

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
    'invoice.cancelled_by_master'
  ));

COMMIT;
