package postgres

// SIN-62341 Lockouts adapter. Two concrete implementations live in this
// file:
//
//   - TenantLockouts wraps the runtime pool and runs every operation
//     under WithTenant for a tenantID supplied at construction. The
//     four-policy RLS template on account_lockout (migrations/0008)
//     keeps a tenant from seeing or writing another tenant's row.
//   - MasterLockouts wraps the same pool and runs under WithMasterOps
//     for an actorID supplied at construction. RLS is bypassed by the
//     app_master_ops role, but every write is recorded by the
//     master_ops_audit trigger so a courtesy lockout (or unlock) is
//     auditable.
//
// Both types satisfy ratelimit.Lockouts (the iam/ratelimit port).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// Compile-time assertions: the two adapters satisfy the domain port.
var (
	_ ratelimit.Lockouts = (*TenantLockouts)(nil)
	_ ratelimit.Lockouts = (*MasterLockouts)(nil)
)

// TenantLockouts is the per-tenant Lockouts adapter. tenantID is captured
// at construction so the port stays scope-agnostic and the http
// middleware does not have to thread the tenant through every call.
type TenantLockouts struct {
	pool     *pgxpool.Pool
	tenantID uuid.UUID
	now      func() time.Time
}

// NewTenantLockouts constructs a TenantLockouts. nil pool returns nil
// so callers see a fast nil-deref panic at first use rather than a
// silent SELECT-no-rows later. uuid.Nil tenantID is rejected — every
// tenant lockout MUST be scoped, so a programmer error here is loud.
func NewTenantLockouts(pool *pgxpool.Pool, tenantID uuid.UUID) (*TenantLockouts, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if tenantID == uuid.Nil {
		return nil, ErrZeroTenant
	}
	return &TenantLockouts{pool: pool, tenantID: tenantID, now: time.Now}, nil
}

// WithClock returns a copy of l whose time source is the supplied
// closure. Tests inject a frozen clock to assert IsLocked boundaries
// deterministically.
func (l *TenantLockouts) WithClock(now func() time.Time) *TenantLockouts {
	cp := *l
	cp.now = now
	return &cp
}

// Lock upserts the lockout row. Re-locking an already-locked user
// extends the existing row's locked_until and refreshes the reason —
// the policy is "the latest reason wins" so the most recent middleware
// decision is reflected in ops dashboards.
func (l *TenantLockouts) Lock(ctx context.Context, userID uuid.UUID, until time.Time, reason string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: TenantLockouts.Lock: userID is nil")
	}
	if until.IsZero() {
		return fmt.Errorf("postgres: TenantLockouts.Lock: until is zero")
	}
	return WithTenant(ctx, l.pool, l.tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO account_lockout (user_id, tenant_id, locked_until, reason)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (user_id) DO UPDATE
			  SET locked_until = EXCLUDED.locked_until,
			      reason       = EXCLUDED.reason
		`, userID, l.tenantID, until, nullIfEmpty(reason))
		if err != nil {
			return fmt.Errorf("postgres: TenantLockouts.Lock exec: %w", err)
		}
		return nil
	})
}

// IsLocked reports whether a row exists for userID with locked_until in
// the future. A row whose locked_until has elapsed is treated as
// "not locked" but is left in place; a future GC pass clears it.
// Leaving stale rows alone keeps the read path purely read-only.
func (l *TenantLockouts) IsLocked(ctx context.Context, userID uuid.UUID) (bool, time.Time, error) {
	var until time.Time
	err := WithTenant(ctx, l.pool, l.tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT locked_until FROM account_lockout WHERE user_id = $1`,
			userID,
		).Scan(&until)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("postgres: TenantLockouts.IsLocked: %w", err)
	}
	if !until.After(l.now()) {
		return false, time.Time{}, nil
	}
	return true, until, nil
}

// Clear deletes the lockout row. Idempotent: a missing row returns nil
// so a successful-login codepath can call Clear unconditionally.
func (l *TenantLockouts) Clear(ctx context.Context, userID uuid.UUID) error {
	return WithTenant(ctx, l.pool, l.tenantID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM account_lockout WHERE user_id = $1`, userID); err != nil {
			return fmt.Errorf("postgres: TenantLockouts.Clear: %w", err)
		}
		return nil
	})
}

// MasterLockouts is the master-scope Lockouts adapter. Every operation
// runs under WithMasterOps; the master_ops_audit trigger writes an
// audit row for each write. Reads run inside the same WithMasterOps
// transaction so the audit trail captures who consulted lockout state
// (master operator activity is audited at session level — see ADR
// 0072 §master ops).
type MasterLockouts struct {
	pool    *pgxpool.Pool
	actorID uuid.UUID
	now     func() time.Time
}

// NewMasterLockouts constructs a MasterLockouts. nil pool returns nil
// so callers see a fast nil-deref panic at first use. uuid.Nil
// actorID is rejected — every master operation MUST be tied to a
// human operator for audit (ADR 0071 §master ops audit).
func NewMasterLockouts(pool *pgxpool.Pool, actorID uuid.UUID) (*MasterLockouts, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &MasterLockouts{pool: pool, actorID: actorID, now: time.Now}, nil
}

// WithClock returns a copy of l with the supplied time source.
func (l *MasterLockouts) WithClock(now func() time.Time) *MasterLockouts {
	cp := *l
	cp.now = now
	return &cp
}

// Lock upserts a master lockout row (tenant_id IS NULL). The
// master_ops_audit trigger writes the corresponding audit entry.
func (l *MasterLockouts) Lock(ctx context.Context, userID uuid.UUID, until time.Time, reason string) error {
	if userID == uuid.Nil {
		return fmt.Errorf("postgres: MasterLockouts.Lock: userID is nil")
	}
	if until.IsZero() {
		return fmt.Errorf("postgres: MasterLockouts.Lock: until is zero")
	}
	return WithMasterOps(ctx, l.pool, l.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO account_lockout (user_id, tenant_id, locked_until, reason)
			VALUES ($1, NULL, $2, $3)
			ON CONFLICT (user_id) DO UPDATE
			  SET locked_until = EXCLUDED.locked_until,
			      reason       = EXCLUDED.reason
		`, userID, until, nullIfEmpty(reason))
		if err != nil {
			return fmt.Errorf("postgres: MasterLockouts.Lock exec: %w", err)
		}
		return nil
	})
}

// IsLocked reports whether a master row exists for userID with
// locked_until in the future. Master and tenant rows for the same
// user_id are forbidden by the PK; a master row therefore has
// tenant_id IS NULL.
func (l *MasterLockouts) IsLocked(ctx context.Context, userID uuid.UUID) (bool, time.Time, error) {
	var until time.Time
	err := WithMasterOps(ctx, l.pool, l.actorID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT locked_until FROM account_lockout WHERE user_id = $1 AND tenant_id IS NULL`,
			userID,
		).Scan(&until)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, time.Time{}, nil
	}
	if err != nil {
		return false, time.Time{}, fmt.Errorf("postgres: MasterLockouts.IsLocked: %w", err)
	}
	if !until.After(l.now()) {
		return false, time.Time{}, nil
	}
	return true, until, nil
}

// Clear deletes the master lockout row. Idempotent.
func (l *MasterLockouts) Clear(ctx context.Context, userID uuid.UUID) error {
	return WithMasterOps(ctx, l.pool, l.actorID, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM account_lockout WHERE user_id = $1 AND tenant_id IS NULL`, userID); err != nil {
			return fmt.Errorf("postgres: MasterLockouts.Clear: %w", err)
		}
		return nil
	})
}
