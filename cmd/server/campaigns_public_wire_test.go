package main

// SIN-62959 — composition-root tests for the public campaign redirect
// wire. The handler itself is covered exhaustively in
// internal/web/public/campaign; these tests pin the wire-level
// behaviour: env parsing, fail-soft when DB / Redis are absent, and
// the rate-limit middleware composition.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	"github.com/pericles-luz/crm/internal/campaigns"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
)

func TestBuildWebCampaignHandler_NilPoolOrRedis_ReturnsNil(t *testing.T) {
	t.Parallel()
	if h, err := buildWebCampaignHandler(nil, nil, func(string) string { return "" }); err != nil || h != nil {
		t.Fatalf("buildWebCampaignHandler(nil, nil) = (%v, %v), want (nil, nil)", h, err)
	}
}

func TestReadCampaignRatePerMin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  string
		want int
	}{
		{name: "unset → default", env: "", want: defaultCampaignRatePerMin},
		{name: "explicit", env: "250", want: 250},
		{name: "non-numeric → default", env: "abc", want: defaultCampaignRatePerMin},
		{name: "zero → default", env: "0", want: defaultCampaignRatePerMin},
		{name: "negative → default", env: "-5", want: defaultCampaignRatePerMin},
		{name: "huge → capped", env: "5000000", want: 1_000_000},
		{name: "padding tolerated", env: "  77  ", want: 77},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := readCampaignRatePerMin(func(string) string { return tc.env })
			if got != tc.want {
				t.Errorf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestParseAllowedHosts(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{name: "empty", raw: "", want: nil},
		{name: "whitespace only", raw: "  ", want: nil},
		{name: "single", raw: "wa.me", want: []string{"wa.me"}},
		{name: "multi", raw: "wa.me,t.me", want: []string{"wa.me", "t.me"}},
		{name: "padding", raw: "  wa.me , t.me ", want: []string{"wa.me", "t.me"}},
		{name: "trailing comma", raw: "wa.me,", want: []string{"wa.me"}},
		{name: "empty middle", raw: "wa.me,,t.me", want: []string{"wa.me", "t.me"}},
		{name: "wildcard", raw: "*.wa.me", want: []string{"*.wa.me"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := parseAllowedHosts(tc.raw)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCookieInsecure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  string
		want bool
	}{
		{name: "unset", env: "", want: false},
		{name: "1", env: "1", want: true},
		{name: "true", env: "true", want: true},
		{name: "TRUE", env: "TRUE", want: true},
		{name: "yes", env: "yes", want: true},
		{name: "0", env: "0", want: false},
		{name: "false", env: "false", want: false},
		{name: "anything else", env: "maybe", want: false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := cookieInsecure(func(string) string { return tc.env })
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAssembleCampaignHandler_RejectsNilRepo(t *testing.T) {
	t.Parallel()
	if _, err := assembleCampaignHandler(nil, nil, true, nil); err == nil {
		t.Fatalf("assembleCampaignHandler(nil) err = nil, want non-nil")
	}
}

func TestBuildCampaignLinker_NilPool(t *testing.T) {
	t.Parallel()
	linker, err := buildCampaignLinker(nil)
	if err != nil {
		t.Fatalf("buildCampaignLinker(nil) err = %v, want nil", err)
	}
	if linker != nil {
		t.Fatalf("buildCampaignLinker(nil) = %v, want nil", linker)
	}
}

func TestAssembleCampaignHandler_HappyPath(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	h, err := assembleCampaignHandler(repo, []string{"wa.me"}, true, slog.Default())
	if err != nil {
		t.Fatalf("assembleCampaignHandler: %v", err)
	}
	if h == nil {
		t.Fatalf("assembleCampaignHandler returned nil handler")
	}
}

func TestBuildCampaignRateLimitMiddleware_ValidatesPolicy(t *testing.T) {
	t.Parallel()
	// A goredis.Client constructed with an unreachable address still
	// builds and satisfies the rlredis adapter contract — the limiter
	// only dials on Allow(). We exercise the wire-up path (policy
	// build + middleware wrap) without booting Redis.
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:0"})
	defer rdb.Close()
	mw, err := buildCampaignRateLimitMiddleware(rdb, 100, slog.Default())
	if err != nil {
		t.Fatalf("buildCampaignRateLimitMiddleware: %v", err)
	}
	if mw == nil {
		t.Fatalf("buildCampaignRateLimitMiddleware returned nil middleware")
	}
}

// TestCampaignPublicRateLimit_SpoofedTrueClientIPDoesNotBypass is the
// AC #2 regression test for SIN-62978: 200 requests from the same TCP
// peer with a forged True-Client-IP per request MUST NOT bypass the
// 100/min/IP rate limit on GET /c/{slug}. We exercise the full chain
// (trusted-proxy wrapper + rate-limit middleware + handler) against an
// in-memory limiter so the test is fast and hermetic.
//
// The trusted-proxy wrapper sits in front of the rate-limit middleware
// in production via the chi router; here we stitch the same envelope
// manually so the wire-level test does not need a full httpapi.NewRouter.
func TestCampaignPublicRateLimit_SpoofedTrueClientIPDoesNotBypass(t *testing.T) {
	t.Parallel()
	cap := 100
	limiter := &countingLimiter{cap: cap}

	rate, err := domainratelimit.NewPolicy(
		"campaign_click_test",
		[]domainratelimit.Bucket{
			{Name: "ip", Window: time.Minute, Max: cap},
		},
		domainratelimit.Lockout{},
	)
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	rl, err := httpratelimit.New(httpratelimit.Config{
		Policy:  rate,
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
		},
	})
	if err != nil {
		t.Fatalf("httpratelimit.New: %v", err)
	}

	inner := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusFound)
	})

	// Default trusted-proxy CIDRs cover loopback, so a 198.51.100.0/24
	// peer (TEST-NET-2) is untrusted — exactly the threat model.
	trusted := httpapi.NewTrustedRealIP(func(string) string { return "" })
	chain := trusted(rl(inner))

	allowed := 0
	throttled := 0
	for i := 1; i <= 200; i++ {
		req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
		req.RemoteAddr = "198.51.100.10:55555" // SAME peer every time
		// Attacker varies True-Client-IP to forge per-IP bucket key.
		req.Header.Set("True-Client-IP", fmt.Sprintf("203.0.113.%d", i%254+1))
		w := httptest.NewRecorder()
		chain.ServeHTTP(w, req)
		switch w.Code {
		case http.StatusFound:
			allowed++
		case http.StatusTooManyRequests:
			throttled++
		}
	}

	// Cap is 100; 200 attempts → ~100 allowed, ~100 throttled. We
	// require at least cap throttled responses to prove the spoofed
	// header did NOT yield a fresh bucket per request.
	if throttled < 90 {
		t.Fatalf("throttled=%d, allowed=%d — spoofed True-Client-IP appears to bypass per-IP cap (want ≥90 throttled out of 200)", throttled, allowed)
	}
	if allowed > cap+10 { // allow small jitter for window edges
		t.Fatalf("allowed=%d, want ≈%d — bucket cap not respected", allowed, cap)
	}
}

// countingLimiter is a tiny in-memory implementation of the
// domainratelimit.RateLimiter port. It counts hits per key inside the
// supplied window; the wire-side test only needs a single-window
// snapshot, so the implementation is deliberately minimal and
// non-sliding.
type countingLimiter struct {
	mu    sync.Mutex
	cap   int
	count map[string]int
}

func (c *countingLimiter) Allow(_ context.Context, key string, window time.Duration, max int) (bool, time.Duration, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == nil {
		c.count = map[string]int{}
	}
	c.count[key]++
	if c.count[key] > max {
		return false, window, nil
	}
	return true, 0, nil
}

// TestIAMRoutesIncludesCampaignPublic pins the stdlib-mux dispatch
// path: the public mux delegates "/c/" to the chi router, which then
// re-matches "GET /c/{slug}" inside the tenanted group. If a future
// refactor drops "/c/" from iamRoutes, the route silently falls
// through to the custom-domain catch-all instead of serving the
// campaign redirect — a regression this assertion catches.
func TestIAMRoutesIncludesCampaignPublic(t *testing.T) {
	t.Parallel()
	for _, r := range iamRoutes {
		if r == "/c/" {
			return
		}
	}
	t.Fatalf("iamRoutes does not contain /c/ — the SIN-62959 mount would be unreachable")
}
