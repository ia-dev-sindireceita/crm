-- Reverse 0119: drop the per-tenant webchat origin columns and the
-- webchat_session grants added on top of migration 0096.
REVOKE SELECT, INSERT, UPDATE, DELETE ON webchat_session FROM app_runtime;
REVOKE SELECT, INSERT, UPDATE, DELETE ON webchat_session FROM app_master_ops;

ALTER TABLE tenants
    DROP COLUMN IF EXISTS webchat_allowed_origins,
    DROP COLUMN IF EXISTS webchat_origin_secret;
