package customdomain_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"

	cd "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// fakeAudit records denied:rate_limited events for assertions.
type fakeAudit struct {
	events []management.AuditEvent
}

func (f *fakeAudit) LogManagement(_ context.Context, ev management.AuditEvent) {
	f.events = append(f.events, ev)
}

// ---- MemoryVerifyRateLimiter unit tests ----

func TestMemoryVerifyRateLimiter_AllowsUpToBurst(t *testing.T) {
	t.Parallel()
	now := time.Now()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{
		Rate:  cd.DefaultVerifyRate,
		Burst: 5,
		Now:   func() time.Time { return now },
	})
	tenant := uuid.New()
	for i := 0; i < 5; i++ {
		allowed, _ := lim.Allow(context.Background(), tenant, "1.2.3.4")
		if !allowed {
			t.Fatalf("request %d: expected allowed, got denied", i+1)
		}
	}
	// 6th request in same instant must be denied (burst exhausted)
	allowed, retry := lim.Allow(context.Background(), tenant, "1.2.3.4")
	if allowed {
		t.Fatal("6th request: expected denied after burst")
	}
	if retry <= 0 {
		t.Fatalf("retry = %v, want > 0", retry)
	}
}

func TestMemoryVerifyRateLimiter_NilTenantDenied(t *testing.T) {
	t.Parallel()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{})
	allowed, retry := lim.Allow(context.Background(), uuid.Nil, "1.2.3.4")
	if allowed {
		t.Fatal("nil tenant: expected denied")
	}
	if retry <= 0 {
		t.Fatalf("retry = %v, want > 0", retry)
	}
}

func TestMemoryVerifyRateLimiter_EmptyIPDenied(t *testing.T) {
	t.Parallel()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{})
	allowed, _ := lim.Allow(context.Background(), uuid.New(), "")
	if allowed {
		t.Fatal("empty IP: expected denied")
	}
}

func TestMemoryVerifyRateLimiter_BucketsArePerTenantIP(t *testing.T) {
	t.Parallel()
	now := time.Now()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{
		Rate:  cd.DefaultVerifyRate,
		Burst: 1,
		Now:   func() time.Time { return now },
	})
	t1, t2 := uuid.New(), uuid.New()
	// Exhaust t1's bucket
	lim.Allow(context.Background(), t1, "1.2.3.4")
	// t2 must still be allowed
	allowed, _ := lim.Allow(context.Background(), t2, "1.2.3.4")
	if !allowed {
		t.Fatal("different tenant should have independent bucket")
	}
	// Same tenant, different IP must still be allowed
	allowed, _ = lim.Allow(context.Background(), t1, "5.6.7.8")
	if !allowed {
		t.Fatal("same tenant, different IP should have independent bucket")
	}
}

func TestMemoryVerifyRateLimiter_SweepEvicts(t *testing.T) {
	t.Parallel()
	var ts time.Time
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{
		Rate:    cd.DefaultVerifyRate,
		Burst:   5,
		IdleTTL: time.Minute,
		Now:     func() time.Time { return ts },
	})
	ts = time.Now()
	lim.Allow(context.Background(), uuid.New(), "1.2.3.4")
	if lim.Len() != 1 {
		t.Fatalf("Len = %d, want 1", lim.Len())
	}
	ts = ts.Add(2 * time.Minute) // past IdleTTL
	removed := lim.Sweep()
	if removed != 1 {
		t.Fatalf("Sweep removed %d, want 1", removed)
	}
	if lim.Len() != 0 {
		t.Fatalf("Len after sweep = %d, want 0", lim.Len())
	}
}

// ---- VerifyRateLimitMiddleware integration tests ----

// newHandlerWithRateLimiter builds a handler with an active rate limiter so
// end-to-end 429 behaviour can be tested through the full mux.
func newHandlerWithRateLimiter(t *testing.T, uc *fakeUseCase, lim cd.VerifyRateLimiter, audit management.AuditLogger) *cd.Handler {
	t.Helper()
	fixedNow := func() time.Time { return time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC) }
	h, err := cd.New(cd.Config{
		UseCase:       uc,
		CSRF:          cd.CSRFConfig{Secret: []byte(testCSRFSecret)},
		PrimaryDomain: "exemplo.com",
		Now:           fixedNow,
		RateLimiter:   lim,
		Audit:         audit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestVerifyRateLimitMiddleware_11thRequestReturns429(t *testing.T) {
	t.Parallel()
	now := time.Now()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{
		Rate:  cd.DefaultVerifyRate, // 10 req/min
		Burst: 5,
		Now:   func() time.Time { return now },
	})
	audit := &fakeAudit{}
	uc := &fakeUseCase{verifyResp: management.VerifyOutcome{Verified: true}}
	h := newHandlerWithRateLimiter(t, uc, lim, audit)
	mux := newServeMux(h)

	domainID := uuid.New()
	target := "/api/customdomains/" + domainID.String() + "/verify"
	tenant := uuid.New()

	// First 5 requests (burst) must be allowed
	for i := 0; i < 5; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.RemoteAddr = "10.0.0.1:9999"
		req = withTenant(req, tenant)
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: got 429 before burst exhausted", i+1)
		}
	}

	// After burst is exhausted, next request must return 429
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.RemoteAddr = "10.0.0.1:9999"
	req = withTenant(req, tenant)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	retryAfter := rec.Header().Get("Retry-After")
	if retryAfter == "" {
		t.Fatal("Retry-After header missing on 429")
	}
	secs, err := strconv.Atoi(retryAfter)
	if err != nil || secs < 1 {
		t.Fatalf("Retry-After = %q, want integer >= 1", retryAfter)
	}
}

func TestVerifyRateLimitMiddleware_AuditEventOnDenial(t *testing.T) {
	t.Parallel()
	now := time.Now()
	// Burst=0 is not valid; use burst=1 and exhaust immediately.
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{
		Rate:  cd.DefaultVerifyRate,
		Burst: 1,
		Now:   func() time.Time { return now },
	})
	audit := &fakeAudit{}
	uc := &fakeUseCase{}
	h := newHandlerWithRateLimiter(t, uc, lim, audit)
	mux := newServeMux(h)

	tenant := uuid.New()
	domainID := uuid.New()
	target := "/api/customdomains/" + domainID.String() + "/verify"

	// Exhaust burst=1
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, target, nil)
	req.RemoteAddr = "192.168.0.1:1234"
	req = withTenant(req, tenant)
	mux.ServeHTTP(rec, req)

	// Second request triggers rate-limit denial
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, target, nil)
	req.RemoteAddr = "192.168.0.1:1234"
	req = withTenant(req, tenant)
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", rec.Code)
	}
	if len(audit.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(audit.events))
	}
	ev := audit.events[0]
	if ev.Outcome != "denied:rate_limited" {
		t.Errorf("Outcome = %q, want denied:rate_limited", ev.Outcome)
	}
	if ev.TenantID != tenant {
		t.Errorf("TenantID mismatch")
	}
}

func TestVerifyRateLimitMiddleware_NoLimiterPassesThrough(t *testing.T) {
	t.Parallel()
	uc := &fakeUseCase{verifyResp: management.VerifyOutcome{Verified: true}}
	// nil RateLimiter → middleware is skipped
	h := newHandlerForTest(t, uc)
	mux := newServeMux(h)

	domainID := uuid.New()
	target := "/api/customdomains/" + domainID.String() + "/verify"

	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, target, nil)
		req.RemoteAddr = "1.1.1.1:80"
		req = withTenant(req, testTenant)
		mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d: unexpected 429 with nil limiter", i+1)
		}
	}
}

func TestVerifyRateLimitMiddleware_NoTenantReturns401(t *testing.T) {
	t.Parallel()
	lim := cd.NewMemoryVerifyRateLimiter(cd.VerifyRateLimitConfig{})
	h := newHandlerWithRateLimiter(t, &fakeUseCase{}, lim, nil)
	mux := newServeMux(h)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/customdomains/"+uuid.New().String()+"/verify", nil)
	req.RemoteAddr = "1.2.3.4:80"
	// No tenant in context
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}
