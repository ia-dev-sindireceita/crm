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

// PaletteWriter is the write-side port the branding admin UI
// (SIN-63084) reaches for to persist an overridden palette and to
// revert manual overrides. It is intentionally a separate interface
// from PaletteStore so the runtime theming layer (which only reads)
// keeps its dependency surface narrow.
//
// Implementations MUST:
//   - honour ctx (deadline, cancellation),
//   - upsert atomically on SetForTenant: a concurrent reader either
//     sees the previous palette or the new one, never a partial row,
//   - treat DeleteForTenant against an absent tenant as a success — the
//     UI revert flow may issue a delete even when no override exists.
type PaletteWriter interface {
	SetForTenant(ctx context.Context, tenantID uuid.UUID, p Palette) error
	DeleteForTenant(ctx context.Context, tenantID uuid.UUID) error
}
