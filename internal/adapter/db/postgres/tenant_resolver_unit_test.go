package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// stubRow lets us drive the QueryRow Scan path (no rows, transient error)
// without spinning up Postgres. Used to keep adapter coverage above 85%.
type stubRow struct{ err error }

func (r stubRow) Scan(...any) error { return r.err }

type stubQuerier struct{ row pgx.Row }

func (s stubQuerier) QueryRow(_ context.Context, _ string, _ ...any) pgx.Row { return s.row }

func TestTenantResolver_ScanNoRowsMapsToNotFound(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: pgx.ErrNoRows}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveByHost(context.Background(), "ghost.crm.local"); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Fatalf("err = %v, want ErrTenantNotFound", err)
	}
}

func TestTenantResolver_ScanTransientErrorWraps(t *testing.T) {
	t.Parallel()
	transient := errors.New("connection reset by peer")
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: transient}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.ResolveByHost(context.Background(), "acme.crm.local")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, transient) {
		t.Fatalf("err = %v, want wraps %v", err, transient)
	}
	if !strings.Contains(err.Error(), "tenant lookup") {
		t.Fatalf("err = %q, want context prefix", err.Error())
	}
}
