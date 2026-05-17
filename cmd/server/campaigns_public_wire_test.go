package main

// SIN-62959 — composition-root tests for the public campaign redirect
// wire. The handler itself is covered exhaustively in
// internal/web/public/campaign; these tests pin the wire-level
// behaviour: env parsing, fail-soft when DB / Redis are absent, and
// the rate-limit middleware composition.

import (
	"log/slog"
	"reflect"
	"testing"

	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/campaigns"
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
