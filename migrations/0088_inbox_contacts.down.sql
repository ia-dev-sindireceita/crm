-- 0088_inbox_contacts.down.sql
-- Reverse of 0088_inbox_contacts.up.sql (SIN-62724).
--
-- Drop order matters: child tables before parents because of FK
-- constraints. assignment + message reference conversation; conversation
-- references contact; contact_channel_identity references contact.
-- inbound_message_dedup has no inbound FKs so it can be dropped at any
-- time, but we keep it last to mirror the up-order in reverse.
--
-- IF EXISTS makes this safe to run twice. Triggers are dropped explicitly
-- so an idempotent re-run does not leave orphan trigger objects pointing
-- at dropped tables (DROP TABLE removes its own triggers; this is belt
-- and braces).
--
-- Run as app_admin. Idempotent.

BEGIN;

DROP TRIGGER IF EXISTS inbound_message_dedup_master_ops_audit ON inbound_message_dedup;
DROP TABLE IF EXISTS inbound_message_dedup;

DROP TRIGGER IF EXISTS assignment_master_ops_audit ON assignment;
DROP TABLE IF EXISTS assignment;

DROP TRIGGER IF EXISTS message_master_ops_audit ON message;
DROP TABLE IF EXISTS message;

DROP TRIGGER IF EXISTS conversation_master_ops_audit ON conversation;
DROP TABLE IF EXISTS conversation;

DROP TRIGGER IF EXISTS contact_channel_identity_master_ops_audit ON contact_channel_identity;
DROP TABLE IF EXISTS contact_channel_identity;

DROP TRIGGER IF EXISTS contact_master_ops_audit ON contact;
DROP TABLE IF EXISTS contact;

COMMIT;
