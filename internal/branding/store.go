package branding

import (
	"context"
	"errors"

	"github.com/google/uuid"
)

// ErrPaletteNotFound is the sentinel a PaletteStore implementation
// returns when no per-tenant palette has been persisted. Producers
// (notably the theme middleware in
// internal/adapter/httpapi/middleware) fall back to DefaultPalette and
// cache the negative result so a flood of requests for an unbranded
// tenant does not hammer the database.
var ErrPaletteNotFound = errors.New("branding: palette not found")

// PaletteStore is the port the runtime theming layer reaches for to
// load a tenant's persisted palette. The Postgres-backed adapter
// against the tenant_palette table (introduced in SIN-63075) lives in
// internal/adapter/branding; this package stays free of database
// imports per the hexagonal boundary documented in ADR 0060.
//
// Implementations MUST:
//   - honour ctx (deadline, cancellation),
//   - return ErrPaletteNotFound when the tenant has no row,
//   - return any other error unwrapped — callers cache only the
//     not-found sentinel and surface transient errors as the default
//     palette without poisoning the cache.
type PaletteStore interface {
	GetByTenantID(ctx context.Context, tenantID uuid.UUID) (Palette, error)
}
