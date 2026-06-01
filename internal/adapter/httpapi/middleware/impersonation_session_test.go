package middleware_test

// SIN-63958 / master-impersonation-spec §5.5 — middleware tests.
//
// Items covered:
//   3  impersonation_stop audit row written when middleware detects
//      expires_at lapsed (step 4 of spec §1.3).
//   4  correlation_id propagated on authz rows fired during active
//      impersonation (spec §3.1: ContextWithCorrelationID sets the
//      context value; downstream handlers read it).

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/impersonation"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// ----- fakes (local to this file to avoid conflicts with existing fakes) ----

type fakeImpRepo struct {
	active *impersonation.Session
	err    error

	endCalls []string // reasons passed to End
}

func (f *fakeImpRepo) Start(_ context.Context, in impersonation.StartInput) (*impersonation.Session, error) {
	return nil, errors.New("not implemented in this fake")
}

func (f *fakeImpRepo) ActiveForSession(_ context.Context, _ uuid.UUID) (*impersonation.Session, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.active == nil {
		return nil, impersonation.ErrNoActiveImpersonation
	}
	return f.active, nil
}

func (f *fakeImpRepo) End(_ context.Context, _ uuid.UUID, reason string, _ time.Time) error {
	f.endCalls = append(f.endCalls, reason)
	return nil
}

func (f *fakeImpRepo) ListAuditByCorrelation(_ context.Context, _ uuid.UUID, _ int) ([]audit.SecurityRow, error) {
	return nil, nil
}

// ----- helpers --------------------------------------------------------------

var (
	mwMasterUserID   = uuid.MustParse("aaaaaaaa-1111-1111-1111-aaaaaaaaaaaa")
	mwMasterSessID   = uuid.MustParse("bbbbbbbb-2222-2222-2222-bbbbbbbbbbbb")
	mwTargetTenantID = uuid.MustParse("cccccccc-3333-3333-3333-cccccccccccc")
)

func activeEnvelope(expiresAt time.Time) *impersonation.Session {
	return &impersonation.Session{
		ID:              uuid.New(),
		MasterUserID:    mwMasterUserID,
		MasterSessionID: mwMasterSessID,
		TargetTenantID:  mwTargetTenantID,
		Reason:          "test reason text",
		StartedAt:       expiresAt.Add(-5 * time.Minute),
		ExpiresAt:       expiresAt,
	}
}

func requestWithMasterSession(method, path string, userID uuid.UUID) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.AddCookie(&http.Cookie{
		Name:  sessioncookie.NameMaster,
		Value: mwMasterSessID.String(),
	})
	ctx := middleware.WithSession(r.Context(), iam.Session{
		ID:       mwMasterSessID,
		UserID:   userID,
		TenantID: uuid.New(),
	})
	return r.WithContext(ctx)
}

func buildSessionMW(
	checker middleware.MasterChecker,
	repo impersonation.Repo,
	auditor audit.SplitLogger,
	clock func() time.Time,
	resolver tenancy.ByIDResolver,
) func(http.Handler) http.Handler {
	return middleware.ImpersonationFromSession(checker, resolver, repo, auditor, clock, nil)
}

// ----- Spec §5.5 #3: expiry writes impersonation_stop row ------------------

// TestImpersonationFromSession_ExpiryWritesAuditStop verifies that when the
// active envelope's ExpiresAt has already passed, the middleware:
//   - calls repo.End with reason "expired"
//   - writes a SecurityEventImpersonationStop audit row
//   - redirects to /master/tenants?expired=1 (303)
//   - does NOT call next.
func TestImpersonationFromSession_ExpiryWritesAuditStop(t *testing.T) {
	t.Parallel()

	past := time.Date(2026, 6, 1, 11, 59, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC) // after past

	repo := &fakeImpRepo{active: activeEnvelope(past)}
	aud := &recordingLogger{}
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{mwMasterUserID: true}}
	resolver := &fakeByIDResolver{
		tenants: map[uuid.UUID]*tenancy.Tenant{
			mwTargetTenantID: {ID: mwTargetTenantID, Name: "acme", Host: "acme.crm.local"},
		},
	}

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	mw := buildSessionMW(checker, repo, aud, func() time.Time { return now }, resolver)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, requestWithMasterSession(http.MethodGet, "/master/tenants", mwMasterUserID))

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status=%d, want 303 (expired redirect)", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/master/tenants?expired=1" {
		t.Errorf("Location=%q, want /master/tenants?expired=1", got)
	}
	if called {
		t.Error("next was called, want it NOT called after expiry")
	}

	// repo.End must have been invoked with reason "expired".
	if len(repo.endCalls) == 0 {
		t.Fatal("repo.End not called, want reason=expired")
	}
	if repo.endCalls[0] != "expired" {
		t.Errorf("end reason=%q, want expired", repo.endCalls[0])
	}

	// audit row for impersonation_stop must exist.
	events := aud.snapshot()
	var stops []audit.SecurityAuditEvent
	for _, e := range events {
		if e.Event == audit.SecurityEventImpersonationStop {
			stops = append(stops, e)
		}
	}
	if len(stops) == 0 {
		t.Fatal("no impersonation_stop audit event written on expiry")
	}
}

// ----- Spec §5.5 #4: correlation_id propagated through middleware ----------

// TestImpersonationFromSession_CorrelationIDPropagated verifies that when an
// active (non-expired) envelope exists, the middleware attaches the
// correlation_id to the context via audit.ContextWithCorrelationID, and
// downstream handlers can read it back with audit.CorrelationIDFromContext.
func TestImpersonationFromSession_CorrelationIDPropagated(t *testing.T) {
	t.Parallel()

	future := time.Date(2026, 6, 1, 12, 15, 0, 0, time.UTC)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

	sess := activeEnvelope(future)
	want := sess.ID

	repo := &fakeImpRepo{active: sess}
	aud := &recordingLogger{}
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{mwMasterUserID: true}}
	resolver := &fakeByIDResolver{
		tenants: map[uuid.UUID]*tenancy.Tenant{
			mwTargetTenantID: {ID: mwTargetTenantID, Name: "acme", Host: "acme.crm.local"},
		},
	}

	var got uuid.UUID
	var gotOK bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, gotOK = audit.CorrelationIDFromContext(r.Context())
	})

	mw := buildSessionMW(checker, repo, aud, func() time.Time { return now }, resolver)
	rec := httptest.NewRecorder()
	mw(next).ServeHTTP(rec, requestWithMasterSession(http.MethodGet, "/master/someaction", mwMasterUserID))

	if !gotOK {
		t.Fatal("CorrelationIDFromContext returned false, want true")
	}
	if got != want {
		t.Errorf("correlation_id=%v, want %v", got, want)
	}
}
