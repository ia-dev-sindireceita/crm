-- 0102_phase4_marketing_billing_dunning.up.sql
-- Fase 4 / SIN-62952: base schema for marketing/billing/dunning surfaces.
--
-- Ships seven tables in one migration because they form a single deployable
-- feature unit for [Fase 4 — SIN-62197](issue tracker):
--
--   * campaigns                    — per-tenant UTM-tagged short links.
--   * campaign_clicks              — global click ledger (one row per
--                                    unique click_id) feeding funnel
--                                    triggers and reporting.
--   * funnel_rules                 — per-tenant automation rules
--                                    (trigger → action). Replaces the
--                                    hard-coded auto-handoffs of 0093.
--   * pix_charges                  — PSP-issued PIX charges paired with
--                                    one of our invoices.
--   * subscription_dunning_states  — per-subscription dunning state
--                                    machine (current → warn → … →
--                                    cancelled). One row per
--                                    subscription.
--   * webhook_events               — global idempotency ledger for the
--                                    PSP reconciler (Fase 4 C13).
--   * token_packages               — global catalogue of one-shot token
--                                    bundles ratified in D4 (Small/
--                                    Medium/Large).
--
-- Numbering: ADR-0086 fork-only numbering. Last fork-only migration on
-- main is 0101_ai_policy_consent (SIN-62929 — landed concurrently with
-- this PR's review window, which forced a renumber from 0101 to 0102).
-- This is 0102_*.
--
-- All tenant-scoped tables follow the canonical four-policy RLS template
-- from docs/adr/0072-rls-policies.md. tenant_id is denormalized onto
-- every child table so policy USING/WITH CHECK clauses are
-- index-backed.
--
-- Deviations from the SIN-62952 issue spec (engineering calls, documented
-- here so a reader of the migration does not have to bounce to the
-- ticket):
--
--   * funnel_rules: spec text says "channel_id NULLABLE". The CRM has no
--     channels table — channels are textual identifiers ('whatsapp',
--     'webchat', 'instagram', …). We use `channel text NULL` to match
--     codebase convention (cf. message.channel, conversation.channel).
--   * funnel_rules: spec text says "team_id NULLABLE". No teams table
--     exists yet. We add `team_id uuid NULL` without FK so the column
--     reserves the slot for the future teams table; the FK can be
--     tightened in a later migration without changing the column type.
--   * campaign_clicks: spec omits tenant_id. We denormalize tenant_id
--     NOT NULL + enable RLS because every campaign already belongs to a
--     tenant — leaving clicks tenant-blind would force every reader to
--     join through campaigns AND trust the join, opening a leak surface.
--     The contact_id column stays NULLABLE for anonymous (pre-identified)
--     clicks.
--
-- Run as app_admin (BYPASSRLS required for policies + grants). Idempotent.

BEGIN;

-- ---------------------------------------------------------------------------
-- campaigns
-- Per-tenant UTM-tagged short links. slug is the URL component the
-- end-user clicks (e.g. /go/blackfriday-2026); UNIQUE per tenant so two
-- tenants can both own "blackfriday-2026" without colliding.
-- expires_at is NULLABLE — evergreen campaigns never expire; dated
-- promos have a hard end. The application enforces expiry at click time.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS campaigns (
  id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id     uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  name          text NOT NULL,
  slug          text NOT NULL,
  utm_source    text,
  utm_medium    text,
  utm_campaign  text,
  utm_term      text,
  utm_content   text,
  redirect_url  text NOT NULL,
  expires_at    timestamptz,
  created_at    timestamptz NOT NULL DEFAULT now(),
  updated_at    timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT campaigns_slug_per_tenant_uniq UNIQUE (tenant_id, slug)
);

CREATE INDEX IF NOT EXISTS campaigns_tenant_id_idx
  ON campaigns (tenant_id);

ALTER TABLE campaigns OWNER TO app_admin;
ALTER TABLE campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaigns FORCE ROW LEVEL SECURITY;

REVOKE ALL ON campaigns FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON campaigns TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON campaigns TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON campaigns;
CREATE POLICY tenant_isolation_select ON campaigns
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON campaigns;
CREATE POLICY tenant_isolation_insert ON campaigns
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON campaigns;
CREATE POLICY tenant_isolation_update ON campaigns
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON campaigns;
CREATE POLICY tenant_isolation_delete ON campaigns
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS campaigns_master_ops_audit ON campaigns;
CREATE TRIGGER campaigns_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON campaigns
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- campaign_clicks
-- One row per unique click. click_id is the public, browser-supplied
-- token used for idempotency (the redirect handler refuses to insert a
-- duplicate click_id, so a page reload does not double-count).
-- contact_id is NULLABLE because most clicks arrive before the visitor
-- identifies (links shared on social media land cold).
-- ip is INET so we can index by /24 for fraud heuristics later without a
-- schema change.
-- meta JSONB is an opaque per-source bag (Meta click ID, x-forwarded-for
-- chain, geoip lookup result, …); validation is at the application
-- boundary, not at the column level (forward-compat).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS campaign_clicks (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  campaign_id  uuid NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
  click_id     text NOT NULL UNIQUE,
  contact_id   uuid REFERENCES contact(id) ON DELETE SET NULL,
  ip           inet,
  user_agent   text,
  referrer     text,
  meta         jsonb NOT NULL DEFAULT '{}'::jsonb,
  created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS campaign_clicks_tenant_id_idx
  ON campaign_clicks (tenant_id);
CREATE INDEX IF NOT EXISTS campaign_clicks_campaign_created_idx
  ON campaign_clicks (campaign_id, created_at DESC);

ALTER TABLE campaign_clicks OWNER TO app_admin;
ALTER TABLE campaign_clicks ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaign_clicks FORCE ROW LEVEL SECURITY;

REVOKE ALL ON campaign_clicks FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON campaign_clicks TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON campaign_clicks TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON campaign_clicks;
CREATE POLICY tenant_isolation_select ON campaign_clicks
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON campaign_clicks;
CREATE POLICY tenant_isolation_insert ON campaign_clicks
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON campaign_clicks;
CREATE POLICY tenant_isolation_update ON campaign_clicks
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON campaign_clicks;
CREATE POLICY tenant_isolation_delete ON campaign_clicks
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS campaign_clicks_master_ops_audit ON campaign_clicks;
CREATE TRIGGER campaign_clicks_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON campaign_clicks
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- funnel_rules
-- Per-tenant trigger→action automation. trigger_config and action_config
-- are intentionally opaque JSONB so new trigger/action kinds can land
-- without a schema change. The application layer validates shape per
-- trigger_type / action_type pair.
--
-- Example rows (informational, not seeded):
--   trigger_type='message_contains', trigger_config={"phrase":"refund"}
--   action_type='move_to_stage',     action_config={"stage":"high-intent"}
--
-- channel: text identifier ('whatsapp', 'webchat', …), NULL = any channel.
-- team_id: forward-compat slot for the future teams table (no FK yet).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS funnel_rules (
  id              uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id       uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  channel         text,
  team_id         uuid,
  name            text NOT NULL,
  trigger_type    text NOT NULL,
  trigger_config  jsonb NOT NULL DEFAULT '{}'::jsonb,
  action_type     text NOT NULL,
  action_config   jsonb NOT NULL DEFAULT '{}'::jsonb,
  enabled         boolean NOT NULL DEFAULT true,
  created_at      timestamptz NOT NULL DEFAULT now(),
  updated_at      timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS funnel_rules_tenant_enabled_idx
  ON funnel_rules (tenant_id, enabled);
CREATE INDEX IF NOT EXISTS funnel_rules_tenant_trigger_idx
  ON funnel_rules (tenant_id, trigger_type)
  WHERE enabled = true;

ALTER TABLE funnel_rules OWNER TO app_admin;
ALTER TABLE funnel_rules ENABLE ROW LEVEL SECURITY;
ALTER TABLE funnel_rules FORCE ROW LEVEL SECURITY;

REVOKE ALL ON funnel_rules FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_rules TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON funnel_rules TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON funnel_rules;
CREATE POLICY tenant_isolation_select ON funnel_rules
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_insert ON funnel_rules;
CREATE POLICY tenant_isolation_insert ON funnel_rules
  FOR INSERT TO app_runtime
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_update ON funnel_rules;
CREATE POLICY tenant_isolation_update ON funnel_rules
  FOR UPDATE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid)
  WITH CHECK (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP POLICY IF EXISTS tenant_isolation_delete ON funnel_rules;
CREATE POLICY tenant_isolation_delete ON funnel_rules
  FOR DELETE TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS funnel_rules_master_ops_audit ON funnel_rules;
CREATE TRIGGER funnel_rules_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON funnel_rules
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- pix_charges
-- PSP-issued PIX charges paired with one of our invoices.
-- external_id is the PSP's charge identifier — NULLABLE because a charge
-- is created in our DB before the PSP responds (we own creation +
-- idempotency on our side via id). external_id is UNIQUE for
-- reconciliation; the partial UNIQUE allows multiple NULL rows during
-- the brief pre-ack window.
-- copy_paste is the BR Code (EMVCo) "PIX copia-e-cola" string; qr_code
-- is the base64-encoded PNG/SVG of the same code, served as data URL.
-- status: 'pending' → 'paid' or 'expired', or 'cancelled' (admin op).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS pix_charges (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  invoice_id   uuid NOT NULL REFERENCES invoice(id) ON DELETE CASCADE,
  external_id  text,
  qr_code      text NOT NULL,
  copy_paste   text NOT NULL,
  status       text NOT NULL DEFAULT 'pending'
                 CHECK (status IN ('pending','paid','expired','cancelled')),
  paid_at      timestamptz,
  expires_at   timestamptz NOT NULL,
  created_at   timestamptz NOT NULL DEFAULT now(),
  updated_at   timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT pix_charges_paid_at_consistency CHECK (
    (status = 'paid' AND paid_at IS NOT NULL)
    OR (status <> 'paid' AND paid_at IS NULL)
  )
);

-- Partial UNIQUE: external_id is unique when populated, but the brief
-- window between INSERT and PSP ack carries NULL — multiple NULL rows
-- are allowed.
CREATE UNIQUE INDEX IF NOT EXISTS pix_charges_external_id_uniq
  ON pix_charges (external_id)
  WHERE external_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS pix_charges_tenant_id_idx
  ON pix_charges (tenant_id);
CREATE INDEX IF NOT EXISTS pix_charges_invoice_id_idx
  ON pix_charges (invoice_id);
CREATE INDEX IF NOT EXISTS pix_charges_status_idx
  ON pix_charges (status)
  WHERE status = 'pending';

ALTER TABLE pix_charges OWNER TO app_admin;
ALTER TABLE pix_charges ENABLE ROW LEVEL SECURITY;
ALTER TABLE pix_charges FORCE ROW LEVEL SECURITY;

REVOKE ALL ON pix_charges FROM PUBLIC;
-- Tenants read their own PIX charges (so the billing UI can render the
-- QR / copy-paste); writes go through master_ops (the PSP integration
-- runs as master_ops, same as the renewer).
GRANT SELECT ON pix_charges TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON pix_charges TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON pix_charges;
CREATE POLICY tenant_isolation_select ON pix_charges
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS pix_charges_master_ops_audit ON pix_charges;
CREATE TRIGGER pix_charges_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON pix_charges
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- subscription_dunning_states
-- One row per subscription tracking the current dunning state.
-- States: current → warn → suspended_outbound → suspended_full → cancelled.
-- override_until lets master_ops grant a temporary reprieve (e.g.
-- payment confirmed manually) without losing the underlying state for
-- audit. override_reason ≥ 10 chars when set (defence-in-depth).
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS subscription_dunning_states (
  id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
  subscription_id   uuid NOT NULL UNIQUE REFERENCES subscription(id) ON DELETE CASCADE,
  state             text NOT NULL
                      CHECK (state IN ('current','warn','suspended_outbound','suspended_full','cancelled')),
  entered_state_at  timestamptz NOT NULL DEFAULT now(),
  last_invoice_id   uuid REFERENCES invoice(id) ON DELETE SET NULL,
  override_until    timestamptz,
  override_reason   text,
  CONSTRAINT subscription_dunning_states_override_consistency CHECK (
    (override_until IS NULL AND override_reason IS NULL)
    OR
    (override_until IS NOT NULL
       AND override_reason IS NOT NULL
       AND char_length(override_reason) >= 10)
  )
);

CREATE INDEX IF NOT EXISTS subscription_dunning_states_tenant_idx
  ON subscription_dunning_states (tenant_id);
-- Hot query: "subscriptions due to escalate" — partial index over the
-- non-terminal states (the dunning worker scan target). The override
-- filter cannot be part of the predicate (override_until is compared
-- to now(), which Postgres rejects as non-IMMUTABLE in index
-- predicates); callers add `AND (override_until IS NULL OR
-- override_until < now())` at query time.
CREATE INDEX IF NOT EXISTS subscription_dunning_states_active_idx
  ON subscription_dunning_states (entered_state_at)
  WHERE state IN ('warn','suspended_outbound');

ALTER TABLE subscription_dunning_states OWNER TO app_admin;
ALTER TABLE subscription_dunning_states ENABLE ROW LEVEL SECURITY;
ALTER TABLE subscription_dunning_states FORCE ROW LEVEL SECURITY;

REVOKE ALL ON subscription_dunning_states FROM PUBLIC;
-- Tenants read their own dunning state (to render the billing banner);
-- writes are owned by master_ops (the dunning worker runs as
-- master_ops; manual overrides are a master action).
GRANT SELECT ON subscription_dunning_states TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON subscription_dunning_states TO app_master_ops;

DROP POLICY IF EXISTS tenant_isolation_select ON subscription_dunning_states;
CREATE POLICY tenant_isolation_select ON subscription_dunning_states
  FOR SELECT TO app_runtime
  USING (tenant_id = current_setting('app.tenant_id', true)::uuid);

DROP TRIGGER IF EXISTS subscription_dunning_states_master_ops_audit
  ON subscription_dunning_states;
CREATE TRIGGER subscription_dunning_states_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON subscription_dunning_states
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- webhook_events
-- Global idempotency ledger for the PSP reconciler (Fase 4 C13). Same
-- shape rationale as inbound_message_dedup (0088): the webhook receiver
-- consults it BEFORE tenant context is fully resolved, so it is
-- intentionally NOT tenant-scoped and has no RLS.
--
-- UNIQUE (source, external_id, event_type) is the dedup key — the PSP
-- may retry the same delivery five times; the table guarantees we
-- process each (source, external_id, event_type) tuple exactly once.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS webhook_events (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  source       text NOT NULL,
  external_id  text NOT NULL,
  event_type   text NOT NULL,
  payload      jsonb NOT NULL,
  received_at  timestamptz NOT NULL DEFAULT now(),
  CONSTRAINT webhook_events_dedup_uniq UNIQUE (source, external_id, event_type)
);

CREATE INDEX IF NOT EXISTS webhook_events_received_idx
  ON webhook_events (received_at);
CREATE INDEX IF NOT EXISTS webhook_events_source_event_idx
  ON webhook_events (source, event_type, received_at DESC);

ALTER TABLE webhook_events OWNER TO app_admin;

REVOKE ALL ON webhook_events FROM PUBLIC;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_events TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON webhook_events TO app_master_ops;

DROP TRIGGER IF EXISTS webhook_events_master_ops_audit ON webhook_events;
CREATE TRIGGER webhook_events_master_ops_audit
  BEFORE INSERT OR UPDATE OR DELETE ON webhook_events
  FOR EACH ROW EXECUTE FUNCTION master_ops_audit_trigger();

-- ---------------------------------------------------------------------------
-- token_packages
-- Global catalogue of one-shot token bundles ratified in D4
-- (SIN-62207). Same shape rationale as `plan` (0097): non-tenanted
-- reference table curated by master_ops; tenants read it to render the
-- "buy more tokens" UI.
--
-- `kind` is a forward-compat slot: today only 'tokens' (the D4
-- ratification scope); future kinds (e.g. 'addon_seats',
-- 'priority_support') can land without a schema change. The seed file
-- in seed/token_packages.sql only writes kind='tokens' rows.
--
-- Markup math (D4, 2026-05-09 ratification):
--   Base cost assumption  : ~US$ 0.30 / 1M tokens (blend 70% Gemini
--                           Flash, 30% Haiku, ~30% output share).
--   FX cushion            : R$ 6.00 / USD → R$ 1.80 / 1M tokens.
--
--   | slug   | tokens | price R$ | gross cost R$ | effective markup |
--   | ------ | ------ | -------- | --------------- | ----------------- |
--   | small  | 1M     | R$ 15    | R$ 1.80         | ~8.3x  (premium small qty) |
--   | medium | 5M     | R$ 49    | R$ 9.00         | ~5.4x  (sweet spot)        |
--   | large  | 20M    | R$ 149   | R$ 36.00        | ~4.1x  (volume discount)   |
--
-- Premises that trigger a future revision (board sign-off needed):
--   1. FX outside R$ 5.50–6.50 / USD.
--   2. Model mix drifts beyond 70/30 Flash/Haiku.
--   3. Output share rises above ~40% of total volume.
-- ---------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS token_packages (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  slug             text NOT NULL UNIQUE,
  kind             text NOT NULL DEFAULT 'tokens',
  name             text NOT NULL,
  tokens           bigint NOT NULL CHECK (tokens > 0),
  price_cents_brl  integer NOT NULL CHECK (price_cents_brl >= 0),
  created_at       timestamptz NOT NULL DEFAULT now(),
  updated_at       timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE token_packages OWNER TO app_admin;

REVOKE ALL ON token_packages FROM PUBLIC;
-- Tenants read the catalogue (billing UI lists packages); master_ops
-- curates it.
GRANT SELECT ON token_packages TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON token_packages TO app_master_ops;

COMMIT;
