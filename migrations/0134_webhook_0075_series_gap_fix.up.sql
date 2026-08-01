-- 0134_webhook_0075_series_gap_fix.up.sql
--
-- Root cause: golang-migrate's source/iofs filename parser only accepts a
-- purely-numeric version prefix (regexp `^([0-9]+)_...`). The upstream
-- ADR-0075 chain — 0075a_webhook_tokens, 0075a2_tenant_channel_associations,
-- 0075b_webhook_idempotency, 0075c_raw_event, 0075d_gc_jobs — uses
-- alphanumeric prefixes, so the real CLI (migrate/migrate:v4.17.1, pinned in
-- Makefile and deploy/scripts/stg-deploy.sh) silently skips all five files
-- during `migrate up`: no error, no "dirty" flag, schema_migrations simply
-- never learns they exist. Only the project's own integration-test harness
-- (internal/webhook/integration/harness_test.go), which lexically sorts
-- *.up.sql including letters, ever actually ran them — which is why CI
-- looked green while every real deploy has zero of these five objects and
-- inbound WhatsApp messages were being dropped at the tenant-resolution
-- step (`whatsapp.unknown_phone_number_id` / dropped_tenant) despite a
-- correctly configured phone_number_id.
--
-- Per ADR 0086, upstream-authored migration files are never renamed or
-- renumbered — this file does NOT touch 0075a/0075a2/0075b/0075c/0075d.
-- Instead it re-creates the same objects under a purely-numeric name so
-- golang-migrate actually applies them (idempotent CREATE ... IF NOT
-- EXISTS / CREATE OR REPLACE, safe to run whether or not the 0075x bodies
-- ever get fixed upstream), and adds the GRANTs to app_runtime that the
-- upstream files omitted — without those, the tables exist but every
-- runtime query against them fails with "permission denied" instead of
-- working.

-- ---- from 0075a_webhook_tokens.up.sql ----
CREATE TABLE IF NOT EXISTS webhook_tokens (
    id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    token_hash      bytea       NOT NULL,
    overlap_minutes int         NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    revoked_at      timestamptz,
    last_used_at    timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS webhook_tokens_active_idx
    ON webhook_tokens (channel, token_hash)
    WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS webhook_tokens_tenant_idx
    ON webhook_tokens (tenant_id);

GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_tokens TO app_runtime;

-- ---- from 0075a2_tenant_channel_associations.up.sql ----
CREATE TABLE IF NOT EXISTS tenant_channel_associations (
    tenant_id   uuid        NOT NULL,
    channel     text        NOT NULL,
    association text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (channel, association)
);

CREATE INDEX IF NOT EXISTS tenant_channel_associations_tenant_idx
    ON tenant_channel_associations (tenant_id, channel);

GRANT SELECT, INSERT, UPDATE, DELETE ON tenant_channel_associations TO app_runtime;

-- ---- from 0075b_webhook_idempotency.up.sql ----
CREATE TABLE IF NOT EXISTS webhook_idempotency (
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    idempotency_key bytea       NOT NULL,
    inserted_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (tenant_id, channel, idempotency_key)
);

CREATE INDEX IF NOT EXISTS webhook_idempotency_gc_idx
    ON webhook_idempotency (inserted_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_idempotency TO app_runtime;

-- ---- from 0075c_raw_event.up.sql ----
CREATE TABLE IF NOT EXISTS raw_event (
    id              uuid        NOT NULL DEFAULT gen_random_uuid(),
    tenant_id       uuid        NOT NULL,
    channel         text        NOT NULL,
    idempotency_key bytea       NOT NULL,
    raw_payload     bytea       NOT NULL,
    headers         jsonb       NOT NULL,
    received_at     timestamptz NOT NULL DEFAULT now(),
    published_at    timestamptz,
    PRIMARY KEY (received_at, id)
) PARTITION BY RANGE (received_at);

CREATE INDEX IF NOT EXISTS raw_event_unpublished_idx
    ON raw_event (received_at)
    WHERE published_at IS NULL;

DO $$
DECLARE
    today_start  date := (now() AT TIME ZONE 'UTC')::date;
    today_end    date := today_start + INTERVAL '1 day';
    tomorrow_end date := today_start + INTERVAL '2 days';
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS raw_event_%s PARTITION OF raw_event '
        'FOR VALUES FROM (%L) TO (%L)',
        to_char(today_start, 'YYYYMMDD'),
        today_start,
        today_end
    );
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS raw_event_%s PARTITION OF raw_event '
        'FOR VALUES FROM (%L) TO (%L)',
        to_char(today_end::date, 'YYYYMMDD'),
        today_end,
        tomorrow_end
    );
END $$;

GRANT SELECT, INSERT, UPDATE, DELETE ON raw_event TO app_runtime;

-- ---- from 0075d_gc_jobs.up.sql ----
CREATE OR REPLACE FUNCTION webhook_gc_idempotency(retention interval)
RETURNS bigint
LANGUAGE sql
SECURITY INVOKER
AS $$
    WITH deleted AS (
        DELETE FROM webhook_idempotency
         WHERE inserted_at < now() - retention
        RETURNING 1
    )
    SELECT COUNT(*) FROM deleted;
$$;

CREATE OR REPLACE FUNCTION webhook_drop_raw_event_partition(part_name text)
RETURNS void
LANGUAGE plpgsql
SECURITY INVOKER
AS $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_class WHERE relname = part_name) THEN
        EXECUTE format('DROP TABLE IF EXISTS %I', part_name);
    END IF;
END;
$$;

CREATE OR REPLACE FUNCTION webhook_create_raw_event_partition(part_date date)
RETURNS void
LANGUAGE plpgsql
SECURITY INVOKER
AS $$
DECLARE
    part_name text := format('raw_event_%s', to_char(part_date, 'YYYYMMDD'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF raw_event FOR VALUES FROM (%L) TO (%L)',
        part_name,
        part_date,
        part_date + INTERVAL '1 day'
    );
END;
$$;

GRANT EXECUTE ON FUNCTION webhook_gc_idempotency(interval) TO app_runtime;
GRANT EXECUTE ON FUNCTION webhook_drop_raw_event_partition(text) TO app_runtime;
GRANT EXECUTE ON FUNCTION webhook_create_raw_event_partition(date) TO app_runtime;
