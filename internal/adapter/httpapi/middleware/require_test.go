package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
)

// stubAuthorizer captures the Can inputs and returns a pre-canned
// Decision. The test asserts on both the wire response and the
// captured arguments so we know the middleware is forwarding the
// right Principal, Action, and Resource.
type stubAuthorizer struct {
	gotPrincipal iam.Principal
	gotAction    iam.Action
	gotResource  iam.Resource
	reply        iam.Decision
}

func (s *stubAuthorizer) Can(_ context.Context, p iam.Principal, a iam.Action, r iam.Resource) iam.Decision {
	s.gotPrincipal = p
	s.gotAction = a
	s.gotResource = r
	return s.reply
}

func TestRequireAuth_NoSession_401(t *testing.T) {
	t.Parallel()
	called := false
	h := middleware.RequireAuth(middleware.RequireAuthDeps{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if called {
		t.Fatal("next handler must not run when session missing")
	}
}

func TestRequireAuth_PropagatesPrincipal(t *testing.T) {
	t.Parallel()
	sess := iam.Session{UserID: uuid.New(), TenantID: uuid.New(), Role: iam.RoleTenantGerente}
	mfa := time.Now().UTC()
	var got iam.Principal
	gotOK := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, gotOK = iam.PrincipalFromContext(r.Context())
	})
	h := middleware.RequireAuth(middleware.RequireAuthDeps{
		MasterImpersonatingFn: func(*http.Request) bool { return true },
		MFAVerifiedAtFn:       func(*http.Request) *time.Time { return &mfa },
	})(next)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(middleware.WithSession(req.Context(), sess))
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !gotOK {
		t.Fatal("Principal not in downstream context")
	}
	if got.UserID != sess.UserID || got.TenantID != sess.TenantID {
		t.Fatalf("principal mismatch: %+v", got)
	}
	if !got.MasterImpersonating {
		t.Fatal("MasterImpersonating not propagated")
	}
	if got.MFAVerifiedAt == nil || !got.MFAVerifiedAt.Equal(mfa) {
		t.Fatalf("MFAVerifiedAt mismatch: %v", got.MFAVerifiedAt)
	}
}

func TestRequireAuth_NilDeps_DoesNotPanic(t *testing.T) {
	t.Parallel()
	sess := iam.Session{UserID: uuid.New(), TenantID: uuid.New(), Role: iam.RoleTenantCommon}
	called := false
	h := middleware.RequireAuth(middleware.RequireAuthDeps{})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(middleware.WithSession(req.Context(), sess))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Fatal("next must run with valid session and zero deps")
	}
}

func TestRequireAction_NilAuthorizer_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil Authorizer")
		}
	}()
	middleware.RequireAction(nil, iam.ActionTenantContactRead, nil)
}

func TestRequireAction_NoPrincipal_401(t *testing.T) {
	t.Parallel()
	stub := &stubAuthorizer{reply: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC}}
	h := middleware.RequireAction(stub, iam.ActionTenantContactRead, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("next must not run without principal")
	}))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestRequireAction_Allow_PropagatesDecisionAndCallsNext(t *testing.T) {
	t.Parallel()
	stub := &stubAuthorizer{reply: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC, TargetKind: "contact", TargetID: "c1"}}
	var sawDecision iam.Decision
	sawOK := false
	called := false
	resolve := func(*http.Request) iam.Resource { return iam.Resource{Kind: "contact", ID: "c1"} }
	h := middleware.RequireAction(stub, iam.ActionTenantContactRead, resolve)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawDecision, sawOK = middleware.DecisionFromContext(r.Context())
		called = true
	}))
	p := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantGerente}}
	req := httptest.NewRequest(http.MethodGet, "/contacts/c1", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !called {
		t.Fatal("next not called on allow")
	}
	if !sawOK || sawDecision.ReasonCode != iam.ReasonAllowedRBAC {
		t.Fatalf("decision not in context: %+v ok=%v", sawDecision, sawOK)
	}
	if stub.gotAction != iam.ActionTenantContactRead {
		t.Fatalf("action forwarded wrong: %q", stub.gotAction)
	}
	if stub.gotResource.Kind != "contact" || stub.gotResource.ID != "c1" {
		t.Fatalf("resource forwarded wrong: %+v", stub.gotResource)
	}
	if stub.gotPrincipal.UserID != p.UserID {
		t.Fatalf("principal forwarded wrong: %v", stub.gotPrincipal.UserID)
	}
}

func TestRequireAction_Deny_403GenericBody(t *testing.T) {
	t.Parallel()
	// SIN-62756: RequireAction MUST NOT echo the ReasonCode in the
	// 403 body — policy names (denied_master_pii_step_up, denied_rbac,
	// …) leak the existence and shape of internal authorization gates
	// to external tenants. The reason still rides the audit trail
	// (SIN-62254). This test pins both halves: generic body on the
	// wire, and no leak of any defined ReasonCode value.
	stub := &stubAuthorizer{reply: iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}}
	called := false
	h := middleware.RequireAction(stub, iam.ActionTenantContactReadPII, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	p := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantCommon}}
	req := httptest.NewRequest(http.MethodGet, "/pii", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}
	if called {
		t.Fatal("next must not run on deny")
	}
	body := strings.TrimSpace(w.Body.String())
	if body != "forbidden" {
		t.Fatalf("body = %q, want %q", body, "forbidden")
	}
	for _, leak := range []iam.ReasonCode{
		iam.ReasonDeniedRBAC,
		iam.ReasonDeniedMasterPIIStepUp,
		iam.ReasonDeniedTenantMismatch,
		iam.ReasonDeniedUnknownAction,
		iam.ReasonDeniedNoPrincipal,
	} {
		if strings.Contains(w.Body.String(), string(leak)) {
			t.Fatalf("403 body leaked ReasonCode %q: %q", leak, w.Body.String())
		}
	}
}

func TestRequireAction_Deny_GenericBody_ForEveryReasonCode(t *testing.T) {
	t.Parallel()
	// Defense-in-depth: even if the Authorizer evolves to emit new
	// ReasonCodes, the wire body MUST stay generic. This drives the
	// middleware with each currently-defined denial reason and asserts
	// the body shape is identical.
	for _, reason := range []iam.ReasonCode{
		iam.ReasonDeniedRBAC,
		iam.ReasonDeniedMasterPIIStepUp,
		iam.ReasonDeniedTenantMismatch,
		iam.ReasonDeniedUnknownAction,
		iam.ReasonDeniedNoPrincipal,
	} {
		reason := reason
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			stub := &stubAuthorizer{reply: iam.Decision{Allow: false, ReasonCode: reason}}
			h := middleware.RequireAction(stub, iam.ActionTenantContactReadPII, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("next must not run on deny")
			}))
			p := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantCommon}}
			req := httptest.NewRequest(http.MethodGet, "/pii", nil)
			req = req.WithContext(iam.WithPrincipal(req.Context(), p))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, req)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
			if got := strings.TrimSpace(w.Body.String()); got != "forbidden" {
				t.Fatalf("body = %q, want %q", got, "forbidden")
			}
			if strings.Contains(w.Body.String(), string(reason)) {
				t.Fatalf("403 body leaked ReasonCode %q: %q", reason, w.Body.String())
			}
		})
	}
}

func TestRequireAction_NilResolver_ZeroResource(t *testing.T) {
	t.Parallel()
	stub := &stubAuthorizer{reply: iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC}}
	h := middleware.RequireAction(stub, iam.ActionTenantContactRead, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	p := iam.Principal{UserID: uuid.New(), TenantID: uuid.New(), Roles: []iam.Role{iam.RoleTenantGerente}}
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if (stub.gotResource != iam.Resource{}) {
		t.Fatalf("nil resolver should yield zero Resource, got %+v", stub.gotResource)
	}
}

func TestDecisionContext_RoundTrip(t *testing.T) {
	t.Parallel()
	if _, ok := middleware.DecisionFromContext(context.Background()); ok {
		t.Fatal("empty context must not yield a Decision")
	}
	want := iam.Decision{Allow: true, ReasonCode: iam.ReasonAllowedRBAC, TargetKind: "k", TargetID: "id"}
	got, ok := middleware.DecisionFromContext(middleware.WithDecision(context.Background(), want))
	if !ok {
		t.Fatal("DecisionFromContext after WithDecision must be ok")
	}
	if got != want {
		t.Fatalf("round trip mismatch: %+v vs %+v", got, want)
	}
}
