package ratelimit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// fakeLimiter is a deterministic ratelimit.RateLimiter test double.
// Each call drains one entry from `responses` (in order). If no
// entries remain it returns an error so the test fails loudly rather
// than masking a missing expectation.
type fakeLimiter struct {
	responses []limiterResp
	calls     []limiterCall
	idx       int32
}

type limiterCall struct {
	key    string
	window time.Duration
	max    int
}

type limiterResp struct {
	allowed    bool
	retryAfter time.Duration
	err        error
}

func (f *fakeLimiter) Allow(_ context.Context, key string, window time.Duration, max int) (bool, time.Duration, error) {
	idx := atomic.AddInt32(&f.idx, 1) - 1
	f.calls = append(f.calls, limiterCall{key: key, window: window, max: max})
	if int(idx) >= len(f.responses) {
		return false, 0, errors.New("fakeLimiter: more calls than responses configured")
	}
	r := f.responses[idx]
	return r.allowed, r.retryAfter, r.err
}

// passingHandler is the protected handler — sets a sentinel header so
// tests can detect "request reached the handler".
var passingHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Reached", "yes")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
})

func mustNew(t *testing.T, cfg httpratelimit.Config) func(http.Handler) http.Handler {
	t.Helper()
	mw, err := httpratelimit.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return mw
}

// loginPolicy is the SIN-62341 login policy without going through the
// DefaultPolicies path (so the test does not depend on those numbers).
func loginPolicy(t *testing.T) ratelimit.Policy {
	t.Helper()
	p, err := ratelimit.NewPolicy("login", []ratelimit.Bucket{
		{Name: "ip", Window: time.Minute, Max: 5},
		{Name: "email", Window: time.Hour, Max: 10},
	}, ratelimit.Lockout{Threshold: 10, Duration: 15 * time.Minute})
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	return p
}

func TestMiddleware_AllowsBelowCap(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{
		responses: []limiterResp{
			{allowed: true},
			{allowed: true},
		},
	}
	mw := mustNew(t, httpratelimit.Config{
		Policy:  loginPolicy(t),
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=alice@a.test"))
	req.RemoteAddr = "1.2.3.4:5555"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	mw(passingHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Reached") != "yes" {
		t.Fatal("handler not reached")
	}
	if got := len(limiter.calls); got != 2 {
		t.Fatalf("limiter calls = %d, want 2", got)
	}
	if limiter.calls[0].key != "login:ip:1.2.3.4" {
		t.Fatalf("ip bucket key = %q", limiter.calls[0].key)
	}
	if limiter.calls[1].key != "login:email:alice@a.test" {
		t.Fatalf("email bucket key = %q", limiter.calls[1].key)
	}
}

func TestMiddleware_ThrottlesAndSetsRetryAfter(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{
		responses: []limiterResp{
			{allowed: false, retryAfter: 7 * time.Second},
		},
	}
	var denyCalls int32
	mw := mustNew(t, httpratelimit.Config{
		Policy:  loginPolicy(t),
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
		OnDeny: func(_, _, _ string, _ time.Duration) { atomic.AddInt32(&denyCalls, 1) },
	})

	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "1.2.3.4:80"
	rec := httptest.NewRecorder()
	mw(passingHandler).ServeHTTP(rec, req)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "7" {
		t.Fatalf("Retry-After = %q, want 7", got)
	}
	if rec.Header().Get("X-Reached") != "" {
		t.Fatal("handler reached despite 429")
	}
	if atomic.LoadInt32(&denyCalls) != 1 {
		t.Fatalf("OnDeny calls = %d, want 1", denyCalls)
	}
	if got := len(limiter.calls); got != 1 {
		t.Fatalf("limiter calls = %d, want 1 (short-circuit on first throttled bucket)", got)
	}
}

func TestMiddleware_RetryAfterRoundsUpAndFloorsAt1(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		retryAfter time.Duration
		want       string
	}{
		{"zero floors at 1", 0, "1"},
		{"100ms rounds up to 1", 100 * time.Millisecond, "1"},
		{"1500ms rounds up to 2", 1500 * time.Millisecond, "2"},
		{"5s exact", 5 * time.Second, "5"},
		{"negative floors at 1", -time.Second, "1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limiter := &fakeLimiter{
				responses: []limiterResp{{allowed: false, retryAfter: tc.retryAfter}},
			}
			mw := mustNew(t, httpratelimit.Config{
				Policy:  loginPolicy(t),
				Limiter: limiter,
				Buckets: []httpratelimit.Bucket{
					{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
					{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			req.RemoteAddr = "1.2.3.4:80"
			rec := httptest.NewRecorder()
			mw(passingHandler).ServeHTTP(rec, req)
			if got := rec.Header().Get("Retry-After"); got != tc.want {
				t.Fatalf("Retry-After = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMiddleware_FailsOpenOnLimiterError(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{
		responses: []limiterResp{
			{err: errors.New("redis down")},
			{allowed: true},
		},
	}
	mw := mustNew(t, httpratelimit.Config{
		Policy:  loginPolicy(t),
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=a@b.c"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = "1.2.3.4:80"
	rec := httptest.NewRecorder()
	mw(passingHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fail-open)", rec.Code)
	}
}

func TestMiddleware_SkipsBucketWithEmptyExtractor(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{
		responses: []limiterResp{{allowed: true}},
	}
	mw := mustNew(t, httpratelimit.Config{
		Policy:  loginPolicy(t),
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			// email extractor returns "" because no body field
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/login", nil)
	req.RemoteAddr = "1.2.3.4:80"
	rec := httptest.NewRecorder()
	mw(passingHandler).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := len(limiter.calls); got != 1 {
		t.Fatalf("limiter calls = %d, want 1 (email skipped)", got)
	}
}

func TestNew_RejectsBadConfig(t *testing.T) {
	t.Parallel()
	good := loginPolicy(t)

	cases := []struct {
		name string
		cfg  httpratelimit.Config
		want string
	}{
		{
			"nil limiter",
			httpratelimit.Config{Policy: good, Buckets: []httpratelimit.Bucket{{Name: "ip", Extractor: httpratelimit.IPKeyExtractor}}},
			"nil Limiter",
		},
		{
			"empty policy",
			httpratelimit.Config{
				Policy:  ratelimit.Policy{Name: "x"},
				Limiter: &fakeLimiter{},
				Buckets: []httpratelimit.Bucket{{Name: "ip", Extractor: httpratelimit.IPKeyExtractor}},
			},
			"has no buckets",
		},
		{
			"no extractors",
			httpratelimit.Config{Policy: good, Limiter: &fakeLimiter{}},
			"has no extractors",
		},
		{
			"unknown bucket name",
			httpratelimit.Config{
				Policy:  good,
				Limiter: &fakeLimiter{},
				Buckets: []httpratelimit.Bucket{
					{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
					{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
					{Name: "session", Extractor: httpratelimit.FormFieldExtractor("session")},
				},
			},
			"does not declare bucket \"session\"",
		},
		{
			"nil extractor",
			httpratelimit.Config{
				Policy:  good,
				Limiter: &fakeLimiter{},
				Buckets: []httpratelimit.Bucket{
					{Name: "ip", Extractor: nil},
					{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
				},
			},
			"nil extractor",
		},
		{
			"duplicate bucket name",
			httpratelimit.Config{
				Policy:  good,
				Limiter: &fakeLimiter{},
				Buckets: []httpratelimit.Bucket{
					{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
					{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
				},
			},
			"wired twice",
		},
		{
			"missing extractor for declared bucket",
			httpratelimit.Config{
				Policy:  good,
				Limiter: &fakeLimiter{},
				Buckets: []httpratelimit.Bucket{
					{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
					// email missing
				},
			},
			"has no extractor",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := httpratelimit.New(tc.cfg)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Extractors
// ---------------------------------------------------------------------------

func TestIPKeyExtractor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		addr string
		want string
	}{
		{"v4 with port", "1.2.3.4:5555", "1.2.3.4"},
		{"v6 with port", "[::1]:1234", "::1"},
		{"v4 no port", "1.2.3.4", "1.2.3.4"},
		{"v6 no brackets", "::1", "::1"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.addr
			if got := httpratelimit.IPKeyExtractor(req); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got := httpratelimit.IPKeyExtractor(nil); got != "" {
		t.Fatalf("nil request returned %q, want empty", got)
	}
}

func TestFormFieldExtractor(t *testing.T) {
	t.Parallel()
	ex := httpratelimit.FormFieldExtractor("email")
	req := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader("email=alice@a.test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if got := ex(req); got != "alice@a.test" {
		t.Fatalf("got %q, want alice@a.test", got)
	}
	// No body
	empty := ex(httptest.NewRequest(http.MethodGet, "/", nil))
	if empty != "" {
		t.Fatalf("missing field should yield empty, got %q", empty)
	}
	// nil request
	if ex(nil) != "" {
		t.Fatal("nil request should yield empty")
	}
}

type ctxKeyType struct{}

func TestContextStringExtractor(t *testing.T) {
	t.Parallel()
	key := ctxKeyType{}
	ex := httpratelimit.ContextStringExtractor(key)

	// Present
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	ctx := context.WithValue(req.Context(), key, "session-abc")
	got := ex(req.WithContext(ctx))
	if got != "session-abc" {
		t.Fatalf("got %q, want session-abc", got)
	}

	// Missing
	if got := ex(req); got != "" {
		t.Fatalf("missing value got %q, want empty", got)
	}
	// Wrong type
	wrongCtx := context.WithValue(req.Context(), key, 42)
	if got := ex(req.WithContext(wrongCtx)); got != "" {
		t.Fatalf("wrong-type value got %q, want empty", got)
	}
	// nil request
	if ex(nil) != "" {
		t.Fatal("nil request must yield empty")
	}
}

// Cross-check that the Retry-After we render parses back as the integer
// the spec mandates (RFC 7231 §7.1.3).
func TestMiddleware_RetryAfterIsParseableInteger(t *testing.T) {
	t.Parallel()
	limiter := &fakeLimiter{responses: []limiterResp{{allowed: false, retryAfter: 10 * time.Second}}}
	mw := mustNew(t, httpratelimit.Config{
		Policy:  loginPolicy(t),
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = "1.2.3.4:80"
	rec := httptest.NewRecorder()
	mw(passingHandler).ServeHTTP(rec, req)
	got := rec.Header().Get("Retry-After")
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("Retry-After %q not an integer: %v", got, err)
	}
	if n != 10 {
		t.Fatalf("Retry-After = %d, want 10", n)
	}
}
