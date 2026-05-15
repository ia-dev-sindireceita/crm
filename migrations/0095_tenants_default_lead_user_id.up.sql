-- 0095_tenants_default_lead_user_id.up.sql
-- Fase 2 F2-07.2 (SIN-62833): tenant-level default lead user for the
-- inbox auto-attribution policy. When a new Conversation is created on
-- an inbound event, the use-case (internal/inbox/usecase/receive_inbound)
-- consults this column and, if non-null, appends an assignment_history
-- row with reason='lead' so the conversation lands on the configured
-- operator. NULL keeps the legacy behaviour ("sem líder" in the UI).
--
-- The `tenants` table is the registry of tenants and is intentionally
-- NOT tenant-scoped (no tenant_id column, no RLS — see migration 0004).
-- Adding a column therefore does NOT require RLS policy changes.
--
-- FK semantics:
--   * default_lead_user_id REFERENCES users(id) ON DELETE SET NULL
--     so removing a user reverts the tenant to "no default" rather
--     than blocking the delete. The application enforces (separately)
--     that the referenced user belongs to the same tenant; the database
--     cannot express that constraint here because users carry the
--     tenant_id and tenants does not.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS default_lead_user_id uuid
  REFERENCES users(id) ON DELETE SET NULL;

-- Sparse index: only the populated rows participate. Hot read pattern
-- is "given a tenant id, what's the default lead user?" served straight
-- off the tenants PK; this index exists for the reverse direction
-- (given a user being deleted/inspected, which tenants point at them).
CREATE INDEX IF NOT EXISTS tenants_default_lead_user_idx
  ON tenants (default_lead_user_id)
  WHERE default_lead_user_id IS NOT NULL;

COMMIT;
