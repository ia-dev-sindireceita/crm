package lgpd_test

// SIN-63191 / Fase 6 PR4 — port-shape assertions for the new
// DeletionLister read port + InRetention synthetic status constant.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/lgpd"
)

// Compile-time assertion: any narrow type can satisfy the small read
// port without dragging in the full DeletionRepository surface.
type onlyLister struct{}

func (onlyLister) ListByTenant(_ context.Context, _ uuid.UUID, _ lgpd.DeletionStatus, _ int) ([]lgpd.DeletionRequest, error) {
	return nil, nil
}

var _ lgpd.DeletionLister = onlyLister{}

func TestInRetention_IsStable(t *testing.T) {
	if lgpd.InRetention != "in_retention" {
		t.Fatalf("InRetention = %q; want in_retention (the literal is wire-stable)", lgpd.InRetention)
	}
}
