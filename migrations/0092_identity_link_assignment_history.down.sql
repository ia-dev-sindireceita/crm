-- 0092_identity_link_assignment_history.down.sql
-- Reverse of 0092_identity_link_assignment_history.up.sql (SIN-62790).
--
-- Drop order: contact_identity_link first (it references identity AND
-- contact), then assignment_history (references conversation + users
-- only), then identity (no inbound FKs after the link table is gone).
-- IF EXISTS keeps the file idempotent so re-running is a no-op.
--
-- Run as app_admin. Idempotent.

BEGIN;

DROP TRIGGER IF EXISTS contact_identity_link_master_ops_audit ON contact_identity_link;
DROP TABLE IF EXISTS contact_identity_link;

DROP TRIGGER IF EXISTS assignment_history_master_ops_audit ON assignment_history;
DROP TABLE IF EXISTS assignment_history;

DROP TRIGGER IF EXISTS identity_master_ops_audit ON identity;
DROP TABLE IF EXISTS identity;

COMMIT;
