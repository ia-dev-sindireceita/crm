-- 0101_ai_policy_consent.up.sql
-- Fase 3 decisão #8 / SIN-62927: per-scope consent ledger gating the first
-- IA call for a tenant operator.
--
-- The IA gate (logic landing in W4-B) blocks every IA invocation until an
-- operator has reviewed and accepted the anonymized payload preview for
-- the (tenant, scope_kind, scope_id) triple they are about to act under.
-- The accepted preview is identified by its SHA-256 digest in
-- payload_hash (32 bytes); we never persist the cleartext payload.
--
-- Versioned columns:
--   * anonymizer_version — version of the anonymizer that produced the
--     preview the operator accepted. When the active anonymizer rolls
--     forward, prior consent rows no longer match and the gate forces a
--     re-consent flow on the next IA attempt.
--   * prompt_version — version of the prompt template the preview was
--     rendered against. Same re-consent trigger as anonymizer_version.
--
-- Invariant: one active consent per (tenant_id, scope_kind, scope_id).
-- Re-consent is an UPDATE on the existing row (UPSERT in the service),
-- not a second INSERT. The UNIQUE constraint enforces this and gives
-- the resolver an indexed lookup for "is this scope consented under
-- the current anonymizer+prompt version".
--
-- Pattern mirrored from 0098 (ai_policy): RLS tenant-scoped, FORCE RLS,
-- GRANTs to app_runtime + app_master_ops, master_ops_audit_trigger.
--
-- Run as app_admin. Idempotent: CREATE TABLE IF NOT EXISTS, DROP POLICY
-- IF EXISTS, DROP TRIGGER IF EXISTS.

BEGIN;

CREATE TABLE IF NOT EXISTS ai_policy_consent (
  id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id            uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  scope_kind           text NOT NULL
                         CHECK (scope_kind IN ('tenant','team','channel')),
  scope_id             text NOT NULL,
  actor_user_id        uuid REFERENCES users(id) ON DELETE SET NULL,
  payload_hash         bytea NOT NULL,
  anonymizer_version   text NOT NULL,
  prompt_version       text NOT NULL,
  accepted_at          timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT ai_policy_consent_scope_uniq
    UNIQUE (tenant_id, scope_kind, scope_id)
);

-- RLS path: tenant predicate first; the planner satisfies the SELECT/
-- UPDATE/DELETE filter from this index before the UNIQUE constraint
-- comes into play. UNIQUE already provides an index for the
-- (tenant_id, scope_kind, scope_id) probe; the dedicated tenant_id
-- index keeps "list every consent for this tenant" scans cheap.
CREATE INDEX IF NOT EXISTS ai_policy_consent_tenant_id_idx
  ON ai_policy_consent (tenant_id);

ALTER TABLE ai_policy_consent OWNER TO app_admin;
ALTER TABLE ai_policy_consent ENABLE ROW LEVEL SECURITY;
ALTER TABLE ai_policy_consent FORCE ROW LEVEL SECURITY;

REVOKE ALL ON ai_policy_consent FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_policy_consent TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ai_policy_consent TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON ai_policy_consent;
CREATE POLICY tenant_isolation_select ON ai_policy_consent
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON ai_policy_consent;
CREATE POLICY tenant_isolation_insert ON ai_policy_consent
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON ai_policy_consent;
CREATE POLICY tenant_isolation_update ON ai_policy_consent
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON ai_policy_consent;
CREATE POLICY tenant_isolation_delete ON ai_policy_consent
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS ai_policy_consent_master_ops_audit ON ai_policy_consent;
CREATE TRIGGER ai_policy_consent_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON ai_policy_consent
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
