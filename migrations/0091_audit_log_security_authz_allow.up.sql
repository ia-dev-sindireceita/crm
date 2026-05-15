-- 0091_audit_log_security_authz_allow.up.sql
-- SIN-62254 / ADR 0004 §6: extend audit_log_security.event_type CHECK
-- so the authz wrapper can persist sampled allow rows alongside the
-- existing 100%-deny rows.
--
-- The wrapper writes one row per Authorizer decision it persists:
--
--   * outcome='deny'  → event_type='authz_deny'  (always sampled, 100%)
--   * outcome='allow' → event_type='authz_allow' (sampled, 1% baseline
--                                                 via deterministic hash
--                                                 over request_id)
--
-- audit_log_security retention (24+ months, never purged by LGPD) is
-- already in place from migration 0083 and is unchanged by this
-- migration. RLS / GRANT / index definitions on the table are also
-- preserved.
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
    '2fa_enroll',
    '2fa_verify',
    'role_change',
    'impersonation_start',
    'impersonation_stop',
    'master_grant',
    'authz_deny',
    'authz_allow',
    'signature_fail',
    'key_rotation'
  ));

COMMIT;
