package postgres

// SIN-64972 — Postgres webchat.SessionStore over webchat_session
// (migration 0096, ADR-0021 D3).
//
// webchat_session is NOT under RLS (its migration adds no policy), so —
// like every webchat row it carries an explicit tenant_id — this
// adapter issues direct keyed queries against the pool. The session id
// is a uuid-v7 primary key, globally unique, so Get/Touch key on it
// alone; the tenant scope is recovered from the row's tenant_id column,
// which the message path trusts for downstream inbox delivery.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	webchat "github.com/pericles-luz/crm/internal/adapter/channels/webchat"
)

// webchatSessionTTL mirrors the unexported webchat.sessionTTL (30min
// idle window, ADR-0021 D3). Touch extends expires_at by this amount.
const webchatSessionTTL = 30 * time.Minute

// WebchatSessionStore is the pgx-backed webchat.SessionStore.
type WebchatSessionStore struct {
	pool *pgxpool.Pool
}

// NewWebchatSessionStore wraps pool. A nil pool returns nil so callers
// see a clear nil-deref at first use.
func NewWebchatSessionStore(pool *pgxpool.Pool) *WebchatSessionStore {
	if pool == nil {
		return nil
	}
	return &WebchatSessionStore{pool: pool}
}

// Create inserts an anonymous visitor session. created_at and
// last_activity_at default to now() at the DB.
func (s *WebchatSessionStore) Create(ctx context.Context, sess webchat.Session) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webchat_session
			(id, tenant_id, csrf_token_hash, origin_sig, ip_hash, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		sess.ID, sess.TenantID, sess.CSRFTokenHash, sess.OriginSig, sess.IPHash, sess.ExpiresAt)
	if err != nil {
		return fmt.Errorf("postgres: webchat session create: %w", err)
	}
	return nil
}

// Get returns the session by id. A missing row maps to
// webchat.ErrSessionNotFound so the handler answers 401.
func (s *WebchatSessionStore) Get(ctx context.Context, sessionID string) (webchat.Session, error) {
	var out webchat.Session
	err := s.pool.QueryRow(ctx, `
		SELECT id, tenant_id, csrf_token_hash, origin_sig, ip_hash, expires_at
		  FROM webchat_session
		 WHERE id = $1`, sessionID).
		Scan(&out.ID, &out.TenantID, &out.CSRFTokenHash, &out.OriginSig, &out.IPHash, &out.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return webchat.Session{}, webchat.ErrSessionNotFound
	}
	if err != nil {
		return webchat.Session{}, fmt.Errorf("postgres: webchat session get: %w", err)
	}
	return out, nil
}

// Touch bumps last_activity_at and slides the idle expiry forward. A
// missing row maps to webchat.ErrSessionNotFound.
func (s *WebchatSessionStore) Touch(ctx context.Context, sessionID string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE webchat_session
		   SET last_activity_at = now(),
		       expires_at = now() + make_interval(secs => $2)
		 WHERE id = $1`,
		sessionID, webchatSessionTTL.Seconds())
	if err != nil {
		return fmt.Errorf("postgres: webchat session touch: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return webchat.ErrSessionNotFound
	}
	return nil
}
