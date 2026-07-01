-- 0128_channel_instances.up.sql
-- Phase 1 of the multi-channel-per-tenant work (SIN-66389, child of
-- SIN-66378 / SIN-66375).
--
-- Numbering: ADR 0086 fork-only numbering. The last previously-landed
-- migration on fork main is 0127_audit_log_security_wa_session, so this
-- batch starts at 0128. The issue body suggested ~0116; that range was
-- already consumed by the functional tranche (0116_master_impersonation
-- … 0127_audit_log_security_wa_session) before this PR was cut — VERIFY
-- against fork main numbering, mind the dup-migration landmine.
--
-- Tables (both in one migration because they form a single deployable
-- unit — same pattern as 0088_inbox_contacts):
--
--   * tenant_channels  — one row per concrete channel instance a tenant
--                        operates (e.g. a specific WhatsApp number or
--                        Instagram handle). The legacy free-text
--                        conversation.channel string is promoted into a
--                        first-class, addressable, access-controllable
--                        entity.
--   * channel_access   — which tenant users may see/act on which channel
--                        instance. Default after backfill: every current
--                        user has access to every backfilled channel, so
--                        the inbox does not regress.
--
-- conversation gains a nullable channel_id FK -> tenant_channels(id). The
-- legacy `channel` text column is kept for one-release overlap so the
-- change is reversible and so code that still reads conversation.channel
-- keeps working during the rollout.
--
-- All tenant-scoped tables follow the canonical four-policy RLS template
-- from docs/adr/0072-rls-policies.md. tenant_id is denormalized onto
-- channel_access so the policy USING/WITH CHECK clauses are index-backed
-- (ADR 0072 §process rule #6), mirroring contact_channel_identity in
-- 0088.
--
-- Run as app_admin. Idempotent (IF NOT EXISTS / ON CONFLICT DO NOTHING).

BEGIN;

-- ---------------------------------------------------------------------------
-- tenant_channels
-- A concrete channel instance. channel_key is the channel family
-- ('whatsapp', 'instagram', …); external_id is the address within that
-- family (the number / handle). external_id defaults to '' so the
-- backfill (which only knows the legacy channel family, not a specific
-- number) can materialize a placeholder instance per family without
-- violating the UNIQUE constraint. display_name is human-facing and may
-- be renamed without touching the addressing tuple.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS tenant_channels (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_key   text NOT NULL,
  external_id   text NOT NULL DEFAULT '',
  display_name  text NOT NULL DEFAULT '',
  is_active     boolean NOT NULL DEFAULT true,
  restricted    boolean NOT NULL DEFAULT false,
  created_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT tenant_channels_tenant_key_external_uniq
    UNIQUE (tenant_id, channel_key, external_id)
);

CREATE INDEX IF NOT EXISTS tenant_channels_tenant_id_idx
  ON tenant_channels (tenant_id);

ALTER TABLE tenant_channels OWNER TO app_admin;
ALTER TABLE tenant_channels ENABLE ROW LEVEL SECURITY;
ALTER TABLE tenant_channels FORCE ROW LEVEL SECURITY;

REVOKE ALL ON tenant_channels FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_channels TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_channels TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON tenant_channels;
CREATE POLICY tenant_isolation_select ON tenant_channels
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON tenant_channels;
CREATE POLICY tenant_isolation_insert ON tenant_channels
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON tenant_channels;
CREATE POLICY tenant_isolation_update ON tenant_channels
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON tenant_channels;
CREATE POLICY tenant_isolation_delete ON tenant_channels
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS tenant_channels_master_ops_audit ON tenant_channels;
CREATE TRIGGER tenant_channels_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON tenant_channels
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- channel_access
-- (channel_id, user_id) grant. tenant_id is denormalized for index-backed
-- RLS. ON DELETE CASCADE on both FKs so removing a channel or a user
-- removes the dangling grants. UNIQUE(channel_id, user_id) makes the
-- backfill grant-all idempotent and prevents duplicate grants.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS channel_access (
  id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel_id  uuid NOT NULL REFERENCES tenant_channels(id) ON DELETE CASCADE,
  user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  created_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT channel_access_channel_user_uniq
    UNIQUE (channel_id, user_id)
);

CREATE INDEX IF NOT EXISTS channel_access_tenant_id_idx
  ON channel_access (tenant_id);
CREATE INDEX IF NOT EXISTS channel_access_user_id_idx
  ON channel_access (user_id);

ALTER TABLE channel_access OWNER TO app_admin;
ALTER TABLE channel_access ENABLE ROW LEVEL SECURITY;
ALTER TABLE channel_access FORCE ROW LEVEL SECURITY;

REVOKE ALL ON channel_access FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_access TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON channel_access TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON channel_access;
CREATE POLICY tenant_isolation_select ON channel_access
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON channel_access;
CREATE POLICY tenant_isolation_insert ON channel_access
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON channel_access;
CREATE POLICY tenant_isolation_update ON channel_access
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON channel_access;
CREATE POLICY tenant_isolation_delete ON channel_access
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS channel_access_master_ops_audit ON channel_access;
CREATE TRIGGER channel_access_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON channel_access
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- conversation.channel_id
-- Nullable so the column can be added without rewriting every existing
-- row up front; the backfill below populates it. The legacy `channel`
-- text column is intentionally KEPT for one-release overlap.
-- ---------------------------------------------------------------------------
ALTER TABLE conversation
  ADD COLUMN IF NOT EXISTS channel_id uuid REFERENCES tenant_channels(id);

CREATE INDEX IF NOT EXISTS conversation_channel_id_idx
  ON conversation (channel_id);

-- ---------------------------------------------------------------------------
-- Backfill (zero inbox regression)
-- 1. Materialize one tenant_channels row per (tenant, distinct existing
--    conversation.channel) with external_id = '' (placeholder), the
--    legacy string as both channel_key and display_name, active and not
--    restricted.
-- 2. Point each conversation at its freshly created channel instance.
-- 3. Grant every current tenant user access to every backfilled channel
--    so the inbox keeps showing the same conversations it did before.
-- Runs as app_admin (BYPASSRLS), so it spans all tenants in one pass; the
-- master_ops_audit_trigger no-ops for non-app_master_ops roles.
-- ---------------------------------------------------------------------------
INSERT INTO tenant_channels (tenant_id, channel_key, external_id, display_name, is_active, restricted)
SELECT DISTINCT c.tenant_id, c.channel, '', c.channel, true, false
FROM conversation c
WHERE c.channel IS NOT NULL
  AND c.channel <> ''
ON CONFLICT (tenant_id, channel_key, external_id) DO NOTHING;

UPDATE conversation c
SET channel_id = tc.id
FROM tenant_channels tc
WHERE tc.tenant_id = c.tenant_id
  AND tc.channel_key = c.channel
  AND tc.external_id = ''
  AND c.channel_id IS NULL;

INSERT INTO channel_access (tenant_id, channel_id, user_id)
SELECT tc.tenant_id, tc.id, u.id
FROM tenant_channels tc
JOIN users u ON u.tenant_id = tc.tenant_id
ON CONFLICT (channel_id, user_id) DO NOTHING;

COMMIT;
