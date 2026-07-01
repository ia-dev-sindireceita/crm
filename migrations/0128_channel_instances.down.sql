-- 0128_channel_instances.down.sql
-- Reverse of 0128_channel_instances.up.sql (SIN-66389).
--
-- Drop order: drop the conversation.channel_id FK column FIRST so the
-- tenant_channels table it references can be dropped, then channel_access
-- (references tenant_channels) before tenant_channels itself. The legacy
-- conversation.channel text column is left intact — it was never dropped
-- by the up migration, so reversing must not touch it.
--
-- IF EXISTS makes this safe to run twice. Triggers are dropped explicitly
-- (belt and braces — DROP TABLE removes its own triggers).
--
-- Run as app_admin. Idempotent.

BEGIN;

DROP INDEX IF EXISTS conversation_channel_id_idx;
ALTER TABLE conversation DROP COLUMN IF EXISTS channel_id;

DROP TRIGGER IF EXISTS channel_access_master_ops_audit ON channel_access;
DROP TABLE IF EXISTS channel_access;

DROP TRIGGER IF EXISTS tenant_channels_master_ops_audit ON tenant_channels;
DROP TABLE IF EXISTS tenant_channels;

COMMIT;
