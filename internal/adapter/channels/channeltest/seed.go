//go:build integration

package channeltest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

// SeedTenant inserts a tenant row with the supplied id (or a fresh one
// when uuid.Nil is passed). The row satisfies the NOT NULL constraints
// from migration 0004; integration tests use it to obtain a tenant_id
// the production resolver fakes can register against the webhook
// fixture's phone_number_id / ig_business_id.
func (h *Harness) SeedTenant(t *testing.T, id uuid.UUID) uuid.UUID {
	t.Helper()
	if id == uuid.Nil {
		id = uuid.New()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := h.pool.Exec(ctx,
		`INSERT INTO tenants (id, name, host) VALUES ($1, $2, $3)`,
		id, fmt.Sprintf("t-%s", id), fmt.Sprintf("%s.crm.local", id),
	); err != nil {
		t.Fatalf("channeltest: seed tenant: %v", err)
	}
	return id
}
