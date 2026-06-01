package middlewaretest_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware/middlewaretest"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
)

func TestWithActiveImpersonation_RoundTripsThroughMiddlewareReader(t *testing.T) {
	t.Parallel()

	sess := &impersonation.Session{
		ID:              uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		MasterUserID:    uuid.MustParse("22222222-2222-2222-2222-222222222222"),
		MasterSessionID: uuid.MustParse("33333333-3333-3333-3333-333333333333"),
		TargetTenantID:  uuid.MustParse("44444444-4444-4444-4444-444444444444"),
		Reason:          "regression: SIN-63978",
		StartedAt:       time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		ExpiresAt:       time.Date(2026, 6, 1, 12, 15, 0, 0, time.UTC),
	}
	ctx := middlewaretest.WithActiveImpersonation(t, context.Background(), sess)
	got, ok := middleware.ActiveImpersonation(ctx)
	if !ok {
		t.Fatal("expected envelope present after WithActiveImpersonation")
	}
	if got != sess {
		t.Fatalf("expected identical pointer; got %p, want %p", got, sess)
	}
}

func TestWithActiveImpersonation_NilSessionReadsBackAsAbsent(t *testing.T) {
	t.Parallel()
	ctx := middlewaretest.WithActiveImpersonation(t, context.Background(), nil)
	got, ok := middleware.ActiveImpersonation(ctx)
	if ok || got != nil {
		t.Fatalf("expected (nil,false) for nil envelope; got (%v,%v)", got, ok)
	}
}
