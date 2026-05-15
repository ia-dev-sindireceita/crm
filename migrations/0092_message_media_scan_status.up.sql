-- 0092_message_media_scan_status.up.sql
-- SIN-62801 / SIN-62788 F2-05a: persist the async media-scan verdict
-- for each `message` row.
--
-- The upload path writes the initial value at message insert time
-- with scan_status="pending"; the mediascan-worker (SIN-62788 F2-05c)
-- patches the same row to "clean" or "infected" once a MediaScanner
-- adapter (e.g. ClamAV, SIN-62788 F2-05b) returns a verdict.
--
-- Shape of the JSON value (matches the Go port in
-- internal/media/scanner — Status enum + ScanResult):
--
--   {
--     "key":         "media/<tenant>/<yyyy-mm>/<hash>.<ext>",
--     "scan_status": "pending" | "clean" | "infected",
--     "scan_engine": "clamav-1.4.2"        -- only set after a verdict
--   }
--
-- We use jsonb (not a discrete column) on purpose: the per-message
-- media payload is going to grow (quarantine bucket, hide flag, EXIF
-- summary, etc. — SIN-62788 F2-05d and beyond) and pinning every
-- field as its own column locks us into a schema-migration per
-- field. `media` is nullable: text-only messages still have no media
-- row.
--
-- Idempotent (ADD COLUMN IF NOT EXISTS). Safe to re-run.
-- Reversible: see 0092_message_media_scan_status.down.sql.

BEGIN;

ALTER TABLE message ADD COLUMN IF NOT EXISTS media jsonb;

COMMENT ON COLUMN message.media IS
  'Media scan result, NULL for text-only messages. Shape: '
  '{"key":"media/<tenant>/<uuid>","scan_status":"pending|clean|infected","scan_engine":"clamav-x.y.z"}. '
  'scan_status enum is defined in internal/media/scanner.Status (SIN-62801).';

COMMIT;
