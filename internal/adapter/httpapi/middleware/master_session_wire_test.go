package middleware_test

// SIN-65321 — wire-level regression for the /master/* impersonation 503.
//
// The real /master/* operator subtree composes (router.go):
//
//	... → RequirePrincipalFromMaster → RequireAction → ImpersonationFromSession → handler
//
// RequireAuth (the tenant-session middleware that normally seeds iam.Session)
// is intentionally ABSENT from this chain (SecEng C3). Before SIN-65321,
// RequirePrincipalFromMaster seeded only an iam.Principal — never a session —
// so ImpersonationFromSession's "session missing on ctx → 503" guard fired for
// every fully authenticated master operator.
//
// These tests exercise the REAL two-middleware ordering (no stubbed
// RequirePrincipalFromMaster) and assert the operator reaches the handler
// instead of 503ing.

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/tenancy"
)

const wireMasterHost = "master.crm.local"

// masterChain composes RequirePrincipalFromMaster → ImpersonationFromSession
// exactly as the /master/* subtree does, around the supplied handler. The
// master context is seeded onto the request (as RequireMasterAuth would
// upstream) so the principal/session synthesis succeeds.
func masterChain(t *testing.T, repo *fakeImpRepo, masterID uuid.UUID, next http.Handler) http.Handler {
	t.Helper()
	principalMW := mastermfa.RequirePrincipalFromMaster(
		mastermfa.RequirePrincipalFromMasterConfig{MasterHost: wireMasterHost},
	)
	checker := &fakeMasterChecker{masters: map[uuid.UUID]bool{masterID: true}}
	resolver := &fakeByIDResolver{tenants: map[uuid.UUID]*tenancy.Tenant{
		mwTargetTenantID: {ID: mwTargetTenantID},
	}}
	clock := func() time.Time { return time.Date(2026, 6, 20, 12, 0, 0, 0, time.UTC) }
	impMW := middleware.ImpersonationFromSession(checker, resolver, repo, &recordingLogger{}, clock, nil)
	return principalMW(impMW(next))
}

func masterChainRequest(masterID uuid.UUID, withCookie bool) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/master/impersonation/feed", nil)
	r.Host = wireMasterHost
	if withCookie {
		r.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: mwMasterSessID.String()})
	}
	// RequireMasterAuth seeds the master onto the context upstream.
	return r.WithContext(mastermfa.WithMaster(r.Context(),
		mastermfa.Master{ID: masterID, Email: "ops@example.com"}))
}

// The original bug: a master operator with NO active impersonation envelope
// still 503'd because the session was missing. After the fix the request
// passes through both middlewares and reaches the handler.
func TestMasterChain_NoEnvelope_ReachesHandler_Not503(t *testing.T) {
	masterID := uuid.New()
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	// repo with no active envelope → ActiveForSession returns
	// ErrNoActiveImpersonation → pass-through (step 2).
	repo := &fakeImpRepo{}
	r := masterChainRequest(masterID, true)
	w := httptest.NewRecorder()
	masterChain(t, repo, masterID, next).ServeHTTP(w, r)

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("regression: master chain 503'd %q — session was not synthesized", w.Body.String())
	}
	if !reached {
		t.Fatalf("handler not reached: code=%d body=%q", w.Code, w.Body.String())
	}
}

// With the master cookie absent the operator acts as a plain master-role user
// (no envelope lookup). This also 503'd before the fix because the session
// guard runs before the cookie check.
func TestMasterChain_NoMasterCookie_ReachesHandler_Not503(t *testing.T) {
	masterID := uuid.New()
	reached := false
	next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	repo := &fakeImpRepo{}
	r := masterChainRequest(masterID, false)
	w := httptest.NewRecorder()
	masterChain(t, repo, masterID, next).ServeHTTP(w, r)

	if w.Code == http.StatusServiceUnavailable {
		t.Fatalf("regression: master chain 503'd %q with no master cookie", w.Body.String())
	}
	if !reached {
		t.Fatalf("handler not reached: code=%d", w.Code)
	}
}

// Active-envelope happy path: the synthesized session's UserID (=master.ID)
// must be the value ImpersonationFromSession passes to checker.IsMaster, so a
// non-expired envelope for a master swaps tenant context and reaches the
// handler.
func TestMasterChain_ActiveEnvelope_SwapsAndReaches(t *testing.T) {
	masterID := uuid.New()
	var sawImpersonation bool
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, sawImpersonation = middleware.ActiveImpersonation(r.Context())
	})

	// Envelope expires well after the fixed clock so step 4 (expiry) is not
	// taken; step 5 (role gate) consults checker.IsMaster(sess.UserID).
	repo := &fakeImpRepo{active: activeEnvelope(time.Date(2026, 6, 20, 13, 0, 0, 0, time.UTC))}
	r := masterChainRequest(masterID, true)
	w := httptest.NewRecorder()
	masterChain(t, repo, masterID, next).ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("active-envelope path: code=%d body=%q want 200", w.Code, w.Body.String())
	}
	if !sawImpersonation {
		t.Fatalf("active impersonation envelope was not attached to the handler context")
	}
}
