package authz_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

func TestMetrics_RegistersOnRegistererAndCounts(t *testing.T) {
	t.Parallel()
	reg := prometheus.NewRegistry()
	m := authz.NewMetrics(reg)

	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000001"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000002"),
	}
	m.Observe(p, iam.ActionTenantContactRead, iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC})
	m.Observe(p, iam.ActionTenantContactRead, iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC})
	m.Observe(p, iam.ActionTenantContactRead, iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC})

	if got := testutil.ToFloat64(m.Decisions.WithLabelValues("tenant.contact.read", "denied_rbac", "deny")); got != 2 {
		t.Fatalf("decisions deny counter = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.Decisions.WithLabelValues("tenant.contact.read", "allowed_rbac", "allow")); got != 1 {
		t.Fatalf("decisions allow counter = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String())); got != 2 {
		t.Fatalf("user deny counter = %v, want 2", got)
	}
}

func TestMetrics_NilReceiverIsSafe(t *testing.T) {
	t.Parallel()
	var m *authz.Metrics
	// Should not panic; nothing to assert beyond that.
	m.Observe(iam.Principal{}, iam.ActionTenantContactRead, iam.Decision{})
}

func TestMetrics_AllowDoesNotIncrementUserDeny(t *testing.T) {
	t.Parallel()
	m := authz.NewMetrics(nil)
	p := iam.Principal{
		UserID:   uuid.MustParse("00000000-0000-0000-0000-000000000010"),
		TenantID: uuid.MustParse("00000000-0000-0000-0000-000000000020"),
	}
	for i := 0; i < 5; i++ {
		m.Observe(p, iam.ActionTenantContactRead, iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC})
	}
	if got := testutil.ToFloat64(m.UserDeny.WithLabelValues(p.UserID.String(), p.TenantID.String())); got != 0 {
		t.Fatalf("UserDeny grew on allow path: %v", got)
	}
}

func TestMetrics_NilRegistererBuildsUnregisteredVectors(t *testing.T) {
	t.Parallel()
	m := authz.NewMetrics(nil)
	if m.Decisions == nil || m.UserDeny == nil {
		t.Fatal("vectors must be non-nil even when registerer is nil")
	}
	// Re-register on a fresh registry to assert the vectors are
	// well-formed independent of NewMetrics' registration step.
	reg := prometheus.NewRegistry()
	reg.MustRegister(m.Decisions, m.UserDeny)
}
