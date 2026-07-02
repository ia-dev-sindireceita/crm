-- 0131_user_credential_tokens.up.sql
-- SIN-66496 / security gate SIN-66494 — initial-credential policy = OPTION A
-- (invite-by-link). When a gerente creates a tenant user, no server-side
-- usable password ever exists: the account is seeded with a random,
-- un-guessable argon2 hash and an invite token is issued. The invited user
-- follows the link and sets their own password (single-use, TTL-bounded).
--
-- Token secrecy (SIN-66494 §1): the plaintext token is 32 bytes of
-- crypto/rand → base64url; only its SHA-256 is stored here, NEVER the
-- plaintext. Lookup is by hash. High-entropy token → fast hash (SHA-256) is
-- correct; argon2 is for low-entropy human passwords only.
--
-- Numbering: ADR 0086 fork-only, after 0130. Follows the canonical
-- four-policy RLS template (docs/adr/0072-rls-policies.md), tenant_id
-- denormalized for index-backed policies (ADR 0072 §6), mirroring
-- tenant_channels in 0128.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS user_credential_tokens (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  user_id      uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  token_sha256 bytea NOT NULL,            -- SHA-256(token); plaintext never stored
  purpose      text NOT NULL CHECK (purpose IN ('invite', 'reset')),
  expires_at   timestamptz NOT NULL,
  consumed_at  timestamptz,               -- single-use: non-NULL ⇒ already spent
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE UNIQUE INDEX IF NOT EXISTS user_credential_tokens_sha_uniq
  ON user_credential_tokens (token_sha256);
CREATE INDEX IF NOT EXISTS user_credential_tokens_tenant_id_idx
  ON user_credential_tokens (tenant_id);
CREATE INDEX IF NOT EXISTS user_credential_tokens_user_id_idx
  ON user_credential_tokens (user_id);

ALTER TABLE user_credential_tokens OWNER TO app_admin;
ALTER TABLE user_credential_tokens ENABLE ROW LEVEL SECURITY;
ALTER TABLE user_credential_tokens FORCE ROW LEVEL SECURITY;

REVOKE ALL ON user_credential_tokens FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_credential_tokens TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON user_credential_tokens TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON user_credential_tokens;
CREATE POLICY tenant_isolation_select ON user_credential_tokens
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON user_credential_tokens;
CREATE POLICY tenant_isolation_insert ON user_credential_tokens
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON user_credential_tokens;
CREATE POLICY tenant_isolation_update ON user_credential_tokens
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON user_credential_tokens;
CREATE POLICY tenant_isolation_delete ON user_credential_tokens
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS user_credential_tokens_master_ops_audit ON user_credential_tokens;
CREATE TRIGGER user_credential_tokens_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON user_credential_tokens
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
