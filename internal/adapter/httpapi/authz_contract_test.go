package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
)

// chainAuthorizedHandler composes the production middleware stack
// for a single action so the contract test exercises the full
// request → Principal → Authorizer → Decision → handler path. We do
// not bring up the full router (that's covered by router_test.go);
// the goal here is to lock the role × action × HTTP-status contract.
func chainAuthorizedHandler(authz iam.Authorizer, action iam.Action) http.Handler {
	final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	withAction := middleware.RequireAction(authz, action, nil)(final)
	withAuth := middleware.RequireAuth(middleware.RequireAuthDeps{
		MasterImpersonatingFn: func(*http.Request) bool { return false },
		MFAVerifiedAtFn:       func(*http.Request) *time.Time { return nil },
	})(withAction)
	return withAuth
}

func sessionRequest(role iam.Role) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	sess := iam.Session{UserID: uuid.New(), TenantID: uuid.New(), Role: role}
	return req.WithContext(middleware.WithSession(req.Context(), sess))
}

// TestAuthz_Contract_RoleActionMatrix is the wire-level contract test:
// every cell asserts (role, action) → HTTP status, with the production
// RBACAuthorizer wired through RequireAuth + RequireAction. When the
// ADR 0090 matrix changes, this test changes in lockstep — both
// directions (a code change without a matrix update fails; a matrix
// update without a code change fails too).
func TestAuthz_Contract_RoleActionMatrix(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})

	cases := []struct {
		name       string
		role       iam.Role
		action     iam.Action
		wantStatus int
	}{
		{"common-read-contact-ALLOW", iam.RoleTenantCommon, iam.ActionTenantContactRead, http.StatusOK},
		{"common-read-pii-DENY", iam.RoleTenantCommon, iam.ActionTenantContactReadPII, http.StatusForbidden},
		{"common-create-contact-DENY", iam.RoleTenantCommon, iam.ActionTenantContactCreate, http.StatusForbidden},
		{"common-send-message-DENY", iam.RoleTenantCommon, iam.ActionTenantMessageSend, http.StatusForbidden},

		{"atendente-send-message-ALLOW", iam.RoleTenantAtendente, iam.ActionTenantMessageSend, http.StatusOK},
		{"atendente-update-contact-ALLOW", iam.RoleTenantAtendente, iam.ActionTenantContactUpdate, http.StatusOK},
		{"atendente-create-contact-DENY", iam.RoleTenantAtendente, iam.ActionTenantContactCreate, http.StatusForbidden},
		{"atendente-read-pii-DENY", iam.RoleTenantAtendente, iam.ActionTenantContactReadPII, http.StatusForbidden},

		{"gerente-read-pii-ALLOW", iam.RoleTenantGerente, iam.ActionTenantContactReadPII, http.StatusOK},
		{"gerente-delete-contact-ALLOW", iam.RoleTenantGerente, iam.ActionTenantContactDelete, http.StatusOK},
		{"gerente-master-action-DENY", iam.RoleTenantGerente, iam.ActionMasterTenantCreate, http.StatusForbidden},

		{"master-master-action-ALLOW", iam.RoleMaster, iam.ActionMasterTenantImpersonate, http.StatusOK},
		{"master-tenant-action-DENY", iam.RoleMaster, iam.ActionTenantContactRead, http.StatusForbidden},

		// SIN-62880 — master billing/courtesy ops (master only).
		{"master-subscription-assign-plan-ALLOW", iam.RoleMaster, iam.ActionMasterSubscriptionAssignPlan, http.StatusOK},
		{"master-subscription-cancel-ALLOW", iam.RoleMaster, iam.ActionMasterSubscriptionCancel, http.StatusOK},
		{"master-grant-free-period-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyFreeSubscriptionPeriod, http.StatusOK},
		{"master-grant-extra-tokens-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyExtraTokens, http.StatusOK},
		{"master-grant-revoke-ALLOW", iam.RoleMaster, iam.ActionMasterGrantCourtesyRevoke, http.StatusOK},
		{"gerente-subscription-assign-plan-DENY", iam.RoleTenantGerente, iam.ActionMasterSubscriptionAssignPlan, http.StatusForbidden},
		{"gerente-grant-revoke-DENY", iam.RoleTenantGerente, iam.ActionMasterGrantCourtesyRevoke, http.StatusForbidden},
		{"atendente-grant-extra-tokens-DENY", iam.RoleTenantAtendente, iam.ActionMasterGrantCourtesyExtraTokens, http.StatusForbidden},

		// SIN-62880 — tenant billing/wallet reads (gerente only).
		{"gerente-billing-view-ALLOW", iam.RoleTenantGerente, iam.ActionTenantBillingView, http.StatusOK},
		{"gerente-wallet-view-ledger-ALLOW", iam.RoleTenantGerente, iam.ActionTenantWalletViewLedger, http.StatusOK},
		{"atendente-billing-view-DENY", iam.RoleTenantAtendente, iam.ActionTenantBillingView, http.StatusForbidden},
		{"atendente-wallet-view-ledger-DENY", iam.RoleTenantAtendente, iam.ActionTenantWalletViewLedger, http.StatusForbidden},
		{"common-billing-view-DENY", iam.RoleTenantCommon, iam.ActionTenantBillingView, http.StatusForbidden},
		{"common-wallet-view-ledger-DENY", iam.RoleTenantCommon, iam.ActionTenantWalletViewLedger, http.StatusForbidden},
		{"master-billing-view-DENY", iam.RoleMaster, iam.ActionTenantBillingView, http.StatusForbidden},
		{"master-wallet-view-ledger-DENY", iam.RoleMaster, iam.ActionTenantWalletViewLedger, http.StatusForbidden},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			handler := chainAuthorizedHandler(authz, tc.action)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, sessionRequest(tc.role))
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body=%q)", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// TestAuthz_Contract_DenyByDefault asserts an unauthenticated request
// hits RequireAuth's 401 path, even when the resolved action would
// otherwise be public. This is the defense-in-depth check: any
// non-public route protected by RequireAuth refuses sessionless calls.
func TestAuthz_Contract_DenyByDefault(t *testing.T) {
	t.Parallel()
	authz := iam.NewRBACAuthorizer(iam.RBACConfig{})
	handler := chainAuthorizedHandler(authz, iam.ActionTenantContactRead)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/x", nil))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (deny-by-default)", w.Code)
	}
}

// TestAuthz_Contract_PublicRoutesNotAuthGated documents the inverse:
// the canonical public-route patterns are present in the allowlist
// so the wireup is free to mount them WITHOUT RequireAuth. The lint
// in PR-B will scan the router and enforce this; here we just lock
// the data contract so the lint has a stable source of truth.
func TestAuthz_Contract_PublicRoutesNotAuthGated(t *testing.T) {
	t.Parallel()
	for _, route := range []struct{ method, pattern string }{
		{http.MethodGet, "/health"},
		{http.MethodGet, "/login"},
		{http.MethodPost, "/login"},
		{http.MethodGet, "/m/login"},
	} {
		if !httpapi.IsPublic(route.method, route.pattern) {
			t.Errorf("expected %q %q to be public but allowlist says no", route.method, route.pattern)
		}
	}
}
