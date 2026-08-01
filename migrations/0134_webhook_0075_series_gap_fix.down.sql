-- 0134_webhook_0075_series_gap_fix.down.sql — rollback.
-- Mirrors 0075a/0075a2/0075b/0075c/0075d down.sql combined. DROP TABLE on
-- raw_event cascades to its date partitions.
DROP FUNCTION IF EXISTS webhook_create_raw_event_partition(date);
DROP FUNCTION IF EXISTS webhook_drop_raw_event_partition(text);
DROP FUNCTION IF EXISTS webhook_gc_idempotency(interval);

DROP INDEX IF EXISTS raw_event_unpublished_idx;
DROP TABLE IF EXISTS raw_event;

DROP INDEX IF EXISTS webhook_idempotency_gc_idx;
DROP TABLE IF EXISTS webhook_idempotency;

DROP INDEX IF EXISTS tenant_channel_associations_tenant_idx;
DROP TABLE IF EXISTS tenant_channel_associations;

DROP INDEX IF EXISTS webhook_tokens_active_idx;
DROP INDEX IF EXISTS webhook_tokens_tenant_idx;
DROP TABLE IF EXISTS webhook_tokens;
