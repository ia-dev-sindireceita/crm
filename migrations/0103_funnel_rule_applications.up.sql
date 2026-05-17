-- 0103_funnel_rule_applications.up.sql
--
-- SIN-62960 — Fase 4 funnel rule engine. Adds the idempotency ledger
-- the NATS consumer writes after applying a rule's action so a
-- redelivery (or a parallel delivery from another worker replica) does
-- not double-apply the same (rule_id, message_id) pair.
--
-- Schema choices:
--
--   * id is a separate uuid PK so the row is referenced by id in audit
--     and pagination paths; the dedup contract is enforced by the
--     UNIQUE (rule_id, message_id) constraint, not by the primary key.
--   * tenant_id is denormalized for the canonical four-policy RLS
--     template (docs/adr/0072-rls-policies.md). Index-backed by the
--     primary key + (tenant_id, applied_at) for the operator console.
--   * rule_id is a hard FK to funnel_rules so cascade-delete on rule
--     removal also drops the ledger entries. message_id is intentionally
--     NOT a FK to message because the inbox aggregate's message rows
--     can be archived independently; the ledger only needs the uuid
--     for the dedup check.
--   * action_type duplicates funnel_rules.action_type at apply time so
--     a future rule mutation (e.g. swapping move_to_stage for a
--     hypothetical send_template) does not retro-actively rewrite the
--     ledger. The column is freeform text to match funnel_rules.
--   * applied_at is the wall-clock moment the consumer recorded the
--     application (post action success). It is what the metric
--     `funnel_evaluation_latency_seconds` measures against the
--     event's occurred_at.
--
-- RLS posture matches funnel_rules and the rest of the Fase 4
-- migrations: tenant_isolation_select/insert/update/delete for
-- app_runtime; app_master_ops gets full access without a tenant filter
-- so the master console can audit cross-tenant.

CREATE TABLE IF NOT EXISTS funnel_rule_applications (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id        uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  rule_id          uuid NOT NULL REFERENCES funnel_rules(id) ON DELETE CASCADE,
  message_id       uuid NOT NULL,
  conversation_id  uuid NOT NULL,
  action_type      text NOT NULL,
  applied_at       timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT funnel_rule_applications_rule_message_uniq
    UNIQUE (rule_id, message_id)
);

CREATE INDEX IF NOT EXISTS funnel_rule_applications_tenant_applied_idx
  ON funnel_rule_applications (tenant_id, applied_at DESC);
CREATE INDEX IF NOT EXISTS funnel_rule_applications_message_idx
  ON funnel_rule_applications (tenant_id, message_id);

ALTER TABLE funnel_rule_applications OWNER TO app_admin;
ALTER TABLE funnel_rule_applications ENABLE ROW LEVEL SECURITY;
ALTER TABLE funnel_rule_applications FORCE ROW LEVEL SECURITY;

REVOKE ALL ON funnel_rule_applications FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_rule_applications TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_rule_applications TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON funnel_rule_applications;
CREATE POLICY tenant_isolation_select ON funnel_rule_applications
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON funnel_rule_applications;
CREATE POLICY tenant_isolation_insert ON funnel_rule_applications
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON funnel_rule_applications;
CREATE POLICY tenant_isolation_update ON funnel_rule_applications
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON funnel_rule_applications;
CREATE POLICY tenant_isolation_delete ON funnel_rule_applications
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS funnel_rule_applications_master_ops_audit ON funnel_rule_applications;
CREATE TRIGGER funnel_rule_applications_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON funnel_rule_applications
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();
