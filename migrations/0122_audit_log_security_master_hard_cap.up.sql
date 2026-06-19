-- 0122_audit_log_security_master_hard_cap.up.sql
-- SIN-65232 (follow-up from SIN-65223 / PR #368): extend
-- audit_log_security.event_type CHECK so the master RequireMasterAuth
-- middleware can persist a hard-cap-hit event when a master operator's
-- session crosses created_at + hard TTL (4h, ADR 0073 §D3).
--
-- One new event type joins the controlled vocabulary:
--
--   * master.session.hard_cap_hit — the storage layer reported
--       ErrSessionHardCap for a master __Host-sess-master session (the
--       row was deleted inside the Touch transaction). The middleware
--       clears the cookie + 303s to /m/login; this row is the SLI for
--       forced master-session expiry, letting dashboards split a breach
--       attempt from a benign idle timeout. Target jsonb carries
--       {session_id, audience: "master", route, created_at}; tenant_id
--       is NULL (master rows have no tenant scope per the
--       audit_log_security RLS rules).
--
-- audit_log_security retention (24+ months, never purged by LGPD) is
-- already in place from migration 0083 and is unchanged here. RLS /
-- GRANT / index definitions on the table are also preserved.
--
-- The dotted name matches the master.* / subscription.* vocabulary
-- already accepted by the CHECK (migration 0100/0110). It mirrors the
-- audit.SecurityEventMasterSessionHardCapHit constant in
-- internal/iam/audit/split.go.
--
-- Union note: this migration deliberately RESTATES the full controlled
-- vocabulary (every audit.SecurityEvent constant + the new one) rather
-- than just appending. As the highest-numbered CHECK migration it runs
-- last, so its list is the authoritative final state. This also heals a
-- pre-existing apply-order quirk: 0112_user_mfa (originally 0107,
-- renumbered by SIN-63230) drop-and-recreates the same constraint
-- WITHOUT 'logout', and on a fresh numeric apply it runs after
-- 0110_audit_log_security_logout — leaving the live CHECK missing
-- 'logout'. Restating the full union here makes the post-0122 CHECK
-- include 'logout' regardless of the 0110-vs-0112 order. The list below
-- is kept in lockstep with allSecurityEvents in split.go.
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
    'invoice.cancelled_by_master',
    'master.session.hard_cap_hit'
  ));

COMMIT;
