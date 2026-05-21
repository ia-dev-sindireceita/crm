package postgres

// SIN-63184 tenant pre-MFA pending-session adapter. Backs the two-step
// admin login flow:
//
//   1. POST /admin/login        — password ok → write a pending row,
//                                  set the __Host-mfa-pending cookie,
//                                  redirect to /admin/2fa/verify.
//   2. POST /admin/2fa/verify   — TOTP/recovery ok → delete the pending
//                                  row, mint the real tenant session,
//                                  set __Host-sess-tenant.
//
// The pending cookie has a short TTL (default 5 min) so an abandoned
// half-login does not leak indefinitely. The cookie value is the
// pending row's UUID; the row carries the resolved user_id, tenant_id,
// and the originally-requested next path so the verify handler can
// honour ?next= even though the URL was rewritten by the redirect.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PendingMFASession is the tenant-side pre-MFA session row.
type PendingMFASession struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TenantID  uuid.UUID
	ExpiresAt time.Time
	NextPath  string
}

// IsExpired reports whether now is at or past ExpiresAt. The verify
// handler rejects an expired row as if the cookie were missing.
func (p PendingMFASession) IsExpired(now time.Time) bool {
	return !now.Before(p.ExpiresAt)
}

// ErrPendingMFANotFound is returned by Get when no row matches the
// supplied id.
var ErrPendingMFANotFound = errors.New("postgres: pending mfa session not found")

// TenantUserMFAPending is the tenant-scope adapter for the
// user_mfa_pending_session table.
type TenantUserMFAPending struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
	now      func() time.Time
}

// NewTenantUserMFAPending validates inputs and returns the adapter.
func NewTenantUserMFAPending(pool *pgxpool.Pool, tenantID uuid.UUID) (*TenantUserMFAPending, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TenantUserMFAPending{pool: pool, tenantID: tenantID, now: time.Now}, nil
}

// WithClock returns a copy whose time source is the supplied closure.
func (a *TenantUserMFAPending) WithClock(now func() time.Time) *TenantUserMFAPending {
	cp := *a
	cp.now = now
	return &cp
}

// Create inserts a fresh pending row that expires after ttl.
func (a *TenantUserMFAPending) Create(ctx context.Context, userID uuid.UUID, ttl time.Duration, nextPath string) (PendingMFASession, error) {
	if userID == uuid.Nil {
		return PendingMFASession{}, fmt.Errorf("postgres: TenantUserMFAPending.Create: userID is nil")
	}
	if ttl <= 0 {
		return PendingMFASession{}, fmt.Errorf("postgres: TenantUserMFAPending.Create: ttl must be positive")
	}
	expires := a.clock().Add(ttl).UTC()
	id := uuid.New()
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO user_mfa_pending_session (id, user_id, tenant_id, expires_at, next_path)
			VALUES ($1, $2, $3, $4, $5)
		`, id, userID, a.tenantID, expires, nextPath)
		if err != nil {
			return fmt.Errorf("exec: %w", err)
		}
		return nil
	})
	if err != nil {
		return PendingMFASession{}, fmt.Errorf("postgres: TenantUserMFAPending.Create: %w", err)
	}
	return PendingMFASession{
		ID:        id,
		UserID:    userID,
		TenantID:  a.tenantID,
		ExpiresAt: expires,
		NextPath:  nextPath,
	}, nil
}

// Get returns the row with the supplied id, or ErrPendingMFANotFound
// when no row matches.
func (a *TenantUserMFAPending) Get(ctx context.Context, id uuid.UUID) (PendingMFASession, error) {
	if id == uuid.Nil {
		return PendingMFASession{}, ErrPendingMFANotFound
	}
	var row PendingMFASession
	row.ID = id
	row.TenantID = a.tenantID
	err := WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT user_id, expires_at, next_path
			   FROM user_mfa_pending_session
			  WHERE id = $1`,
			id,
		).Scan(&row.UserID, &row.ExpiresAt, &row.NextPath)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return PendingMFASession{}, ErrPendingMFANotFound
	}
	if err != nil {
		return PendingMFASession{}, fmt.Errorf("postgres: TenantUserMFAPending.Get: %w", err)
	}
	return row, nil
}

// Delete removes the row with the supplied id. A delete of a missing
// row is NOT an error — the operation is idempotent.
func (a *TenantUserMFAPending) Delete(ctx context.Context, id uuid.UUID) error {
	if id == uuid.Nil {
		return nil
	}
	return WithTenant(ctx, a.pool, a.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`DELETE FROM user_mfa_pending_session WHERE id = $1`,
			id,
		)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserMFAPending.Delete: %w", err)
		}
		return nil
	})
}

func (a *TenantUserMFAPending) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}
