-- 0134_users_deactivated_at.up.sql
-- SIN-66496 (child of SIN-66493 / board SIN-66492) — tenant user management.
--
-- Numbering: ADR 0086 fork-only numbering. Last landed migration on fork
-- main is 0129_audit_log_security_channel_access, so this batch starts at
-- 0130. (The security gate SIN-66494 suggested 0116; that range was already
-- consumed by the functional tranche — verified against fork main.)
--
-- "Desativar-não-deletar" (SIN-66494 guardrail G5): a tenant gerente can
-- soft-deactivate a user. The row is preserved (history / audit trail); the
-- login path filters it out. This is an access-control mechanism and does
-- NOT replace the LGPD right-to-erasure flow (a separate flow that anonymizes
-- / deletes PII).
--
-- Backward-compatible: existing rows get NULL (= active). The login credential
-- reader adds `AND deactivated_at IS NULL`; active users are unaffected.
-- Reversible: DOWN drops the column.
--
-- Run as app_admin. Idempotent (IF NOT EXISTS).

BEGIN;

ALTER TABLE users ADD COLUMN IF NOT EXISTS deactivated_at timestamptz;

-- Partial index: the management surface lists active users first and the
-- anti-lockout "last active gerente" count filters on deactivated_at IS NULL.
CREATE INDEX IF NOT EXISTS users_tenant_active_idx
  ON users (tenant_id)
  WHERE deactivated_at IS NULL;

COMMIT;
