package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/tenancy"
)

// TenantQuerier is the minimal pool surface NewTenantResolver needs.
// *pgxpool.Pool satisfies it. Defining the interface keeps the adapter
// trivially mockable while keeping the production wiring (cmd/server)
// simple — pass the pool, done.
type TenantQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// TenantResolver is the postgres-backed tenancy.Resolver. It runs a
// single SELECT on the tenants table keyed by host.
//
// IMPORTANT: this query intentionally runs OUTSIDE of WithTenant. The
// resolver's whole purpose is to figure out which tenant a request
// belongs to, so it cannot itself depend on app.tenant_id being set.
// The tenants table is never tenant-scoped (no tenant_id column, no
// RLS — see migration 0004), and app_runtime is granted SELECT on it
// (also migration 0004). That single SELECT is the documented
// exception to the "all reads through WithTenant" rule. The notenant
// linter exempts this package by path; see tools/lint/notenant.
//
// Follow-up: ADR-0002 will document this exception alongside the
// existing role/RLS ADRs (0071/0072).
type TenantResolver struct {
	db TenantQuerier
}

// NewTenantResolver wires the resolver around a pool. ErrNilPool is
// returned eagerly so a misconfigured cmd/server fails at startup
// rather than on the first request.
func NewTenantResolver(db TenantQuerier) (*TenantResolver, error) {
	if db == nil {
		return nil, ErrNilPool
	}
	return &TenantResolver{db: db}, nil
}

const tenantByHostSQL = `SELECT id, name, host FROM tenants WHERE host = $1`

const tenantByIDSQL = `SELECT id, name, host FROM tenants WHERE id = $1`

// ResolveByHost runs the host lookup. Misses become tenancy.ErrTenantNotFound
// so the middleware can render the secure-by-default 404.
func (r *TenantResolver) ResolveByHost(ctx context.Context, host string) (*tenancy.Tenant, error) {
	if r == nil || r.db == nil {
		return nil, ErrNilPool
	}
	if host == "" {
		return nil, tenancy.ErrTenantNotFound
	}

	var (
		id      uuid.UUID
		name    string
		gotHost string
	)
	row := r.db.QueryRow(ctx, tenantByHostSQL, host)
	if err := row.Scan(&id, &name, &gotHost); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tenancy.ErrTenantNotFound
		}
		return nil, fmt.Errorf("postgres: tenant lookup: %w", err)
	}
	return &tenancy.Tenant{ID: id, Name: name, Host: gotHost}, nil
}

// ResolveByID looks up a tenant by uuid. Used by the master impersonation
// middleware (SIN-62219). uuid.Nil and a missing row both collapse to
// tenancy.ErrTenantNotFound so the caller can return a generic 4xx.
func (r *TenantResolver) ResolveByID(ctx context.Context, id uuid.UUID) (*tenancy.Tenant, error) {
	if r == nil || r.db == nil {
		return nil, ErrNilPool
	}
	if id == uuid.Nil {
		return nil, tenancy.ErrTenantNotFound
	}

	var (
		gotID   uuid.UUID
		name    string
		gotHost string
	)
	row := r.db.QueryRow(ctx, tenantByIDSQL, id)
	if err := row.Scan(&gotID, &name, &gotHost); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, tenancy.ErrTenantNotFound
		}
		return nil, fmt.Errorf("postgres: tenant by id lookup: %w", err)
	}
	return &tenancy.Tenant{ID: gotID, Name: name, Host: gotHost}, nil
}
