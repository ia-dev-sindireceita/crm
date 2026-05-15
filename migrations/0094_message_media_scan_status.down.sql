-- 0094_message_media_scan_status.down.sql
-- Reverse of 0094_message_media_scan_status.up.sql.
--
-- Drops `message.media` and the COMMENT attached to it (the COMMENT
-- is implicitly removed with the column). This DOES discard the
-- persisted scan verdicts — the worker can repopulate them by
-- re-publishing media.scan.requested for affected messages.
--
-- Idempotent (DROP COLUMN IF EXISTS). Safe to re-run.

BEGIN;

ALTER TABLE message DROP COLUMN IF EXISTS media;

COMMIT;
