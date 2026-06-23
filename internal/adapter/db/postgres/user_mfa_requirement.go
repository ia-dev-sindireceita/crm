package postgres

// SIN-63184 user MFA-requirement reader. The login post handler uses
// this to decide whether to mint the real session cookie or redirect
// to the /admin/2fa flow with a pending cookie:
//
//   * role == "admin"            → TOTP required (AC #1).
//   * totp_required_at IS NOT NULL → TOTP required (AC #7 opt-in for member).
//   * neither                     → TOTP not required; proceed to
//                                   the normal tenant session.
//
// Enrolled mirrors user_mfa.reenroll_required = false: a user whose
// recovery code was just consumed is treated as not enrolled so the
// next login forces a fresh setup.

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// UserMFARequirement is the read snapshot the login handler consumes.
type UserMFARequirement struct {
	Role            string
	TOTPRequired    bool
	TOTPEnrolled    bool
	ReenrollPending bool
}

// AdminRole names the role string that triggers mandatory TOTP under
// AC #1. LMHost's tenant role vocabulary uses "admin" for the
// tenant administrator; "member" is the opt-in non-admin role.
const AdminRole = "admin"

// TenantUserMFARequirement queries users.role + users.totp_required_at
// + user_mfa.reenroll_required for a tenant-scoped principal.
type TenantUserMFARequirement struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
}

// NewTenantUserMFARequirement validates inputs and returns the reader.
func NewTenantUserMFARequirement(pool *pgxpool.Pool, tenantID uuid.UUID) (*TenantUserMFARequirement, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TenantUserMFARequirement{pool: pool, tenantID: tenantID}, nil
}

// Load returns the MFA-requirement snapshot for userID.
//
// The row JOIN is LEFT so users without a user_mfa row report
// TOTPEnrolled=false instead of erroring out. The query runs inside
// WithTenant so RLS isolates the read to this adapter's tenantID.
func (r *TenantUserMFARequirement) Load(ctx context.Context, userID uuid.UUID) (UserMFARequirement, error) {
	if userID == uuid.Nil {
		return UserMFARequirement{}, fmt.Errorf("postgres: TenantUserMFARequirement.Load: userID is nil")
	}
	var (
		role           string
		hasMFARow      bool
		reenrollScan   *bool
		requiredAtScan *string
	)
	err := WithTenant(ctx, r.pool, r.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT u.role,
			       CASE WHEN u.totp_required_at IS NULL THEN NULL
			            ELSE u.totp_required_at::text END,
			       m.user_id IS NOT NULL AS has_mfa_row,
			       m.reenroll_required
			  FROM users u
			  LEFT JOIN user_mfa m ON m.user_id = u.id
			 WHERE u.id = $1
		`, userID).Scan(&role, &requiredAtScan, &hasMFARow, &reenrollScan)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return UserMFARequirement{}, fmt.Errorf("postgres: TenantUserMFARequirement.Load: user not found")
	}
	if err != nil {
		return UserMFARequirement{}, fmt.Errorf("postgres: TenantUserMFARequirement.Load: %w", err)
	}
	out := UserMFARequirement{
		Role:         role,
		TOTPRequired: role == AdminRole || requiredAtScan != nil,
		TOTPEnrolled: hasMFARow && (reenrollScan == nil || !*reenrollScan),
	}
	if reenrollScan != nil {
		out.ReenrollPending = *reenrollScan
	}
	return out, nil
}

// SetTOTPRequired stamps users.totp_required_at = now() so the next
// login enforces TOTP. Used by the opt-in setup route for member
// users (AC #7).
func (r *TenantUserMFARequirement) SetTOTPRequired(ctx context.Context, userID uuid.UUID) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: TenantUserMFARequirement.SetTOTPRequired: userID is nil")
	}
	return WithTenant(ctx, r.pool, r.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE users SET totp_required_at = now() WHERE id = $1 AND totp_required_at IS NULL`,
			userID,
		)
		if err != nil {
			return fmt.Errorf("postgres: TenantUserMFARequirement.SetTOTPRequired: %w", err)
		}
		return nil
	})
}
