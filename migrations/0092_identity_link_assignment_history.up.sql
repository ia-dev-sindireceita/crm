-- 0092_identity_link_assignment_history.up.sql
-- Fase 2 F2-03 (SIN-62790): Identity aggregate + leadership history.
--
-- Tables (RLS pattern matches 0088_inbox_contacts and docs/adr/0072):
--   * identity                — per-tenant logical contact (N contacts may
--                               merge into 1 identity via merged_into_id).
--   * contact_identity_link   — UNIQUE(contact_id) many-to-one link from
--                               contact to identity.
--   * assignment_history      — append-only ledger of leadership decisions
--                               on a conversation (distinct from the
--                               `assignment` table from 0088 that tracks
--                               open/closed current intervals).
--
-- Backfill: each existing contact gets one identity (1:1) + one
-- contact_identity_link with link_reason='manual'. Idempotent on contact_id.
-- Run as app_admin.

BEGIN;

-- identity ------------------------------------------------------------------
-- merged_into_id is a self-FK (A merged into B → A.merged_into_id = B.id).
-- NULL for terminal rows. ON DELETE SET NULL preserves merged-source rows
-- when the merge target is removed.
CREATE TABLE IF NOT EXISTS identity (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  created_at      timestamptz NOT NULL DEFAULT now(),
  merged_into_id  uuid REFERENCES identity(id) ON DELETE SET NULL
);

CREATE INDEX IF NOT EXISTS identity_tenant_idx
  ON identity (tenant_id);

ALTER TABLE identity OWNER TO app_admin;
ALTER TABLE identity ENABLE ROW LEVEL SECURITY;
ALTER TABLE identity FORCE ROW LEVEL SECURITY;

REVOKE ALL ON identity FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON identity TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON identity TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON identity;
CREATE POLICY tenant_isolation_select ON identity
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON identity;
CREATE POLICY tenant_isolation_insert ON identity
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON identity;
CREATE POLICY tenant_isolation_update ON identity
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON identity;
CREATE POLICY tenant_isolation_delete ON identity
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS identity_master_ops_audit ON identity;
CREATE TRIGGER identity_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON identity
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- contact_identity_link -----------------------------------------------------
-- UNIQUE(contact_id) is the load-bearing invariant: F2-06 domain code will
-- UPDATE this row to repoint contacts on merge, never INSERT a duplicate.
CREATE TABLE IF NOT EXISTS contact_identity_link (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  identity_id  uuid NOT NULL REFERENCES identity(id) ON DELETE CASCADE,
  contact_id   uuid NOT NULL REFERENCES contact(id) ON DELETE CASCADE,
  link_reason  text NOT NULL
                 CHECK (link_reason IN ('phone', 'email', 'external_id', 'manual')),
  created_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT contact_identity_link_contact_uniq UNIQUE (contact_id)
);

CREATE INDEX IF NOT EXISTS contact_identity_link_tenant_identity_idx
  ON contact_identity_link (tenant_id, identity_id);
CREATE INDEX IF NOT EXISTS contact_identity_link_tenant_contact_idx
  ON contact_identity_link (tenant_id, contact_id);

ALTER TABLE contact_identity_link OWNER TO app_admin;
ALTER TABLE contact_identity_link ENABLE ROW LEVEL SECURITY;
ALTER TABLE contact_identity_link FORCE ROW LEVEL SECURITY;

REVOKE ALL ON contact_identity_link FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_identity_link TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON contact_identity_link TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON contact_identity_link;
CREATE POLICY tenant_isolation_select ON contact_identity_link
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON contact_identity_link;
CREATE POLICY tenant_isolation_insert ON contact_identity_link
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON contact_identity_link;
CREATE POLICY tenant_isolation_update ON contact_identity_link
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON contact_identity_link;
CREATE POLICY tenant_isolation_delete ON contact_identity_link
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS contact_identity_link_master_ops_audit ON contact_identity_link;
CREATE TRIGGER contact_identity_link_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON contact_identity_link
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- assignment_history --------------------------------------------------------
-- Hot read pattern: "who leads conversation X under tenant T", served by
-- (tenant_id, conversation_id, assigned_at DESC) LIMIT 1.
CREATE TABLE IF NOT EXISTS assignment_history (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  conversation_id  uuid NOT NULL REFERENCES conversation(id) ON DELETE CASCADE,
  user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  assigned_at      timestamptz NOT NULL DEFAULT now(),
  reason           text NOT NULL
                     CHECK (reason IN ('lead', 'manual', 'reassign'))
);

CREATE INDEX IF NOT EXISTS assignment_history_tenant_conv_assigned_idx
  ON assignment_history (tenant_id, conversation_id, assigned_at DESC);

ALTER TABLE assignment_history OWNER TO app_admin;
ALTER TABLE assignment_history ENABLE ROW LEVEL SECURITY;
ALTER TABLE assignment_history FORCE ROW LEVEL SECURITY;

REVOKE ALL ON assignment_history FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON assignment_history TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON assignment_history TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON assignment_history;
CREATE POLICY tenant_isolation_select ON assignment_history
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON assignment_history;
CREATE POLICY tenant_isolation_insert ON assignment_history
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON assignment_history;
CREATE POLICY tenant_isolation_update ON assignment_history
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON assignment_history;
CREATE POLICY tenant_isolation_delete ON assignment_history
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS assignment_history_master_ops_audit ON assignment_history;
CREATE TRIGGER assignment_history_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON assignment_history
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- backfill ------------------------------------------------------------------
-- Temp table pairs contact_id with a pre-generated identity_id so the two
-- INSERTs reference the same uuid. ON CONFLICT DO NOTHING makes both
-- inserts idempotent.
CREATE TEMP TABLE _identity_backfill_pairs (
  contact_id   uuid PRIMARY KEY,
  tenant_id    uuid NOT NULL,
  identity_id  uuid NOT NULL DEFAULT gen_random_uuid()
) ON COMMIT DROP;

INSERT INTO _identity_backfill_pairs (contact_id, tenant_id)
SELECT c.id, c.tenant_id
  FROM contact c
 WHERE NOT EXISTS (
         SELECT 1 FROM contact_identity_link l WHERE l.contact_id = c.id
       );

INSERT INTO identity (id, tenant_id)
SELECT identity_id, tenant_id FROM _identity_backfill_pairs
ON CONFLICT (id) DO NOTHING;

INSERT INTO contact_identity_link (tenant_id, identity_id, contact_id, link_reason)
SELECT tenant_id, identity_id, contact_id, 'manual'
  FROM _identity_backfill_pairs
ON CONFLICT (contact_id) DO NOTHING;

COMMIT;
