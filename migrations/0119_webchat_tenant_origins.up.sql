-- Webchat per-tenant origin config (ADR-0021 D2 + D4).
--
-- webchat_allowed_origins: JSONB array of full origins
--   (e.g. ["https://acme.com.br","https://www.acme.com.br"]). No
--   subdomain wildcard in Fase 2. Empty array = fail-closed: the
--   tenant must populate it before the webchat channel accepts a
--   session (POST /widget/v1/session returns 403). Default '[]'.
--
-- webchat_origin_secret: per-tenant HMAC-SHA256 key for the origin
--   signature (D4). Rotatable; redacted in audit logs. NULL until the
--   tenant enables the channel — the OriginValidator treats NULL as
--   fail-closed.
--
-- Both live as columns on tenants (not a dedicated tenant_settings
-- table) following the 0108 dpo_* precedent; tenants is NOT under RLS
-- (migration 0004) so the OriginValidator reads them with a single
-- keyed SELECT, the documented exception to "all reads through
-- WithTenant".
ALTER TABLE tenants
    ADD COLUMN IF NOT EXISTS webchat_allowed_origins jsonb NOT NULL DEFAULT '[]'::jsonb,
    ADD COLUMN IF NOT EXISTS webchat_origin_secret   bytea;

-- Fix a latent gap from migration 0096: webchat_session was created
-- WITHOUT per-table grants (this codebase grants every table explicitly
-- — see 0001 "Per-table privileges are granted in [each migration]"),
-- so app_runtime (NOBYPASSRLS) could never read or write it. The table
-- was never wired into a composition root until SIN-64972, so the gap
-- was dormant. Grant the runtime + master_ops roles the CRUD the
-- SessionStore needs (DELETE covers the ADR-0021 daily expiry sweep).
--
-- webchat_session intentionally carries NO row-level-security policy:
-- the session id is an unguessable uuid-v7 capability and the only key
-- the visitor presents, so the handler reads it by id alone (the tenant
-- is recovered FROM the row). The row stores only hashes + tenant_id —
-- no PII — so capability-by-session-id is the chosen boundary; this is
-- called out for the SecurityEngineer review of the public surface.
GRANT SELECT, INSERT, UPDATE, DELETE ON webchat_session TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON webchat_session TO app_master_ops;
