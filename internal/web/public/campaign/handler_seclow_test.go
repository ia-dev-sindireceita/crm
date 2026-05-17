package campaign_test

// SIN-62983 — SecurityEngineer LOW bundle follow-up tests.
//
// LOW-1 pins the __Host- cookie prefix. The behaviour is already
// covered transitively by the happy-path test (which reads
// campaign.CookieName), but a literal-string assertion guards against
// a future refactor that quietly retitles the constant without
// rewriting the spec value.
//
// LOW-3 covers the truncateForLog mitigation: a pathological
// redirect_url emitted by the allowlist-reject warn log MUST NOT bloat
// the structured log line beyond maxLoggedURLBytes + the elision
// marker.

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/public/campaign"
)

// TestHandler_CookieNameUsesHostPrefix locks the SIN-62983 LOW-1 spec
// value. Asserts against the literal string rather than re-reading
// the constant so a future rename of the constant value (e.g. dropping
// the prefix) is caught loudly.
func TestHandler_CookieNameUsesHostPrefix(t *testing.T) {
	t.Parallel()
	if campaign.CookieName != "__Host-crm_click_id" {
		t.Fatalf("campaign.CookieName = %q, want %q (SIN-62983 LOW-1)", campaign.CookieName, "__Host-crm_click_id")
	}
	if !strings.HasPrefix(campaign.CookieName, "__Host-") {
		t.Fatalf("cookie name %q missing __Host- prefix", campaign.CookieName)
	}
}

// TestHandler_SetClickCookieEmitsHostPrefixedName proves the rename
// reaches the wire: the Set-Cookie header on the happy path carries
// the __Host- cookie name verbatim, with Path=/ and no Domain (the
// two other __Host- invariants).
func TestHandler_SetClickCookieEmitsHostPrefixedName(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h, err := campaign.New(campaign.Deps{
		Repo:         repo,
		Now:          now,
		NewClickID:   func() string { return "ck-host" },
		AllowedHosts: []string{"wa.me"},
		CookieSecure: true,
		Logger:       slog.New(slog.NewTextHandler(discardWriter{}, nil)),
	})
	if err != nil {
		t.Fatalf("campaign.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.10:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("cookies len = %d, want 1", len(cookies))
	}
	got := cookies[0]
	if got.Name != "__Host-crm_click_id" {
		t.Fatalf("cookie Name = %q, want %q", got.Name, "__Host-crm_click_id")
	}
	if got.Path != "/" {
		t.Errorf("cookie Path = %q, want / (required by __Host- prefix)", got.Path)
	}
	if got.Domain != "" {
		t.Errorf("cookie Domain = %q, want empty (required by __Host- prefix)", got.Domain)
	}
	if !got.Secure {
		t.Errorf("cookie Secure = false, want true (required by __Host- prefix under production wiring)")
	}
}

// TestHandler_TruncatesRedirectURLInWarnLog pins the SIN-62983 LOW-3
// mitigation: an out-of-allowlist redirect_url longer than
// maxLoggedURLBytes (512) MUST be elided in the structured log line.
// We probe via a JSON log handler so we can read the recorded field
// directly rather than substring-matching the rendered prefix.
func TestHandler_TruncatesRedirectURLInWarnLog(t *testing.T) {
	t.Parallel()

	// Build a redirect_url whose host is in-policy attacker.example but
	// whose path is a 1 KiB filler. We need allowlist-reject to fire
	// (so the warn log emits) — the wire below intentionally omits
	// attacker.example from AllowedHosts.
	longPath := "/" + strings.Repeat("a", 1024)
	redirectURL := "https://attacker.example.com" + longPath
	if len(redirectURL) <= 512 {
		t.Fatalf("test misconfigured: redirectURL len %d not > 512", len(redirectURL))
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "evil", redirectURL, nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }

	h, err := campaign.New(campaign.Deps{
		Repo:         repo,
		Now:          now,
		NewClickID:   func() string { return "ck-trunc" },
		AllowedHosts: []string{"wa.me"},
		CookieSecure: false,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("campaign.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/c/evil", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.11:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d (allowlist reject)", w.Result().StatusCode, http.StatusBadGateway)
	}

	// One warn line expected. Parse the JSON line.
	out := strings.TrimSpace(buf.String())
	if out == "" {
		t.Fatalf("logger captured no output; expected one warn line")
	}
	lines := strings.Split(out, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %s", len(lines), out)
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatalf("warn line not JSON: %v (raw=%q)", err, lines[0])
	}
	got, ok := rec["redirect_url"].(string)
	if !ok {
		t.Fatalf("warn line missing string redirect_url field (rec=%v)", rec)
	}
	if !strings.HasSuffix(got, "…trunc") {
		t.Fatalf("redirect_url field not truncated; got len=%d, suffix missing: %q", len(got), tail(got, 32))
	}
	// max bytes (512) + len("…trunc"). "…" is 3 bytes UTF-8.
	const elisionBytes = len("…trunc")
	wantMax := 512 + elisionBytes
	if len(got) != wantMax {
		t.Fatalf("redirect_url len = %d, want exactly %d (512 + elision)", len(got), wantMax)
	}
	if !strings.HasPrefix(got, "https://attacker.example.com/") {
		t.Fatalf("redirect_url did not keep host prefix; got %q", got[:64])
	}
}

// TestHandler_DoesNotTruncateShortRedirectURLInWarnLog proves
// truncateForLog is a pure-floor mitigation: a redirect_url under the
// cap rides through unchanged.
func TestHandler_DoesNotTruncateShortRedirectURLInWarnLog(t *testing.T) {
	t.Parallel()

	redirectURL := "https://attacker.example.com/short"
	if len(redirectURL) > 512 {
		t.Fatalf("test misconfigured: redirectURL len %d > 512", len(redirectURL))
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))

	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "short", redirectURL, nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }

	h, err := campaign.New(campaign.Deps{
		Repo:         repo,
		Now:          now,
		NewClickID:   func() string { return "ck-short" },
		AllowedHosts: []string{"wa.me"},
		CookieSecure: false,
		Logger:       logger,
	})
	if err != nil {
		t.Fatalf("campaign.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)

	req := httptest.NewRequest(http.MethodGet, "/c/short", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.12:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	out := strings.TrimSpace(buf.String())
	var rec map[string]any
	if err := json.Unmarshal([]byte(out), &rec); err != nil {
		t.Fatalf("warn line not JSON: %v (raw=%q)", err, out)
	}
	got, _ := rec["redirect_url"].(string)
	if got != redirectURL {
		t.Fatalf("redirect_url field = %q, want %q (short URLs must ride through unchanged)", got, redirectURL)
	}
	if strings.Contains(got, "…trunc") {
		t.Fatalf("short URL must not carry the trunc marker: %q", got)
	}
}

// discardWriter is a tiny io.Writer that drops everything. Inlined so
// the test file does not depend on io.Discard via an extra import in
// other helpers that already pull in io.Discard themselves.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// tail returns the last n bytes of s, or s if shorter. Used in failure
// messages so a multi-KiB blob does not blow up the test output.
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
