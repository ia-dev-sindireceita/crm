-- 0093_funnel_stage_transition.down.sql
-- Reverse of 0093_funnel_stage_transition.up.sql (SIN-62787).
--
-- Drop order: tenants-seed trigger first (it depends on the seed function),
-- then funnel_transition (it references funnel_stage), then funnel_stage,
-- then the helper functions. IF EXISTS keeps every step idempotent so the
-- file is safe to run twice in a row.
--
-- Run as app_admin. Idempotent.

BEGIN;

DROP TRIGGER IF EXISTS tenants_seed_funnel_stages ON tenants;
DROP FUNCTION IF EXISTS seed_default_funnel_stages_trigger();

DROP TRIGGER IF EXISTS funnel_transition_master_ops_audit ON funnel_transition;
DROP TABLE IF EXISTS funnel_transition;

DROP TRIGGER IF EXISTS funnel_stage_master_ops_audit ON funnel_stage;
DROP TABLE IF EXISTS funnel_stage;

DROP FUNCTION IF EXISTS seed_default_funnel_stages(uuid);

COMMIT;
