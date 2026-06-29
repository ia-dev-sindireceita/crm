package whatsmeowdev

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestSQLStoreDeviceForFailsClosedOnSecondTenant is the F2 regression test
// (SIN-66268): registering two distinct tenants against the same container must
// fail closed. The first tenant binds the container; a second, distinct tenant
// must be refused with ErrTenantIsolation instead of silently resolving to the
// first tenant's session (cross-tenant credential leak / BOLA).
//
// The guard rejects the second tenant *before* touching the container, so the
// nil container is never dereferenced — this exercises the real DeviceFor
// fail-closed path without a database (no DB mocking; rule 5).
func TestSQLStoreDeviceForFailsClosedOnSecondTenant(t *testing.T) {
	t.Parallel()
	s := &sqlStore{} // nil container: only the binding guard runs

	tenantA := uuid.New()
	// Bind the container to tenant A. We bind via the guard directly because
	// fetching A's device would require a live container; the binding gate is
	// the isolation boundary under test.
	if err := s.bindTenant(tenantA); err != nil {
		t.Fatalf("binding first tenant: unexpected err: %v", err)
	}

	tenantB := uuid.New()
	if _, err := s.DeviceFor(context.Background(), tenantB); !errors.Is(err, ErrTenantIsolation) {
		t.Fatalf("DeviceFor(second distinct tenant) err = %v, want ErrTenantIsolation", err)
	}
}

func TestSQLStoreBindTenant(t *testing.T) {
	t.Parallel()

	t.Run("zero tenant is rejected", func(t *testing.T) {
		t.Parallel()
		s := &sqlStore{}
		if err := s.bindTenant(uuid.Nil); !errors.Is(err, ErrTenantIsolation) {
			t.Fatalf("bindTenant(uuid.Nil) err = %v, want ErrTenantIsolation", err)
		}
	})

	t.Run("first tenant binds, same tenant is idempotent", func(t *testing.T) {
		t.Parallel()
		s := &sqlStore{}
		tenant := uuid.New()
		if err := s.bindTenant(tenant); err != nil {
			t.Fatalf("first bind: %v", err)
		}
		if err := s.bindTenant(tenant); err != nil {
			t.Fatalf("re-bind same tenant should be idempotent, got %v", err)
		}
	})

	t.Run("distinct second tenant is refused", func(t *testing.T) {
		t.Parallel()
		s := &sqlStore{}
		if err := s.bindTenant(uuid.New()); err != nil {
			t.Fatalf("first bind: %v", err)
		}
		if err := s.bindTenant(uuid.New()); !errors.Is(err, ErrTenantIsolation) {
			t.Fatalf("second distinct bind err = %v, want ErrTenantIsolation", err)
		}
	})

	t.Run("isolation error never leaks the refused tenant id", func(t *testing.T) {
		t.Parallel()
		s := &sqlStore{}
		bound := uuid.New()
		if err := s.bindTenant(bound); err != nil {
			t.Fatalf("first bind: %v", err)
		}
		refused := uuid.New()
		err := s.bindTenant(refused)
		if err == nil {
			t.Fatal("expected isolation error")
		}
		if strings.Contains(err.Error(), refused.String()) {
			t.Errorf("error message %q leaks the refused tenant id", err)
		}
	})
}
