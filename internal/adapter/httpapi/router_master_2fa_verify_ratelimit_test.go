package httpapi_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// SIN-62380 (CAVEAT-3): the m_2fa_verify policy is mounted on POST
// /m/2fa/verify with the session and user extractors from the
// mastermfa package. The session bucket is 3/min, the user bucket is
// 10/h.

// masterAuthInjector is a passthrough middleware that simulates
// RequireMasterAuth: it stamps a Master into the context using the
// per-test fixed UUID so the user-bucket extractor has a stable key.
func masterAuthInjector(uid uuid.UUID) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r = r.WithContext(mastermfa.WithMaster(r.Context(), mastermfa.Master{ID: uid, Email: "ops@example.com"}))
			next.ServeHTTP(w, r)
		})
	}
}

// newRateLimitedMasterRouter wires a master router with the
// m_2fa_verify policy active. The verify handler is the same `nopH`
// 200-OK passthrough used elsewhere — the rate-limit middleware is
// the only thing under test.
func newRateLimitedMasterRouter(t *testing.T, uid uuid.UUID, denyOn func(policy, bucket, key string, retryAfter time.Duration)) (http.Handler, *inmemSlidingWindow) {
	t.Helper()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: uuid.New(), Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": tenants["acme.crm.local"].ID}
	store := newInmemIAM(tenantIDs)
	policies, err := domainratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	limiter := newInmemSlidingWindow()
	md := stubMasterDeps()
	md.RequireMasterAuth = masterAuthInjector(uid)
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:                 store,
		TenantResolver:      &fakeResolver{byHost: tenants},
		Master:              md,
		Policies:            policies,
		RateLimiter:         limiter,
		RateLimitDenyMetric: denyOn,
	})
	return r, limiter
}

func postVerifyRateLimited(t *testing.T, h http.Handler, sessionID string) *httptest.ResponseRecorder {
	t.Helper()
	body := url.Values{"code": {"000000"}}
	req := httptest.NewRequest(http.MethodPost, "/m/2fa/verify", strings.NewReader(body.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sessionID})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRouter_Master2FAVerify_PerSessionRateLimit pins the SIN-62380
// session-bucket cap: four POSTs from the same session id within the
// 1m window must 429 the fourth attempt.
func TestRouter_Master2FAVerify_PerSessionRateLimit(t *testing.T) {
	t.Parallel()

	type denyEvent struct {
		policy, bucket string
		retryAfter     time.Duration
	}
	var denied []denyEvent
	onDeny := func(p, b, _ string, ra time.Duration) {
		denied = append(denied, denyEvent{policy: p, bucket: b, retryAfter: ra})
	}

	uid := uuid.New()
	h, _ := newRateLimitedMasterRouter(t, uid, onDeny)

	sid := uuid.New().String()
	for i := 1; i <= 3; i++ {
		rec := postVerifyRateLimited(t, h, sid)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=429 prematurely; want <429 (session cap is 3)", i)
		}
	}

	rec := postVerifyRateLimited(t, h, sid)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("4th attempt: status=%d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatalf("4th attempt: missing Retry-After header")
	}
	if raSec, err := strconv.Atoi(ra); err != nil || raSec < 1 {
		t.Fatalf("4th attempt: Retry-After=%q (parsed=%d, err=%v), want integer >=1", ra, raSec, err)
	}
	if len(denied) != 1 {
		t.Fatalf("denied=%d, want 1", len(denied))
	}
	if got := denied[0]; got.policy != "m_2fa_verify" || got.bucket != "session" {
		t.Fatalf("denied=%+v, want policy=m_2fa_verify bucket=session", got)
	}
}

// TestRouter_Master2FAVerify_PerUserRateLimit pins the user-bucket
// cap: 11 POSTs across distinct sessions for the same user must trip
// the per-user 10/h ceiling. The session cap is 3/min so the test
// rotates session ids per attempt to bypass it.
//
// We can't easily exhaust the per-hour limit without a clock seam in
// the in-memory limiter, so this test exercises the lower per-window
// path: 11 distinct sessions in <1m, the 11th attempt trips the user
// bucket since the 10/h window is already 10 hits deep.
func TestRouter_Master2FAVerify_PerUserRateLimit(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	h, _ := newRateLimitedMasterRouter(t, uid, nil)

	// Each attempt has a distinct session id so the session bucket
	// never trips. Distinct sids also means the rate-limit middleware
	// always evaluates the user bucket (the only one present across
	// requests).
	for i := 1; i <= 10; i++ {
		sid := uuid.New().String()
		rec := postVerifyRateLimited(t, h, sid)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=429 prematurely; want <429 (user cap is 10/h)", i)
		}
	}

	// 11th attempt with a fresh session id must trip the user bucket.
	rec := postVerifyRateLimited(t, h, uuid.New().String())
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: status=%d, want 429", rec.Code)
	}
}

// TestRouter_Master2FAVerify_GetIsNotRateLimited pins that the
// rate-limit middleware is mounted on POST only. The HTML form must
// remain reachable on GET so a legitimate user who refreshes the
// page after a failed submission gets the form back.
func TestRouter_Master2FAVerify_GetIsNotRateLimited(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	h, _ := newRateLimitedMasterRouter(t, uid, nil)
	sid := uuid.New().String()

	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/m/2fa/verify", nil)
		req.AddCookie(&http.Cookie{Name: sessioncookie.NameMaster, Value: sid})
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("GET attempt %d: unexpectedly 429", i)
		}
	}
}

// TestRouter_Master2FAVerify_NoRateLimitWhenDepsOmitted is the back-
// compat pin: existing master-router wireups that don't pass
// Policies + RateLimiter must keep working unchanged. Other PRs in
// SIN-62343 may add the SQL/iam-side controls; the HTTP-boundary
// rate limit is opt-in via Deps.
func TestRouter_Master2FAVerify_NoRateLimitWhenDepsOmitted(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: uuid.New(), Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": tenants["acme.crm.local"].ID}
	store := newInmemIAM(tenantIDs)
	md := stubMasterDeps()
	md.RequireMasterAuth = masterAuthInjector(uid)
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		Master:         md,
		// Policies / RateLimiter omitted — the verify route must remain
		// un-throttled at the HTTP boundary.
	})
	sid := uuid.New().String()
	for i := 0; i < 20; i++ {
		rec := postVerifyRateLimited(t, h, sid)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("iter %d: unexpected 429 — rate limit must be absent without Policies/RateLimiter", i)
		}
	}
}

// TestRouter_Master2FAVerify_NoRateLimitWhenPolicyMissing is the
// graceful-skip pin: a Policies map that does not contain
// m_2fa_verify must NOT panic — older config snapshots may pre-date
// the policy entry. The login policy still installs (its build helper
// panics on a missing key, which is the right behaviour for the
// long-installed login route).
func TestRouter_Master2FAVerify_NoRateLimitWhenPolicyMissing(t *testing.T) {
	t.Parallel()

	uid := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: uuid.New(), Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": tenants["acme.crm.local"].ID}
	store := newInmemIAM(tenantIDs)
	policies, err := domainratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	delete(policies, "m_2fa_verify")
	md := stubMasterDeps()
	md.RequireMasterAuth = masterAuthInjector(uid)
	h := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		Master:         md,
		Policies:       policies,
		RateLimiter:    newInmemSlidingWindow(),
	})
	sid := uuid.New().String()
	for i := 0; i < 5; i++ {
		rec := postVerifyRateLimited(t, h, sid)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("iter %d: unexpected 429 with m_2fa_verify policy missing", i)
		}
	}
}
