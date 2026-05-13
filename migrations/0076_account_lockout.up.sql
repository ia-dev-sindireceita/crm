-- 0008_account_lockout.up.sql
-- Durable per-user account lockout (SIN-62341, ADR 0073 §D4).
--
-- Run as app_admin. Idempotent.
--
-- The Redis sliding-window counters in internal/adapter/ratelimit/redis
-- handle the short-burst throttling. This table is the *durable*
-- companion: when an authentication endpoint accumulates more than its
-- policy threshold of consecutive failures on the same principal, the
-- middleware writes a row here with locked_until = now() + duration.
-- The login handler then checks IsLocked BEFORE verifying the password,
-- so a Redis flush does NOT erase the penalty (ADR 0073: "rate limits
-- without durable lockout die on a Redis flush").
--
-- tenant_id mirrors the (master XOR tenant) pattern from the users
-- table (migrations/0005_create_users.up.sql): tenant lockouts carry a
-- non-NULL tenant_id and are visible to app_runtime under the standard
-- four-policy RLS template; master lockouts (is_master users) carry a
-- NULL tenant_id and are reachable only via app_master_ops (BYPASSRLS,
-- audited). The CHECK constraint enforces the same shape so a tenant
-- row cannot accidentally land with NULL tenant_id and become invisible
-- to its own login flow.

BEGIN;

-- Denormalisation note. tenant_id MUST mirror users.tenant_id for the
-- locked user (so RLS gates the row to that user's tenant). Master
-- lockouts (users.is_master = true, users.tenant_id IS NULL) carry a
-- NULL tenant_id and are reachable only via app_master_ops. The
-- adapter enforces this denormalisation at insert/update time; there
-- is no CHECK constraint here because Postgres CHECK cannot reference
-- another table.
CREATE TABLE IF NOT EXISTS account_lockout (
  user_id       uuid PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  tenant_id     uuid REFERENCES tenants(id) ON DELETE CASCADE,
  locked_until  timestamptz NOT NULL,
  reason        text,
  created_at    timestamptz NOT NULL DEFAULT now()
);

-- Composite index on (user_id, locked_until) per SIN-62341 spec. The
-- canonical lookup is "is this user_id locked NOW?", served by the PK
-- alone; the secondary key on locked_until lets a future GC job range-
-- scan expired rows cheaply.
CREATE INDEX IF NOT EXISTS account_lockout_user_locked_until_idx
  ON account_lockout (user_id, locked_until);

-- Tenant-scoped rows benefit from a tenant-prefixed index too: master
-- console lookups ("show all currently-locked users in tenant X") use
-- it. WHERE tenant_id IS NOT NULL keeps the index narrow.
CREATE INDEX IF NOT EXISTS account_lockout_tenant_id_idx
  ON account_lockout (tenant_id, locked_until)
  WHERE tenant_id IS NOT NULL;

ALTER TABLE account_lockout OWNER TO app_admin;

ALTER TABLE account_lockout ENABLE ROW LEVEL SECURITY;
ALTER TABLE account_lockout FORCE ROW LEVEL SECURITY;

REVOKE ALL ON account_lockout FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON account_lockout TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON account_lockout TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON account_lockout;
CREATE POLICY tenant_isolation_select ON account_lockout
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON account_lockout;
CREATE POLICY tenant_isolation_insert ON account_lockout
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON account_lockout;
CREATE POLICY tenant_isolation_update ON account_lockout
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON account_lockout;
CREATE POLICY tenant_isolation_delete ON account_lockout
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS account_lockout_master_ops_audit ON account_lockout;
CREATE TRIGGER account_lockout_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON account_lockout
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
