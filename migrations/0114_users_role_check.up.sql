-- 0114_users_role_check.up.sql
-- SIN-63342: schema-layer CHECK constraint on users.role.
--
-- Defense-in-depth backstop for the application-layer tenant role
-- allowlist landed in SIN-63336. The app guard in internal/iam/login.go
-- maps users.role -> session role and silently downgrades unknown
-- values to RoleTenantCommon for tenant logins. This migration ensures
-- the storage layer itself cannot hold an invalid value, so hostile or
-- buggy writers (new code, ORM misuse, hand-run SQL during incident
-- response, a future tenant self-service UI) cannot land an invalid
-- users.role in the first place.
--
-- Template: sessions.role CHECK constraint from
-- migrations/0077_session_activity.up.sql (sessions_role_check).
--
-- Three steps in order:
--   1. Backfill legacy 'agent' rows to 'tenant_common' (only tenant
--      users; masters keep 'master'). 'agent' is the pre-Role-enum
--      historical value; the iam-layer downgrade already maps it to
--      RoleTenantCommon, so promoting storage matches behaviour and
--      the CHECK becomes applicable.
--   2. Update the column DEFAULT so naive INSERTs land on a value
--      that survives the new CHECK.
--   3. Add the CHECK constraint allowlisting the canonical role
--      values plus 'admin' (the totp_required_at marker read by
--      internal/adapter/db/postgres/user_mfa_requirement.go).
--
-- Run as app_admin. Idempotent.

BEGIN;

-- Step 1: backfill legacy 'agent' values on tenant rows.
UPDATE users
   SET role = 'tenant_common'
 WHERE is_master = false
   AND role = 'agent';

-- Step 2: switch column DEFAULT to a least-privilege value that
-- satisfies the new CHECK.
ALTER TABLE users ALTER COLUMN role SET DEFAULT 'tenant_common';

-- Step 3: add the CHECK. DROP IF EXISTS first so the migration is
-- idempotent under up/down/up. Allowlist values:
--   - 'master'           -> master operator row (is_master = true)
--   - 'tenant_gerente'   -> tenant manager (iam.RoleTenantGerente)
--   - 'tenant_atendente' -> tenant agent   (iam.RoleTenantAtendente)
--   - 'tenant_common'    -> tenant common  (iam.RoleTenantCommon)
--   - 'admin'            -> MFA-required marker read by
--                           user_mfa_requirement.go (AdminRole). Not
--                           a valid iam.Role; the allowlist is wider
--                           than iam.Role.Valid() on purpose.
ALTER TABLE users
  DROP CONSTRAINT IF EXISTS users_role_chk;
ALTER TABLE users
  ADD CONSTRAINT users_role_chk
  CHECK (role IN ('master', 'tenant_gerente', 'tenant_atendente', 'tenant_common', 'admin'));

COMMIT;
