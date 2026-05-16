-- webchat_session persists anonymous visitor sessions created by
-- POST /widget/v1/session (ADR-0021 D3).
-- csrf_token_hash = sha256(csrf_token), base64url; plaintext never stored.
-- origin_sig = HMAC-SHA256(tenant_origin_secret, canonical_origin).
-- ip_hash = sha256(ip || tenant_id); LGPD-safe: plaintext never stored.
CREATE TABLE IF NOT EXISTS webchat_session (
    id               text        PRIMARY KEY,
    tenant_id        uuid        NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    csrf_token_hash  text        NOT NULL,
    origin_sig       text        NOT NULL,
    ip_hash          text        NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_activity_at timestamptz NOT NULL DEFAULT now(),
    expires_at       timestamptz NOT NULL
);

CREATE INDEX IF NOT EXISTS webchat_session_tenant_expires
    ON webchat_session (tenant_id, expires_at);
