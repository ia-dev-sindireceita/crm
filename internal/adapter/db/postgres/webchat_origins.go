package postgres

// SIN-64972 — Postgres OriginValidator for the webchat public surface
// (ADR-0021 D2 CORS allowlist + D4 origin signature).
//
// Reads tenants.webchat_allowed_origins (jsonb array) and
// tenants.webchat_origin_secret (bytea). The tenants table is NOT under
// RLS (migration 0004) and app_runtime is granted SELECT, so — like the
// TenantResolver — this adapter issues a single keyed SELECT outside
// WithTenant. That is the documented exception to "all reads through
// WithTenant".
//
// Fail-closed by construction: an empty/absent allowlist denies every
// origin; a NULL/empty origin secret makes HMAC error so POST
// /widget/v1/session returns 403 until the tenant provisions the secret.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebchatOrigins is the pgx-backed webchat.OriginValidator.
type WebchatOrigins struct {
	pool *pgxpool.Pool
}

// NewWebchatOrigins wraps pool. A nil pool returns nil so callers see a
// clear nil-deref at first use rather than a silent "no rows" later.
func NewWebchatOrigins(pool *pgxpool.Pool) *WebchatOrigins {
	if pool == nil {
		return nil
	}
	return &WebchatOrigins{pool: pool}
}

const webchatTenantOriginsSQL = `
	SELECT webchat_allowed_origins, webchat_origin_secret
	  FROM tenants
	 WHERE id = $1`

// load returns the tenant's allowed origins and HMAC secret. A missing
// tenant row yields (nil, nil, nil): fail-closed, not an error.
func (o *WebchatOrigins) load(ctx context.Context, tenantID uuid.UUID) (origins []string, secret []byte, err error) {
	var rawOrigins []byte
	row := o.pool.QueryRow(ctx, webchatTenantOriginsSQL, tenantID)
	if scanErr := row.Scan(&rawOrigins, &secret); scanErr != nil {
		if errors.Is(scanErr, pgx.ErrNoRows) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("postgres: webchat origins load: %w", scanErr)
	}
	if len(rawOrigins) > 0 {
		if err := json.Unmarshal(rawOrigins, &origins); err != nil {
			return nil, nil, fmt.Errorf("postgres: webchat allowed_origins decode: %w", err)
		}
	}
	return origins, secret, nil
}

// Valid implements webchat.OriginValidator. It returns true only when
// the canonicalised origin exactly matches a canonicalised allowlist
// entry. An empty allowlist is fail-closed (false, nil).
func (o *WebchatOrigins) Valid(ctx context.Context, tenantID uuid.UUID, origin string) (bool, error) {
	origins, _, err := o.load(ctx, tenantID)
	if err != nil {
		return false, err
	}
	want, err := canonicalOrigin(origin)
	if err != nil {
		return false, nil // malformed origin is not allowlisted
	}
	for _, allowed := range origins {
		got, err := canonicalOrigin(allowed)
		if err != nil {
			continue
		}
		if got == want {
			return true, nil
		}
	}
	return false, nil
}

// HMAC implements webchat.OriginValidator: HMAC-SHA256(origin_secret,
// canonical_origin) hex-encoded (ADR-0021 D4). A NULL/empty secret is
// fail-closed (error → handler 403).
func (o *WebchatOrigins) HMAC(ctx context.Context, tenantID uuid.UUID, origin string) (string, error) {
	_, secret, err := o.load(ctx, tenantID)
	if err != nil {
		return "", err
	}
	if len(secret) == 0 {
		return "", errors.New("postgres: webchat origin secret not provisioned")
	}
	canonical, err := canonicalOrigin(origin)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(canonical))
	return hex.EncodeToString(mac.Sum(nil)), nil
}

// canonicalOrigin normalises an origin to "<scheme>://<host>[:<port>]"
// with scheme+host lowercased and the default port (80 http / 443
// https) omitted (ADR-0021 D4).
func canonicalOrigin(origin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if scheme == "" || host == "" {
		return "", fmt.Errorf("postgres: malformed origin %q", origin)
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	if port != "" {
		return scheme + "://" + host + ":" + port, nil
	}
	return scheme + "://" + host, nil
}
