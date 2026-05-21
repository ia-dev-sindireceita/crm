-- 0107_user_mfa.up.sql
-- Tenant-scope TOTP enrolment and recovery codes (SIN-63184, Fase 6 PR1).
--
-- Counterpart of the master tables from 0086 — RLS-isolated per tenant
-- so app_runtime cannot reach across tenants. Same hash/cipher contract:
--
--   * user_mfa.totp_seed_encrypted is application-encrypted with the
--     symmetric app key (env var, ADR 0074 §1). Plaintext seeds MUST
--     NOT be written here.
--   * user_recovery_code.code_hash is "argon2id$..." produced by the
--     helper from ADR 0070 (internal/iam/password). Plaintext codes
--     are shown to the user exactly once at enrol/regen time.
--
-- Run as app_admin. Idempotent.

BEGIN;

-- ---------------------------------------------------------------------------
-- users.totp_required_at: per-user opt-in flag. NULL = TOTP not required;
-- non-NULL = TOTP required since that timestamp.
-- ---------------------------------------------------------------------------
ALTER TABLE users
  ADD COLUMN IF NOT EXISTS totp_required_at timestamptz;

-- ---------------------------------------------------------------------------
-- user_mfa: one row per tenant user that has completed TOTP enrolment.
-- tenant_id duplicated for the canonical four-policy RLS template.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_mfa (
  user_id                 uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  tenant_id               uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  totp_seed_encrypted     bytea NOT NULL,
  reenroll_required       boolean NOT NULL DEFAULT false,
  enrolled_at             timestamptz NOT NULL DEFAULT now(),
  last_verified_at        timestamptz
);

CREATE INDEX IF NOT EXISTS user_mfa_tenant_idx ON user_mfa (tenant_id);

ALTER TABLE user_mfa OWNER TO app_admin;

ALTER TABLE user_mfa ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_mfa FORCE ROW LEVEL SECURITY;

REVOKE ALL ON user_mfa FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON user_mfa;
CREATE POLICY tenant_isolation_select ON user_mfa
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON user_mfa;
CREATE POLICY tenant_isolation_insert ON user_mfa
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON user_mfa;
CREATE POLICY tenant_isolation_update ON user_mfa
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON user_mfa;
CREATE POLICY tenant_isolation_delete ON user_mfa
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------------------
-- user_recovery_code: ten Argon2id-hashed single-use codes per tenant user.
-- AC #6 names the column as used_at (not consumed_at like master).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_recovery_code (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  code_hash     text NOT NULL,
  used_at       timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS user_recovery_code_active_idx
  ON user_recovery_code (user_id)
  WHERE used_at IS NULL;

CREATE INDEX IF NOT EXISTS user_recovery_code_tenant_idx
  ON user_recovery_code (tenant_id);

ALTER TABLE user_recovery_code OWNER TO app_admin;

ALTER TABLE user_recovery_code ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_recovery_code FORCE ROW LEVEL SECURITY;

REVOKE ALL ON user_recovery_code FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_recovery_code TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_recovery_code TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON user_recovery_code;
CREATE POLICY tenant_isolation_select ON user_recovery_code
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON user_recovery_code;
CREATE POLICY tenant_isolation_insert ON user_recovery_code
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON user_recovery_code;
CREATE POLICY tenant_isolation_update ON user_recovery_code
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON user_recovery_code;
CREATE POLICY tenant_isolation_delete ON user_recovery_code
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------------------
-- user_mfa_pending_session: short-lived state between password-auth and
-- TOTP-verify. AC #1 requires the session cookie to be issued ONLY after
-- the second factor is validated, so the post-password redirect carries
-- a distinct pending-mfa token that does not unlock authed routes.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS user_mfa_pending_session (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  created_at   timestamptz NOT NULL DEFAULT now(),
  expires_at   timestamptz NOT NULL,
  next_path    text NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS user_mfa_pending_session_user_idx
  ON user_mfa_pending_session (user_id);
CREATE INDEX IF NOT EXISTS user_mfa_pending_session_expires_idx
  ON user_mfa_pending_session (expires_at);

ALTER TABLE user_mfa_pending_session OWNER TO app_admin;

ALTER TABLE user_mfa_pending_session ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_mfa_pending_session FORCE ROW LEVEL SECURITY;

REVOKE ALL ON user_mfa_pending_session FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa_pending_session TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_mfa_pending_session TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON user_mfa_pending_session;
CREATE POLICY tenant_isolation_select ON user_mfa_pending_session
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON user_mfa_pending_session;
CREATE POLICY tenant_isolation_insert ON user_mfa_pending_session
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON user_mfa_pending_session;
CREATE POLICY tenant_isolation_delete ON user_mfa_pending_session
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

-- ---------------------------------------------------------------------------
-- audit_log_security: extend event_type CHECK to allow the new tenant
-- 2FA events. Drop-and-recreate the constraint so the table picks up
-- the new union without losing the existing rows.
-- ---------------------------------------------------------------------------
ALTER TABLE audit_log_security
  DROP CONSTRAINT IF EXISTS audit_log_security_event_type_check;

ALTER TABLE audit_log_security
  ADD CONSTRAINT audit_log_security_event_type_check
  CHECK (event_type IN (
    'login',
    'login_fail',
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
