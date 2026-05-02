-- 0010_master_grants.up.sql
-- SIN-62241 / F39 — master_grants table.
-- Caps are enforced in the domain (MasterGrantPolicy), not in DB constraints.
-- This schema only persists facts; the rolling-window aggregates are
-- computed by the Postgres adapter via window-restricted SUMs.
--
-- Schema-only: ownership/role grants and the master_ops_audit_trigger
-- attachment land in a follow-up adapter migration when the Postgres adapter
-- is wired (see SIN-62195 — Subscription + Master grants Fase 2.5).

BEGIN;

CREATE TABLE IF NOT EXISTS master_grants (
    id              TEXT PRIMARY KEY,
    master_id       TEXT NOT NULL,
    tenant_id       TEXT NOT NULL,
    subscription_id TEXT NOT NULL,
    amount          BIGINT NOT NULL CHECK (amount > 0),
    reason          TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN (
        'granted', 'pending_approval', 'approved', 'cancelled'
    )),
    created_at      TIMESTAMPTZ NOT NULL,
    decided_by      TEXT NULL,
    decided_at      TIMESTAMPTZ NULL
);

-- Window-sum lookups by subscription (90 day rolling) and master (365 day
-- rolling) are the hot paths; these indexes keep them sub-millisecond at
-- expected volumes.
CREATE INDEX IF NOT EXISTS idx_master_grants_subscription_window
    ON master_grants (subscription_id, created_at DESC, status);

CREATE INDEX IF NOT EXISTS idx_master_grants_master_window
    ON master_grants (master_id, created_at DESC, status);

-- Audit log for every grant attempt (granted, denied, pending, approved,
-- cancelled, alert_emitted, validation_failed). Used by SIN-62192 audit
-- infra; the request payload is stored verbatim for compliance.
CREATE TABLE IF NOT EXISTS master_grants_audit_log (
    id              BIGSERIAL PRIMARY KEY,
    kind            TEXT NOT NULL,
    occurred_at     TIMESTAMPTZ NOT NULL,
    grant_id        TEXT NULL,
    principal       TEXT NOT NULL,
    ip_address      TEXT NOT NULL,
    request_payload JSONB NOT NULL,
    decision_payload JSONB NULL,
    note            TEXT NULL
);

CREATE INDEX IF NOT EXISTS idx_master_grants_audit_grant_id
    ON master_grants_audit_log (grant_id);
CREATE INDEX IF NOT EXISTS idx_master_grants_audit_principal_at
    ON master_grants_audit_log (principal, occurred_at DESC);

COMMIT;
