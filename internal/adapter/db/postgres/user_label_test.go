package postgres_test

// SIN-63184 TenantUserLabel integration tests. Mirrors the migration-
// stack used by TenantUserMFAPending so we re-use freshDBWithUserMFA
// without needing extra tables.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

func TestNewTenantUserLabel_RejectsBadInputs(t *testing.T) {
	t.Parallel()
	if _, err := postgres.NewTenantUserLabel(nil); !errors.Is(err, postgres.ErrNilPool) {
		t.Fatalf("nil pool err = %v, want ErrNilPool", err)
	}
}

func TestTenantUserLabel_LookupLabel(t *testing.T) {
	db := freshDBWithUserMFA(t)
	tenant, user := seedTenantUser(t, db, "acme-label.crm.local", "admin@acme-label.test")
	ctx := context.Background()

	a, err := postgres.NewTenantUserLabel(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantUserLabel: %v", err)
	}
	got, err := a.LookupLabel(ctx, tenant, user)
	if err != nil {
		t.Fatalf("LookupLabel: %v", err)
	}
	if got != "admin@acme-label.test" {
		t.Fatalf("LookupLabel: want %q got %q", "admin@acme-label.test", got)
	}

	if _, err := a.LookupLabel(ctx, uuid.Nil, user); err == nil {
		t.Fatalf("expected error for zero tenant id")
	}
	if _, err := a.LookupLabel(ctx, tenant, uuid.Nil); err == nil {
		t.Fatalf("expected error for zero user id")
	}
}
