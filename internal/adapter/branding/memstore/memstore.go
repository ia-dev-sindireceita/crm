// Package memstore provides an in-memory PaletteStore + PaletteWriter
// adapter for the branding port. It is the bootstrap adapter the SIN-
// 63084 admin UI consumes until the Postgres-backed tenant_palette
// adapter lands with SIN-63075. The struct is goroutine-safe and is
// intended to be a single per-process singleton wired alongside the
// theme middleware so reads and writes share state.
package memstore

import (
	"context"
	"sync"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/branding"
)

// Store is the in-memory PaletteStore + PaletteWriter implementation.
// The zero value is unsafe — always construct via New so the underlying
// map is initialised.
type Store struct {
	mu      sync.RWMutex
	palette map[uuid.UUID]branding.Palette
}

// New constructs an empty Store ready for use.
func New() *Store {
	return &Store{palette: map[uuid.UUID]branding.Palette{}}
}

// Compile-time interface assertions so a refactor of either port flags
// here first.
var (
	_ branding.PaletteStore  = (*Store)(nil)
	_ branding.PaletteWriter = (*Store)(nil)
)

// GetByTenantID returns the stored palette or ErrPaletteNotFound when
// none has been persisted. ctx is honoured for cancellation.
func (s *Store) GetByTenantID(ctx context.Context, tenantID uuid.UUID) (branding.Palette, error) {
	if err := ctx.Err(); err != nil {
		return branding.Palette{}, err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.palette[tenantID]
	if !ok {
		return branding.Palette{}, branding.ErrPaletteNotFound
	}
	return p, nil
}

// SetForTenant upserts the tenant palette atomically.
func (s *Store) SetForTenant(ctx context.Context, tenantID uuid.UUID, p branding.Palette) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.palette[tenantID] = p
	return nil
}

// DeleteForTenant removes the persisted palette. Absence is a success.
func (s *Store) DeleteForTenant(ctx context.Context, tenantID uuid.UUID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.palette, tenantID)
	return nil
}
