-- 0102_phase4_marketing_billing_dunning.down.sql
-- Reverse of 0102: drop the seven Fase 4 base tables in reverse FK order.
--
-- token_packages, webhook_events: no FK dependents in this migration.
-- subscription_dunning_states, pix_charges, funnel_rules, campaign_clicks,
-- campaigns: dropped in reverse declaration order.
--
-- The DROP TABLE … CASCADE form cleans up the per-table policies,
-- triggers, and indexes without an explicit drop list. The migration
-- created no shared functions, sequences, or types — only tables — so
-- the CASCADE blast radius stays contained.
--
-- Idempotent: IF EXISTS guards let the down migration replay safely.
-- Run as app_admin.

BEGIN;

DROP TABLE IF EXISTS token_packages CASCADE;
DROP TABLE IF EXISTS webhook_events CASCADE;
DROP TABLE IF EXISTS subscription_dunning_states CASCADE;
DROP TABLE IF EXISTS pix_charges CASCADE;
DROP TABLE IF EXISTS funnel_rules CASCADE;
DROP TABLE IF EXISTS campaign_clicks CASCADE;
DROP TABLE IF EXISTS campaigns CASCADE;

COMMIT;
