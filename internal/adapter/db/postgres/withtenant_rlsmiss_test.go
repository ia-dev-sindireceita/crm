package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/prometheus/client_golang/prometheus/testutil"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/obs"
)

// SIN-62218: WithTenant must increment rls_misses_total (the canary)
// when called with uuid.Nil. We assert against a test-local Metrics
// instance set as the package default for the duration of the test
// and reset on cleanup so the integration tests aren't polluted.

func TestWithTenant_NilTenantID_BumpsRLSMisses(t *testing.T) {
	prev := obs.Default()
	t.Cleanup(func() { obs.SetDefault(prev) })

	m := obs.NewMetrics()
	obs.SetDefault(m)

	beginner := &stubBeginner{tx: &stubTx{}}
	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.Nil, func(pgx.Tx) error {
		t.Fatal("fn must not run when tenantID is uuid.Nil")
		return nil
	})
	if !errors.Is(err, postgresadapter.ErrZeroTenant) {
		t.Errorf("err: got %v, want ErrZeroTenant", err)
	}
	if got := testutil.ToFloat64(m.RLSMisses); got != 1 {
		t.Errorf("rls_misses_total: got %v, want 1", got)
	}
}

// Sanity: a valid tenantID does NOT increment the counter.
func TestWithTenant_ValidTenantID_DoesNotBumpRLSMisses(t *testing.T) {
	prev := obs.Default()
	t.Cleanup(func() { obs.SetDefault(prev) })

	m := obs.NewMetrics()
	obs.SetDefault(m)

	beginner := &stubBeginner{tx: &stubTx{}}
	err := postgresadapter.WithTenant(context.Background(), beginner, uuid.New(), func(pgx.Tx) error {
		return nil
	})
	if err != nil {
		t.Fatalf("WithTenant valid: %v", err)
	}
	if got := testutil.ToFloat64(m.RLSMisses); got != 0 {
		t.Errorf("rls_misses_total: got %v, want 0", got)
	}
}
