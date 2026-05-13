-- 0086_master_mfa.up.sql
-- Master TOTP enrolment and recovery codes (SIN-62342, ADR 0074).
--
-- Run as app_admin. Idempotent.
--
-- Both tables are master-private: the only application role that may
-- read or write them is app_master_ops. They carry no tenant_id (master
-- users live under is_master = true / tenant_id IS NULL in the users
-- table from migration 0005), so the standard four-policy RLS template
-- doesn't apply — instead we revoke from app_runtime entirely and grant
-- only to app_master_ops, then attach the master_ops_audit_trigger from
-- migration 0002 so every change lands in master_ops_audit.
--
-- The seed in master_mfa.totp_seed_encrypted is application-encrypted
-- with the symmetric app key (env var, see ADR 0074 §1) before it ever
-- reaches Postgres. Plaintext seeds MUST NOT be written here.

BEGIN;

-- ---------------------------------------------------------------------------
-- master_mfa: one row per master user that has completed TOTP enrolment.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS master_mfa (
  user_id                 uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  totp_seed_encrypted     bytea NOT NULL,
  reenroll_required       boolean NOT NULL DEFAULT false,
  enrolled_at             timestamptz NOT NULL DEFAULT now(),
  last_verified_at        timestamptz
);

ALTER TABLE master_mfa OWNER TO app_admin;

REVOKE ALL ON master_mfa FROM PUBLIC;
REVOKE ALL ON master_mfa FROM app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON master_mfa TO app_master_ops;

DROP TRIGGER IF EXISTS master_mfa_master_ops_audit ON master_mfa;
CREATE TRIGGER master_mfa_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_mfa
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- master_recovery_code: ten Argon2id-hashed single-use codes per master.
-- A row's code_hash is "argon2id$v=19$m=…,t=…,p=…$<salt>$<hash>" produced
-- by the helper from ADR 0070 (internal/iam/password). The plaintext code
-- is shown to the user exactly once at enrol/regen time and MUST NOT be
-- written to disk.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS master_recovery_code (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  user_id       uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  code_hash     text NOT NULL,
  consumed_at   timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Lookup pattern is "list all not-yet-consumed codes for this master
-- and Argon2id-verify each candidate against the submitted plaintext"
-- (ADR 0074 §2 — linear scan, salts differ so no equality lookup is
-- possible). The partial index keeps the live set small and lets the
-- regenerate path mass-mark consumed cheaply.
CREATE INDEX IF NOT EXISTS master_recovery_code_active_idx
  ON master_recovery_code (user_id)
  WHERE consumed_at IS NULL;

CREATE INDEX IF NOT EXISTS master_recovery_code_user_consumed_idx
  ON master_recovery_code (user_id, consumed_at);

ALTER TABLE master_recovery_code OWNER TO app_admin;

REVOKE ALL ON master_recovery_code FROM PUBLIC;
REVOKE ALL ON master_recovery_code FROM app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON master_recovery_code TO app_master_ops;

DROP TRIGGER IF EXISTS master_recovery_code_master_ops_audit ON master_recovery_code;
CREATE TRIGGER master_recovery_code_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_recovery_code
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
