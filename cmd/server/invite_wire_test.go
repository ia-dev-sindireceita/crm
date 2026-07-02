package main

// SIN-66510 — composition-root tests for the public invite / set-password
// wire. The handler and domain are covered exhaustively in
// internal/web/invite and internal/tenantusers; these tests pin the
// wire-level behaviour: env parsing, token-prefix extractor, fail-soft when
// DB / Redis are absent, assembly, and the iamRoutes delegation prefix.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
)

// stubCredRepo is an in-memory CredentialTokenRepository for the assembly test.
type stubCredRepo struct {
	inv tenantusers.Invite
	err error
}

func (s stubCredRepo) LookupToken(context.Context, uuid.UUID, []byte, time.Time) (tenantusers.Invite, error) {
	return s.inv, s.err
}

func (s stubCredRepo) ConsumeToken(context.Context, uuid.UUID, []byte, time.Time, string) (uuid.UUID, error) {
	return s.inv.UserID, s.err
}

func TestBuildWebInviteHandler_NilDeps(t *testing.T) {
	// Nil pool or nil redis → skip mounting (no rate limiter ⇒ never ship the
	// public endpoint unprotected).
	if h, err := buildWebInviteHandler(nil, nil, func(string) string { return "" }); h != nil || err != nil {
		t.Fatalf("nil deps: h=%v err=%v", h, err)
	}
}

func TestAssembleWebInviteHandler_Serves(t *testing.T) {
	repo := stubCredRepo{inv: tenantusers.Invite{UserID: uuid.New(), Email: "u@ex.com", Purpose: tenantusers.PurposeInvite}}
	h, err := assembleWebInviteHandler(repo, stubAuditor{}, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "Acme", Host: "acme.crm.local"}
	req := httptest.NewRequest(http.MethodGet, "/invite/sometoken", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET code=%d", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "u@ex.com") {
		t.Fatalf("email not rendered")
	}
}

func TestBuildInviteRateLimitMiddleware_ValidatesPolicy(t *testing.T) {
	// A goredis.Client with an unreachable address still constructs; the
	// middleware builder only validates the policy + bucket wiring (no Redis
	// round-trip until a request lands), so this exercises the two-bucket
	// (ip + token) composition without a live Redis.
	rdb := goredis.NewClient(&goredis.Options{Addr: "127.0.0.1:0"})
	t.Cleanup(func() { _ = rdb.Close() })

	mw, err := buildInviteRateLimitMiddleware(rdb, defaultInviteRatePerMin, slog.Default())
	if err != nil {
		t.Fatalf("buildInviteRateLimitMiddleware: %v", err)
	}
	if mw == nil {
		t.Fatal("nil middleware")
	}
	// The wrapped handler is a valid http.Handler (fail-open on the limiter's
	// connect error means the inner handler still runs).
	wrapped := mw(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rr := httptest.NewRecorder()
	wrapped.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/invite/tok", nil))
	if rr.Code != http.StatusTeapot {
		t.Fatalf("wrapped handler code = %d, want fail-open passthrough", rr.Code)
	}
}

func TestBuildInvitePasswordPolicy(t *testing.T) {
	p, err := buildInvitePasswordPolicy(slog.Default())
	if err != nil {
		t.Fatalf("buildInvitePasswordPolicy: %v", err)
	}
	if p == nil || p.LocalList == nil {
		t.Fatalf("policy = %+v, want non-nil with LocalList wired", p)
	}
}

func TestReadInviteRatePerMin(t *testing.T) {
	tests := map[string]struct {
		raw  string
		want int
	}{
		"unset":      {"", defaultInviteRatePerMin},
		"valid":      {"7", 7},
		"whitespace": {"  9 ", 9},
		"zero":       {"0", defaultInviteRatePerMin},
		"negative":   {"-3", defaultInviteRatePerMin},
		"nonnumeric": {"abc", defaultInviteRatePerMin},
		"capped":     {"9999999", 1_000_000},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			got := readInviteRatePerMin(func(string) string { return tc.raw })
			if got != tc.want {
				t.Fatalf("raw=%q got=%d want=%d", tc.raw, got, tc.want)
			}
		})
	}
}

func TestInviteTokenPrefixExtractor(t *testing.T) {
	long := strings.Repeat("a", inviteTokenPrefixLen+30)
	tests := map[string]struct {
		path string
		want string
	}{
		"short token":      {"/invite/abc", "abc"},
		"long token trunc": {"/invite/" + long, long[:inviteTokenPrefixLen]},
		"trailing segment": {"/invite/tok/extra", "tok"},
		"missing token":    {"/invite/", ""},
		"not an invite":    {"/login", ""},
		"empty path":       {"", ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "http://acme.crm.local"+nonEmptyPath(tc.path), nil)
			if tc.path == "" {
				req.URL.Path = ""
			}
			got := inviteTokenPrefixExtractor(req)
			if got != tc.want {
				t.Fatalf("path=%q got=%q want=%q", tc.path, got, tc.want)
			}
		})
	}
	// nil request → empty key (defensive).
	if got := inviteTokenPrefixExtractor(nil); got != "" {
		t.Fatalf("nil req got=%q", got)
	}
}

func nonEmptyPath(p string) string {
	if p == "" {
		return "/"
	}
	return p
}

// TestIAMRoutesIncludesInvite pins the stdlib-mux delegation prefix. Without
// "/invite/" in iamRoutes the public mux would let the custom-domain catch-all
// at "/" shadow the page and it would 404 (same failure class as SIN-64973 /
// SIN-64977).
func TestIAMRoutesIncludesInvite(t *testing.T) {
	for _, r := range iamRoutes {
		if r == "/invite/" {
			return
		}
	}
	t.Fatalf("iamRoutes does not contain /invite/ — the SIN-66510 mount would be unreachable")
}
