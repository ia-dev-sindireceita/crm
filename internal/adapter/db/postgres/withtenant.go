// Package postgres holds the pgx-backed adapters for tenanted database
// access. Domain code MUST go through WithTenant or WithMasterOps; the
// custom analyzer in tools/lint/notenant fails CI if any code under
// internal/ calls *pgxpool.Pool.{Exec,Query,QueryRow,SendBatch} directly.
//
// See docs/adr/0071-postgres-roles.md and docs/adr/0072-rls-policies.md
// for the full design.
package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/obs"
)

// TxBeginner is the minimal surface WithTenant / WithMasterOps need to start
// a transaction. *pgxpool.Pool satisfies it. Defining a small interface keeps
// the helpers test-friendly without leaking the pool type to call sites.
type TxBeginner interface {
	BeginTx(ctx context.Context, opts pgx.TxOptions) (pgx.Tx, error)
}

// Compile-time assertion that *pgxpool.Pool still satisfies TxBeginner.
var _ TxBeginner = (*pgxpool.Pool)(nil)

// ErrNilPool is returned when callers pass a nil pool. Use errors.Is to test.
var ErrNilPool = errors.New("postgres: pool is nil")

// ErrNilFn is returned when callers pass a nil work function.
var ErrNilFn = errors.New("postgres: fn is nil")

// ErrZeroTenant is returned when callers pass uuid.Nil to WithTenant.
var ErrZeroTenant = errors.New("postgres: tenantID must not be uuid.Nil")

// ErrZeroActor is returned when callers pass uuid.Nil to WithMasterOps.
var ErrZeroActor = errors.New("postgres: actorID must not be uuid.Nil")

// WithTenant runs fn inside a transaction with the GUC app.tenant_id set to
// tenantID for the duration of the transaction. Every RLS policy on a
// tenanted table is gated on app.tenant_id; if WithTenant is bypassed the
// policy compares against NULL and denies every row.
//
// fn receives the pgx.Tx so it can run any number of queries inside the same
// tenant scope. Returning a non-nil error rolls the transaction back. The
// transaction is also rolled back on context cancellation.
//
// set_config(..., true) is used instead of "SET LOCAL app.tenant_id = $1"
// because pgx does not bind parameters into SET LOCAL.
func WithTenant(ctx context.Context, db TxBeginner, tenantID uuid.UUID, fn func(pgx.Tx) error) error {
	if db == nil {
		return ErrNilPool
	}
	if tenantID == uuid.Nil {
		// rls_misses_total is the SIN-62218 canary: defense in depth
		// alongside middleware + Auth + RLS. The middleware path
		// should make a uuid.Nil reach this branch impossible, so
		// any increment alarms oncall via Prometheus + Alertmanager.
		obs.IncRLSMiss()
		return ErrZeroTenant
	}
	if fn == nil {
		return ErrNilFn
	}

	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: WithTenant begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rollback uses a fresh background context so an already-
			// cancelled ctx still releases the connection.
			_ = tx.Rollback(context.Background())
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.tenant_id', $1, true)", tenantID.String()); err != nil {
		return fmt.Errorf("postgres: WithTenant set tenant: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: WithTenant commit: %w", err)
	}
	committed = true
	return nil
}

// WithMasterOps runs fn inside a transaction with the GUC
// app.master_ops_actor_user_id set to actorID. The role used to connect MUST
// be app_master_ops; the master_ops_audit_trigger refuses to write audit
// rows otherwise and aborts the transaction.
//
// A "session_open" audit row is written eagerly so that even an inspection-
// only fn (SELECT-only) leaves a record of who logged in cross-tenant.
//
// Callers MUST NOT use WithMasterOps for tenant-scoped work; that is what
// WithTenant is for. WithMasterOps exists only for the small set of master
// console operations that need to span tenants (incident response, billing
// rollups, GDPR deletes orchestrated by support).
func WithMasterOps(ctx context.Context, db TxBeginner, actorID uuid.UUID, fn func(pgx.Tx) error) error {
	if db == nil {
		return ErrNilPool
	}
	if actorID == uuid.Nil {
		return ErrZeroActor
	}
	if fn == nil {
		return ErrNilFn
	}

	tx, err := db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("postgres: WithMasterOps begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.Background())
		}
	}()

	if _, err := tx.Exec(ctx, "SELECT set_config('app.master_ops_actor_user_id', $1, true)", actorID.String()); err != nil {
		return fmt.Errorf("postgres: WithMasterOps set actor: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO master_ops_audit (actor_user_id, tenant_id, query_kind, target_table, target_pk)
		VALUES ($1, NULL, 'session_open', '__session__', NULL)
	`, actorID); err != nil {
		return fmt.Errorf("postgres: WithMasterOps audit open: %w", err)
	}

	if err := fn(tx); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: WithMasterOps commit: %w", err)
	}
	committed = true
	return nil
}
