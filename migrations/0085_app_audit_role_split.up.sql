-- 0085_app_audit_role_split.up.sql (re-landed from legacy 0014 per ADR 0086)
-- SIN-62252: extend the dedicated app_audit role (created in 0009) so
-- it can INSERT into the new audit_log_security and audit_log_data
-- tables while keeping its existing INSERT on the legacy audit_log.
--
-- 0009 stays unchanged so older branches that haven't picked up the
-- split keep working. After SIN-62252's follow-up retires the legacy
-- table, a future migration will revoke INSERT on audit_log.
--
-- BYPASSRLS on app_audit is preserved (set in 0009): the impersonation
-- and master flows that resolve tenant_id at runtime depend on the
-- writer not getting blocked by the RLS policy when app.tenant_id has
-- not yet been set.
--
-- Run as app_admin. Idempotent.

BEGIN;

REVOKE ALL ON audit_log_security FROM app_audit;
GRANT INSERT ON audit_log_security TO app_audit;

REVOKE ALL ON audit_log_data FROM app_audit;
GRANT INSERT ON audit_log_data TO app_audit;

COMMIT;
