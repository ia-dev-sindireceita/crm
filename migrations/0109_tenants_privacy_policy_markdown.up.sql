-- 0109_tenants_privacy_policy_markdown.up.sql
-- SIN-63191 / Fase 6 PR4 (AC #3): public /privacy page needs the
-- markdown body of the currently-published policy plus a
-- last_updated stamp. PR3 (migration 0108) added the version + URL
-- pointers on `tenants`; this migration completes the surface so the
-- public page can render the policy text inline without a second
-- network hop to privacy_policy_url.
--
-- Columns:
--   * privacy_policy_markdown   — full Markdown body of the
--                                  currently-published privacy policy.
--                                  Rendered server-side via goldmark
--                                  + bluemonday-equivalent sanitiser
--                                  (yuin/goldmark, already in go.mod).
--   * privacy_policy_updated_at — wall-clock stamp the master operator
--                                  rolled the policy to its current
--                                  version. Public page renders the
--                                  ISO-8601 form so visitors can audit
--                                  policy freshness without fetching
--                                  the changelog.
--
-- Both columns are nullable so existing tenants stay valid until the
-- master operator publishes their first policy. When NULL the public
-- /privacy page falls back to the platform-default fixture (see
-- internal/legal.PrivacyPolicyFallback).
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE tenants
  ADD COLUMN IF NOT EXISTS privacy_policy_markdown   text,
  ADD COLUMN IF NOT EXISTS privacy_policy_updated_at timestamptz;

COMMIT;
