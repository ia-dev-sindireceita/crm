package postgres_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/db/postgres/testpg"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// Stable UUIDs match migrations/seed/stg.sql so changing one means
// changing both.
const (
	acmeID   = "00000000-0000-0000-0000-00000000ac01"
	globexID = "00000000-0000-0000-0000-00000000eb02"
)

// applyTenantMigration runs migration 0004 (creates the tenants table)
// against the per-test DB. testpg.Start applies 0001/0002/0003 globally,
// but the tenants table belongs to a later migration we exercise here.
func applyTenantMigration(t *testing.T, db *testpg.DB) {
	t.Helper()
	migDir := harness.MigrationsDir()
	sqlBytes, err := os.ReadFile(filepath.Join(migDir, "0004_create_tenant.up.sql"))
	if err != nil {
		t.Fatalf("read 0004: %v", err)
	}
	if _, err := db.AdminPool().Exec(newCtx(t), string(sqlBytes)); err != nil {
		t.Fatalf("apply 0004: %v", err)
	}
}

func seedAcmeAndGlobex(t *testing.T, db *testpg.DB) {
	t.Helper()
	if _, err := db.AdminPool().Exec(newCtx(t), `
		INSERT INTO tenants (id, name, host) VALUES
		  ($1, 'acme',   'acme.crm.local'),
		  ($2, 'globex', 'globex.crm.local')
		ON CONFLICT (id) DO NOTHING
	`, acmeID, globexID); err != nil {
		t.Fatalf("seed tenants: %v", err)
	}
}

func TestResolveByHost_AcmeAndGlobex(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)
	seedAcmeAndGlobex(t, db)

	resolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantResolver: %v", err)
	}

	ctx := newCtx(t)
	wantAcme := uuid.MustParse(acmeID)
	wantGlobex := uuid.MustParse(globexID)

	gotAcme, err := resolver.ResolveByHost(ctx, "acme.crm.local")
	if err != nil {
		t.Fatalf("acme lookup: %v", err)
	}
	if gotAcme.ID != wantAcme || gotAcme.Name != "acme" || gotAcme.Host != "acme.crm.local" {
		t.Fatalf("acme: got %#v", gotAcme)
	}

	gotGlobex, err := resolver.ResolveByHost(ctx, "globex.crm.local")
	if err != nil {
		t.Fatalf("globex lookup: %v", err)
	}
	if gotGlobex.ID != wantGlobex || gotGlobex.Name != "globex" {
		t.Fatalf("globex: got %#v", gotGlobex)
	}
}

func TestResolveByHost_Unknown_404Generic(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)

	resolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatalf("NewTenantResolver: %v", err)
	}

	got, err := resolver.ResolveByHost(newCtx(t), "ghost.crm.local")
	if !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
	if got != nil {
		t.Fatalf("tenant = %#v, want nil", got)
	}
}

func TestResolveByHost_EmptyHost_NotFound(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)

	resolver, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.ResolveByHost(newCtx(t), ""); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("empty host err = %v, want ErrTenantNotFound", err)
	}
}

func TestResolveByHost_CacheCutsDBHits(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)
	seedAcmeAndGlobex(t, db)

	upstream, err := postgresadapter.NewTenantResolver(db.RuntimePool())
	if err != nil {
		t.Fatal(err)
	}
	counter := &countingResolver{upstream: upstream}
	cache := tenancy.NewCachingResolver(counter, 0)

	ctx := newCtx(t)
	for i := 0; i < 5; i++ {
		if _, err := cache.ResolveByHost(ctx, "acme.crm.local"); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	if counter.calls != 1 {
		t.Fatalf("upstream DB resolver called %d times, want exactly 1", counter.calls)
	}
}

func TestNewTenantResolver_NilPool(t *testing.T) {
	if _, err := postgresadapter.NewTenantResolver(nil); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Fatalf("err = %v, want ErrNilPool", err)
	}
}

// TestResolveByID_KnownAndUnknown was relocated here in SIN-62424
// (Phase B.2) when the legacy audit_logger_test.go was deleted. It
// exercises the tenant resolver, not the audit logger; it lived in the
// wrong file by accident.
func TestResolveByID_KnownAndUnknown(t *testing.T) {
	db := harness.DB(t)
	applyTenantMigration(t, db)
	seedAcmeAndGlobex(t, db)
	resolver, _ := postgresadapter.NewTenantResolver(db.RuntimePool())
	ctx := newCtx(t)

	got, err := resolver.ResolveByID(ctx, uuid.MustParse(acmeID))
	if err != nil {
		t.Fatalf("known id: %v", err)
	}
	if got.Host != "acme.crm.local" {
		t.Fatalf("got.Host=%q, want acme.crm.local", got.Host)
	}

	if _, err := resolver.ResolveByID(ctx, uuid.New()); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("unknown id: err=%v, want ErrTenantNotFound", err)
	}
	if _, err := resolver.ResolveByID(ctx, uuid.Nil); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("nil id: err=%v, want ErrTenantNotFound", err)
	}
}

// countingResolver wraps a tenancy.Resolver to assert exactly one
// underlying DB round-trip per host across many cache calls.
type countingResolver struct {
	upstream tenancy.Resolver
	calls    int
}

func (c *countingResolver) ResolveByHost(ctx context.Context, host string) (*tenancy.Tenant, error) {
	c.calls++
	return c.upstream.ResolveByHost(ctx, host)
}
