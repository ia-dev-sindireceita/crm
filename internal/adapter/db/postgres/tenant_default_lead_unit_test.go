package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// Unit-level coverage for the F2-07.2 read path (SIN-62833). These
// tests pair with the integration tests in
// tenant_default_lead_integration_test.go: the integration tests
// prove the SQL contract; these stub-driven tests cover the
// guard/error branches that are awkward to reach against a real DB.

func TestTenantResolver_DefaultLeadUserID_NilReceiver(t *testing.T) {
	t.Parallel()
	var nilResolver *postgresadapter.TenantResolver
	if _, err := nilResolver.DefaultLeadUserID(context.Background(), uuid.New()); !errors.Is(err, postgresadapter.ErrNilPool) {
		t.Errorf("err = %v, want ErrNilPool", err)
	}
}

func TestTenantResolver_DefaultLeadUserID_NoRowsMapsToNotFound(t *testing.T) {
	t.Parallel()
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: pgx.ErrNoRows}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.DefaultLeadUserID(context.Background(), uuid.New()); !errors.Is(err, tenancy.ErrTenantNotFound) {
		t.Errorf("err = %v, want ErrTenantNotFound", err)
	}
}

func TestTenantResolver_DefaultLeadUserID_TransientErrorWraps(t *testing.T) {
	t.Parallel()
	transient := errors.New("connection refused")
	r, err := postgresadapter.NewTenantResolver(stubQuerier{row: stubRow{err: transient}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.DefaultLeadUserID(context.Background(), uuid.New())
	if err == nil {
		t.Fatal("err = nil, want wrapped transient error")
	}
	if !errors.Is(err, transient) {
		t.Errorf("err = %v, want wraps %v", err, transient)
	}
	if !strings.Contains(err.Error(), "default_lead_user_id") {
		t.Errorf("err = %q, want context prefix", err.Error())
	}
}
