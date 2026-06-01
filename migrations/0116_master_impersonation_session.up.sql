-- 0116_master_impersonation_session.up.sql
-- SIN-63958 / master-impersonation-spec §1.2.
--
-- Session-bound, 15-minute, server-authoritative impersonation envelope.
-- The row records a master operator's active tenant-impersonation under a
-- specific master_session, the human-supplied reason (CHECK-enforced
-- length 8..500), the server-computed expires_at, and the end-of-session
-- bookkeeping (ended_at + ended_reason).
--
-- Unique partial index `one_active_per_master_session` enforces the
-- spec's invariant: a master_session may carry at most one active
-- (ended_at IS NULL) impersonation row. Concurrent /start calls from
-- the same master session resolve to a 409 at the handler via the
-- UniqueViolation → ErrAlreadyActive mapping (spec §1.4 / §5.5 item 10).
--
-- Lives in the master_ops scope: app_master_ops owns the table, the
-- master_ops_audit_trigger writes one master_ops_audit row per change,
-- and app_runtime never sees the rows. The audit correlation in 0117
-- writes audit_log_security.correlation_id (FK to this PK) for every
-- authz event fired while an impersonation is active.
--
-- Run as app_admin. Idempotent.

BEGIN;

CREATE TABLE IF NOT EXISTS master_impersonation_session (
  id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  master_user_id     uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  master_session_id  uuid NOT NULL REFERENCES master_session(id) ON DELETE CASCADE,
  target_tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  reason             text NOT NULL CHECK (length(reason) BETWEEN 8 AND 500),
  started_at         timestamptz NOT NULL DEFAULT now(),
  expires_at         timestamptz NOT NULL,
  ended_at           timestamptz,
  ended_reason       text
);

-- At most one active impersonation per master_session. Concurrent
-- /start calls from the same master_session collapse to a
-- UniqueViolation at INSERT time → ErrAlreadyActive → 409 at the
-- handler (spec §5.5 #10).
CREATE UNIQUE INDEX IF NOT EXISTS one_active_per_master_session
  ON master_impersonation_session (master_session_id)
  WHERE ended_at IS NULL;

-- Hot read paths: (target_tenant_id, started_at DESC) supports the
-- per-tenant audit panel; (master_user_id, started_at DESC) supports
-- the per-operator history view.
CREATE INDEX IF NOT EXISTS master_impersonation_session_tenant_started_idx
  ON master_impersonation_session (target_tenant_id, started_at DESC);

CREATE INDEX IF NOT EXISTS master_impersonation_session_master_user_started_idx
  ON master_impersonation_session (master_user_id, started_at DESC);

ALTER TABLE master_impersonation_session OWNER TO app_admin;

REVOKE ALL ON master_impersonation_session FROM PUBLIC;
REVOKE ALL ON master_impersonation_session FROM app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON master_impersonation_session TO app_master_ops;

DROP TRIGGER IF EXISTS master_impersonation_session_master_ops_audit ON master_impersonation_session;
CREATE TRIGGER master_impersonation_session_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON master_impersonation_session
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

COMMIT;
