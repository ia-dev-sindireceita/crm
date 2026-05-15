-- 0093_funnel_stage_transition.up.sql
-- Fase 2 F2-04 (SIN-62787): basic funnel — five default stages per tenant
-- + per-conversation transition history.
--
-- Tables (RLS pattern matches 0088_inbox_contacts and 0092_identity per
-- docs/adr/0072-rls-policies.md):
--
--   * funnel_stage       — per-tenant stage definition (key/label/position).
--                          UNIQUE (tenant_id, key) lets domain code address
--                          a stage by its stable key instead of its uuid.
--   * funnel_transition  — append-only ledger of stage changes on a
--                          conversation. from_stage_id is nullable for the
--                          first-ever transition (entering the funnel).
--
-- Seed:
--   * seed_default_funnel_stages(p_tenant_id) — idempotent INSERT of the
--     five default stages (`novo`, `qualificando`, `proposta`, `ganho`,
--     `perdido`) at positions 1..5 with is_default=true.
--   * AFTER INSERT trigger on tenants calls the seed function so every new
--     tenant gets the default funnel without an explicit application call.
--   * Backfill at the end of this migration calls the same function for
--     every existing tenant, also idempotent.
--
-- Run as app_admin. Idempotent.

BEGIN;

-- ---------------------------------------------------------------------------
-- funnel_stage
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS funnel_stage (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  key         text NOT NULL,
  label       text NOT NULL,
  position    int  NOT NULL,
  is_default  boolean NOT NULL DEFAULT false,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT funnel_stage_tenant_key_uniq UNIQUE (tenant_id, key)
);

CREATE INDEX IF NOT EXISTS funnel_stage_tenant_position_idx
  ON funnel_stage (tenant_id, position);

ALTER TABLE funnel_stage OWNER TO app_admin;
ALTER TABLE funnel_stage ENABLE ROW LEVEL SECURITY;
ALTER TABLE funnel_stage FORCE ROW LEVEL SECURITY;

REVOKE ALL ON funnel_stage FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_stage TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_stage TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON funnel_stage;
CREATE POLICY tenant_isolation_select ON funnel_stage
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON funnel_stage;
CREATE POLICY tenant_isolation_insert ON funnel_stage
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON funnel_stage;
CREATE POLICY tenant_isolation_update ON funnel_stage
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON funnel_stage;
CREATE POLICY tenant_isolation_delete ON funnel_stage
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS funnel_stage_master_ops_audit ON funnel_stage;
CREATE TRIGGER funnel_stage_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON funnel_stage
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- funnel_transition
-- from_stage_id is nullable to represent "first entry into the funnel".
-- to_stage_id is RESTRICTed: deleting a stage that is still referenced by
-- history must fail loudly so operators do not silently lose audit context.
-- transitioned_by_user_id is NOT NULL + CASCADE to mirror assignment_history
-- (0092) — the durable who-did-what trail lives in master_ops_audit_trigger.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS funnel_transition (
  id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id                uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id          uuid NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
  from_stage_id            uuid REFERENCES funnel_stage(id) ON DELETE RESTRICT,
  to_stage_id              uuid NOT NULL REFERENCES funnel_stage(id) ON DELETE RESTRICT,
  transitioned_by_user_id  uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  transitioned_at          timestamptz NOT NULL DEFAULT now(),
  reason                   text
);

CREATE INDEX IF NOT EXISTS funnel_transition_tenant_conv_at_idx
  ON funnel_transition (tenant_id, conversation_id, transitioned_at DESC);

ALTER TABLE funnel_transition OWNER TO app_admin;
ALTER TABLE funnel_transition ENABLE ROW LEVEL SECURITY;
ALTER TABLE funnel_transition FORCE ROW LEVEL SECURITY;

REVOKE ALL ON funnel_transition FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_transition TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_transition TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON funnel_transition;
CREATE POLICY tenant_isolation_select ON funnel_transition
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON funnel_transition;
CREATE POLICY tenant_isolation_insert ON funnel_transition
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON funnel_transition;
CREATE POLICY tenant_isolation_update ON funnel_transition
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON funnel_transition;
CREATE POLICY tenant_isolation_delete ON funnel_transition
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS funnel_transition_master_ops_audit ON funnel_transition;
CREATE TRIGGER funnel_transition_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON funnel_transition
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- seed_default_funnel_stages: idempotent seed of the five default stages
-- for one tenant. ON CONFLICT (tenant_id, key) DO NOTHING means re-running
-- it on a tenant that already has a customised funnel is a safe no-op for
-- the keys that already exist.
-- ---------------------------------------------------------------------------
CREATE OR REPLACE FUNCTION seed_default_funnel_stages(p_tenant_id uuid)
RETURNS void
LANGUAGE plpgsql AS $$
BEGIN
  INSERT INTO funnel_stage (tenant_id, key, label, position, is_default) VALUES
    (p_tenant_id, 'novo',         'Novo',         1, true),
    (p_tenant_id, 'qualificando', 'Qualificando', 2, true),
    (p_tenant_id, 'proposta',     'Proposta',     3, true),
    (p_tenant_id, 'ganho',        'Ganho',        4, true),
    (p_tenant_id, 'perdido',      'Perdido',      5, true)
  ON CONFLICT (tenant_id, key) DO NOTHING;
END;
$$;

ALTER FUNCTION seed_default_funnel_stages(uuid) OWNER TO app_admin;

CREATE OR REPLACE FUNCTION seed_default_funnel_stages_trigger()
RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
  PERFORM seed_default_funnel_stages(NEW.id);
  RETURN NEW;
END;
$$;

ALTER FUNCTION seed_default_funnel_stages_trigger() OWNER TO app_admin;

DROP TRIGGER IF EXISTS tenants_seed_funnel_stages ON tenants;
CREATE TRIGGER tenants_seed_funnel_stages
  AFTER INSERT ON tenants
  FOR EACH ROW EXECUTE FUNCTION seed_default_funnel_stages_trigger();

-- ---------------------------------------------------------------------------
-- Backfill: seed every existing tenant. ON CONFLICT in the seed function
-- keeps this idempotent across re-runs of the migration.
-- ---------------------------------------------------------------------------
DO $$
DECLARE
  t record;
BEGIN
  FOR t IN SELECT id FROM tenants LOOP
    PERFORM seed_default_funnel_stages(t.id);
  END LOOP;
END $$;

COMMIT;
