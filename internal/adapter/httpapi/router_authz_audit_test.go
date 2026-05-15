package httpapi_test

// SIN-62765 — HTTP-boundary wireup test for the SIN-62254
// AuditingAuthorizer. The decorator implements iam.Authorizer, so
// RequireAction must accept it without change and the recorder must
// see every decision (deny at 100%, allow when the sampler says yes).
//
// This is the contract the AC pins: "RequireAction middleware and its
// tests should require zero changes (decorator implements the same
// iam.Authorizer interface)". A regression here means a future
// refactor accidentally re-typed the seam.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/authz"
)

type capturedRecord struct {
	principal iam.Principal
	action    iam.Action
	resource  iam.Resource
	decision  iam.Decision
}

type recordingRecorder struct {
	mu      sync.Mutex
	records []capturedRecord
}

func (r *recordingRecorder) Record(_ context.Context, p iam.Principal, a iam.Action, res iam.Resource, d iam.Decision, _ time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, capturedRecord{principal: p, action: a, resource: res, decision: d})
}

func (r *recordingRecorder) snapshot() []capturedRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]capturedRecord, len(r.records))
	copy(out, r.records)
	return out
}

func auditedChain(t *testing.T, recorder *recordingRecorder, sampler authz.Sampler, action iam.Action) http.Handler {
	t.Helper()
	inner := iam.NewRBACAuthorizer(iam.RBACConfig{})
	audited := authz.New(authz.Config{
		Inner:    inner,
		Recorder: recorder,
		Sampler:  sampler,
	})
	final := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	withAction := middleware.RequireAction(audited, action, nil)(final)
	return middleware.RequireAuth(middleware.RequireAuthDeps{
		MasterImpersonatingFn: func(*http.Request) bool { return false },
		MFAVerifiedAtFn:       func(*http.Request) *time.Time { return nil },
	})(withAction)
}

func authedRequest(role iam.Role) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	sess := iam.Session{UserID: uuid.New(), TenantID: uuid.New(), Role: role}
	return req.WithContext(middleware.WithSession(req.Context(), sess))
}

func TestRequireAction_AcceptsAuditedAuthorizer_DenyRecorded(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	handler := auditedChain(t, rec, authz.NeverSample{}, iam.ActionTenantContactReadPII)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedRequest(iam.RoleTenantCommon))

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded = %d, want 1 (deny is unsampled)", len(got))
	}
	if got[0].decision.Allow {
		t.Fatalf("recorded an allow on a 403 path: %+v", got[0])
	}
	if got[0].decision.ReasonCode != iam.ReasonDeniedRBAC {
		t.Fatalf("reason_code = %s, want %s", got[0].decision.ReasonCode, iam.ReasonDeniedRBAC)
	}
}

func TestRequireAction_AcceptsAuditedAuthorizer_AllowRecordedWhenSamplerYes(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	handler := auditedChain(t, rec, authz.AlwaysSample{}, iam.ActionTenantContactRead)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedRequest(iam.RoleTenantAtendente))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("recorded = %d, want 1 (sampler=Always)", len(got))
	}
	if !got[0].decision.Allow {
		t.Fatalf("recorded a deny on a 200 path: %+v", got[0])
	}
}

func TestRequireAction_AcceptsAuditedAuthorizer_AllowDroppedWhenSamplerNo(t *testing.T) {
	t.Parallel()
	rec := &recordingRecorder{}
	handler := auditedChain(t, rec, authz.NeverSample{}, iam.ActionTenantContactRead)

	w := httptest.NewRecorder()
	handler.ServeHTTP(w, authedRequest(iam.RoleTenantAtendente))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("recorded = %d, want 0 (sampler=Never)", len(got))
	}
}
