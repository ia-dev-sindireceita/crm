package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// inmemSlidingWindow is a process-local sliding-window RateLimiter
// matching the contract of the production redis adapter (ADR 0073
// §D4): per-key timestamp list, trim entries older than now-window,
// reject when len ≥ max, return retryAfter as the time until the
// oldest in-window entry ages out.
//
// It is deliberately a real in-memory adapter — not a "respond N
// times then fail" mock — so the integration tests below exercise the
// actual hit-counting logic the middleware relies on. Per quality-bar
// rule 5 the database is never mocked; Redis is not the database, but
// this adapter still matches production semantics so the test is
// honest about what it is checking.
type inmemSlidingWindow struct {
	mu   sync.Mutex
	hits map[string][]time.Time
	now  func() time.Time
}

func newInmemSlidingWindow() *inmemSlidingWindow {
	return &inmemSlidingWindow{
		hits: map[string][]time.Time{},
		now:  time.Now,
	}
}

func (s *inmemSlidingWindow) Allow(_ context.Context, key string, window time.Duration, max int) (bool, time.Duration, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	cutoff := now.Add(-window)
	hits := s.hits[key]
	pruned := hits[:0]
	for _, h := range hits {
		if h.After(cutoff) {
			pruned = append(pruned, h)
		}
	}
	if len(pruned) >= max {
		// retryAfter = window − (now − oldest).
		retryAfter := window - now.Sub(pruned[0])
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		s.hits[key] = pruned
		return false, retryAfter, nil
	}
	pruned = append(pruned, now)
	s.hits[key] = pruned
	return true, 0, nil
}

func newRateLimitedRouter(t *testing.T, denyOn func(policy, bucket, key string, retryAfter time.Duration)) (http.Handler, *inmemIAM, *inmemSlidingWindow) {
	t.Helper()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw-alice", uuid.New())

	resolver := &fakeResolver{byHost: tenants}

	policies, err := domainratelimit.DefaultPolicies()
	if err != nil {
		t.Fatalf("DefaultPolicies: %v", err)
	}
	limiter := newInmemSlidingWindow()

	r := httpapi.NewRouter(httpapi.Deps{
		IAM:                 store,
		TenantResolver:      resolver,
		Policies:            policies,
		RateLimiter:         limiter,
		RateLimitDenyMetric: denyOn,
	})
	return r, store, limiter
}

// postLogin issues POST /login against host with the given body and a
// fixed RemoteAddr. Returns the recorder so tests can assert status,
// headers, and cookies.
func postLogin(t *testing.T, h http.Handler, host, remoteAddr string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
	req.Host = host
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRouter_LoginPost_PerIPRateLimit pins SIN-62376 acceptance #1:
// six POST /login from the same IP within the policy window must
// 429 the sixth attempt with a Retry-After ≥ 1. The first five are
// allowed (the policy max is 5/min/IP).
func TestRouter_LoginPost_PerIPRateLimit(t *testing.T) {
	t.Parallel()

	type denyEvent struct {
		policy, bucket string
		retryAfter     time.Duration
	}
	var denied []denyEvent
	onDeny := func(p, b, _ string, ra time.Duration) {
		denied = append(denied, denyEvent{policy: p, bucket: b, retryAfter: ra})
	}

	h, _, _ := newRateLimitedRouter(t, onDeny)

	const sameIP = "203.0.113.7:54321"
	form := func(email string) url.Values {
		v := url.Values{}
		v.Set("email", email)
		v.Set("password", "wrong")
		return v
	}

	for i := 1; i <= 5; i++ {
		// Cycle the email so the per-email bucket does not run dry
		// before we hit the per-IP limit. Per-IP cap is 5/min, per-
		// email is 10/h — at 5 distinct emails neither will trip
		// before the 6th attempt.
		email := "alice" + strconv.Itoa(i) + "@acme.test"
		rec := postLogin(t, h, "acme.crm.local", sameIP, form(email))
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=429 prematurely; want <429 (per-IP cap is 5)", i)
		}
	}
	if len(denied) != 0 {
		t.Fatalf("denied=%d after 5 attempts, want 0", len(denied))
	}

	rec := postLogin(t, h, "acme.crm.local", sameIP, form("alice6@acme.test"))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: status=%d, want 429", rec.Code)
	}
	ra := rec.Header().Get("Retry-After")
	if ra == "" {
		t.Fatalf("6th attempt: missing Retry-After header")
	}
	raSec, err := strconv.Atoi(ra)
	if err != nil {
		t.Fatalf("6th attempt: Retry-After=%q not an integer: %v", ra, err)
	}
	if raSec < 1 {
		t.Fatalf("6th attempt: Retry-After=%d, want >= 1", raSec)
	}

	if len(denied) != 1 {
		t.Fatalf("denied=%d, want 1", len(denied))
	}
	got := denied[0]
	if got.policy != "login" || got.bucket != "ip" {
		t.Fatalf("denied=%+v, want policy=login bucket=ip", got)
	}
	if got.retryAfter <= 0 {
		t.Fatalf("denied retryAfter=%s, want > 0", got.retryAfter)
	}

	if cs := rec.Result().Cookies(); len(cs) != 0 {
		t.Fatalf("429 response has %d cookies, want 0", len(cs))
	}
}

// TestRouter_LoginPost_PerEmailRateLimit pins SIN-62376 acceptance #2:
// 11 POST /login on the same email from distinct IPs within the
// per-email window must 429 the 11th attempt. The policy max is
// 10/hour for the email bucket.
func TestRouter_LoginPost_PerEmailRateLimit(t *testing.T) {
	t.Parallel()

	h, _, _ := newRateLimitedRouter(t, nil)

	const sameEmail = "victim@acme.test"
	form := url.Values{}
	form.Set("email", sameEmail)
	form.Set("password", "wrong")

	for i := 1; i <= 10; i++ {
		ip := "198.51.100." + strconv.Itoa(i) + ":12345"
		rec := postLogin(t, h, "acme.crm.local", ip, form)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d: status=429 prematurely; want <429 (per-email cap is 10)", i)
		}
	}

	// 11th attempt from a fresh IP must trip the per-email bucket.
	rec := postLogin(t, h, "acme.crm.local", "198.51.100.99:54321", form)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("11th attempt: status=%d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatalf("11th attempt: missing Retry-After header")
	}
}

// TestRouter_LoginPost_NoRateLimitWhenDepsOmitted is the back-compat
// pin: existing wireups that don't pass Policies + RateLimiter must
// keep working unchanged (the lockout in iam.Service.Login is the
// only defence in that mode).
func TestRouter_LoginPost_NoRateLimitWhenDepsOmitted(t *testing.T) {
	t.Parallel()
	h, _, _ := newRouter(t)

	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "wrong")
	for i := 0; i < 20; i++ {
		rec := postLogin(t, h, "acme.crm.local", "203.0.113.10:5555", form)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("iter %d: unexpected 429 — rate limit is supposed to be absent without Policies/RateLimiter", i)
		}
	}
}

// TestRouter_LoginPost_RateLimitMissingLoginPolicy_Panics defends the
// programmer-error path: a non-nil Policies map without the "login"
// key MUST panic at NewRouter time so a misconfiguration is surfaced
// at boot, not on the first throttled request.
func TestRouter_LoginPost_RateLimitMissingLoginPolicy_Panics(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic when Policies map omits 'login'")
		}
	}()

	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: uuid.New(), Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": tenants["acme.crm.local"].ID}
	store := newInmemIAM(tenantIDs)

	httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		Policies:       map[string]domainratelimit.Policy{},
		RateLimiter:    newInmemSlidingWindow(),
	})
}
