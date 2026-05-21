package postgres

// SIN-63184 otpauth label reader. Used by /admin/2fa/setup to embed the
// user's email under the issuer string in the QR code. tenantID is per
// call (the verify cookie carries the pending row's tenantID) so the
// adapter is constructed once and reused; isolation comes from
// WithTenant on every read.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TenantUserLabel resolves the otpauth:// label for a tenant user by
// reading users.email under WithTenant. The HTTP layer holds it by an
// interface (usermfa.UserLabelReader) so the http package never
// imports pgx.
type TenantUserLabel struct {
	pool *pgxpool.Pool
}

// NewTenantUserLabel wraps a pool.
func NewTenantUserLabel(pool *pgxpool.Pool) (*TenantUserLabel, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	return &TenantUserLabel{pool: pool}, nil
}

// LookupLabel returns users.email for the supplied principal.
func (a *TenantUserLabel) LookupLabel(ctx context.Context, tenantID, userID uuid.UUID) (string, error) {
	if tenantID == uuid.Nil || userID == uuid.Nil {
		return "", fmt.Errorf("postgres: TenantUserLabel.LookupLabel: zero id")
	}
	var email string
	err := WithTenant(ctx, a.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT email FROM users WHERE id = $1`, userID).Scan(&email)
	})
	if err != nil {
		return "", fmt.Errorf("postgres: TenantUserLabel.LookupLabel: %w", err)
	}
	return email, nil
}
