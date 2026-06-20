-- migrations/seed/stg.sql
-- Staging seed (SIN-62209). Two tenants (acme, globex), one agent user
-- per tenant, one global master user, and (SIN-63336) one tenant_gerente
-- admin user on acme. Idempotent: ON CONFLICT clauses let `make seed-stg`
-- run repeatedly without erroring.
--
-- Run as app_admin (BYPASSRLS) so policies do not block the inserts.
--
-- Password hashes are argon2id PHC encodings of the literal string
-- "stg-password" with the package-iam parameters (m=65536, t=3, p=4,
-- 16-byte salt, 32-byte hash) — adequate for staging fixtures, NEVER
-- for prod. Keep this file staging-only; the prod seed lives in PR9 and
-- is sourced from secrets. SIN-63154 swapped bcrypt placeholders to
-- argon2id because internal/iam.VerifyPassword only accepts argon2id
-- (decodePHC rejects $2a$ outright). Regenerate via iam.HashPassword;
-- a regression test in internal/iam/password_seed_test.go re-verifies
-- each hash against the verifier on every `go test ./...`.
--
-- SIN-63336 added the per-tenant gerente row for acme so the LGPD admin
-- authz positive case is exercisable on the seeded staging environment.
-- The user is born with totp_required_at=now() because the LGPD admin
-- surface (Fase 6) only renders after the MFA-requirement reader
-- (internal/adapter/db/postgres/user_mfa_requirement.go) treats the
-- principal as 2FA-required. Globex stays as the cross-tenant control
-- with no gerente, so an authorizer regression that grants gerente
-- across tenants fails the AC#5 sweep instead of silently shipping.
--
-- SIN-63146 — tenant FQDNs and per-tenant agent emails are templated on
-- the `base_domain` psql variable so the same file seeds dev (default
-- `crm.local`) and staging on the real VPS base domain. Callers pass it
-- with psql -v:
--
--   psql -v base_domain="${STG_BASE_DOMAIN:-crm.local}" ... < stg.sql
--
-- The Makefile `seed-stg` target wires the default; the runbook §5d
-- block shows the staging override (typically the FQDN suffix already
-- announced in DNS, e.g. `crm.someu.com.br`). The master user stays on
-- `master@crm.local` because it is global (NULL tenant_id) and the
-- master surface is not bound to a tenant FQDN.

BEGIN;

-- Stable UUIDs so callers can reference them in scripts/tests.
-- Tenant hosts are templated on :'base_domain' (default crm.local).
INSERT INTO tenants (id, name, host) VALUES
  ('00000000-0000-0000-0000-00000000ac01', 'acme',   'acme.'   || :'base_domain'),
  ('00000000-0000-0000-0000-00000000eb02', 'globex', 'globex.' || :'base_domain')
ON CONFLICT (id) DO NOTHING;

-- SIN-63858: tenant agent rows seed with role='tenant_atendente'. The
-- SIN-63821 /inbox gate requires {tenant_atendente, tenant_gerente};
-- the prior SIN-63342 seeding as 'tenant_common' shipped these users
-- one role below the gate and made every staging operator 403 on
-- /inbox. Atendente is a strict superset of common on tenant-scope
-- actions (see authorizer-matrix audit on SIN-63858), so promoting the
-- seed does not strip access to any existing route — it only adds
-- ActionTenantInboxRead + ActionTenantMessageSend + ActionTenantContactUpdate.
-- The 0114 users_role_chk CHECK allowlist includes 'tenant_atendente',
-- so the new value survives the schema-level guard. Migration 0115
-- promotes already-seeded staging rows in place; this file is the
-- source of truth for a fresh DB.
INSERT INTO users (id, tenant_id, email, password_hash, role, is_master) VALUES
  ('00000000-0000-0000-0000-0000000a0e01',
   '00000000-0000-0000-0000-00000000ac01',
   'agent@acme.'   || :'base_domain',
   '$argon2id$v=19$m=65536,t=3,p=4$xdUl6TonL7/7uBXHOr1l6A$A1WB5t0HT3Du/tzT3o9wlZxcjiknaCozvcS9evnIPiM',
   'tenant_atendente', false),
  ('00000000-0000-0000-0000-0000000e0e02',
   '00000000-0000-0000-0000-00000000eb02',
   'agent@globex.' || :'base_domain',
   '$argon2id$v=19$m=65536,t=3,p=4$V2UIy0HwezJHHCZ7V6GYzA$g+BY8yIY7FfEsS/87CjEaX7+iXLj18FmOUBQCpELZ8k',
   'tenant_atendente', false),
  ('00000000-0000-0000-0000-0000000a57e7',
   NULL,
   'master@crm.local',
   '$argon2id$v=19$m=65536,t=3,p=4$KVbQRF4vARjcL2LzJ3tPAw$T1pX3waMktzhSjBoigZjPQmHGcJiN7tqeo0NvWX9WcE',
   'master', true)
ON CONFLICT (id) DO NOTHING;

-- SIN-63336: acme tenant_gerente. Role 'tenant_gerente' lets the LGPD
-- authorizer grant ActionTenantLGPDExport/Delete; totp_required_at=now()
-- makes the MFA-requirement reader treat this user as 2FA-required so
-- the Fase 6 admin surface renders. Hash is a fresh argon2id PHC of
-- "stg-password" (distinct salt from agent@acme, same params). Globex
-- intentionally has no gerente — it is the cross-tenant control.
INSERT INTO users (id, tenant_id, email, password_hash, role, is_master, totp_required_at) VALUES
  ('00000000-0000-0000-0000-0000000ad301',
   '00000000-0000-0000-0000-00000000ac01',
   'admin@acme.'   || :'base_domain',
   '$argon2id$v=19$m=65536,t=3,p=4$AQ2aIYpmI90lFTr7haT7xw$S5BS4ORDTDBmaFoP5/4U54z4vHOGSz6tPqwx0Hkdkm0',
   'tenant_gerente', false, now())
ON CONFLICT (id) DO NOTHING;

-- SIN-63858 — agent@* are tenant_atendente; the gerente arm of the
-- inbox gate is seeded separately via admin@acme above. Common-only
-- fixtures are not exercised in staging; add them only if a future
-- test demands the negative case.

-- SIN-65285: enable AI-assist for the acme tenant in staging so the
-- LLM/OpenRouter round-trip is exercisable without a manual gerente
-- toggle. Tenant-level row (scope_type='tenant', scope_id=tenant UUID
-- as text — matches the resolver's ScopeTenant cascade branch).
-- model defaults to openrouter/auto; OPENROUTER_MODEL env overrides
-- the runtime model so this row does not pin a specific model string.
-- Idempotent: ON CONFLICT DO UPDATE so re-running `make seed-stg`
-- after a DB reset refreshes the row safely.
--
-- Wrapped in a DO block that skips gracefully when the ai_policy table
-- does not yet exist (minimal integration-test DBs may not run migration
-- 0098 which also creates ai_summary — a table with an FK to
-- conversation that many test harnesses omit). Real staging always has
-- the table.
DO $$
BEGIN
  IF EXISTS (
    SELECT FROM information_schema.tables
    WHERE table_schema = 'public' AND table_name = 'ai_policy'
  ) THEN
    INSERT INTO ai_policy (id, tenant_id, scope_type, scope_id, ai_enabled, model, anonymize, opt_in)
    VALUES (
      '00000000-0000-0000-0000-00000065ac01',
      '00000000-0000-0000-0000-00000000ac01',
      'tenant',
      '00000000-0000-0000-0000-00000000ac01',
      true,
      'openrouter/auto',
      false,
      false
    )
    ON CONFLICT (tenant_id, scope_type, scope_id) DO UPDATE
      SET ai_enabled = true,
          updated_at = now();
  END IF;
END $$;

COMMIT;
