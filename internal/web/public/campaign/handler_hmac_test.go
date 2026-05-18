package campaign_test

// SIN-62982 — handler-level HMAC marker tests. The marker primitive is
// covered exhaustively in internal/campaigns/marker_test.go; this file
// pins the redirect-handler side: that the handler substitutes the
// signed token into the {click_id} placeholder when a marker key is
// configured and falls back to the legacy unsigned form otherwise.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/public/campaign"
)

func newSignedTestHandler(t *testing.T, repo campaigns.Repository, now func() time.Time, allowed []string, key campaigns.MarkerKey) http.Handler {
	t.Helper()
	h, err := campaign.New(campaign.Deps{
		Repo:         repo,
		Now:          now,
		NewClickID:   func() string { return "ck-test-token" },
		AllowedHosts: allowed,
		CookieSecure: false,
		MarkerKey:    key,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("campaign.New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func TestHandler_SignsClickMarkerWhenKeySet(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999?text=hi%20%5Bcrm%3A{click_id}%5D", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	h := newSignedTestHandler(t, repo, now, []string{"wa.me"}, key)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.7:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	req.AddCookie(&http.Cookie{Name: campaign.CookieName, Value: "ck-pin"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Result().StatusCode != http.StatusFound {
		t.Fatalf("status = %d, want %d", w.Result().StatusCode, http.StatusFound)
	}
	loc := w.Result().Header.Get("Location")

	// The placeholder substitution is url.QueryEscape'd, so the dot
	// separator survives but the brackets in the template are
	// percent-encoded. Walk both encodings.
	decoded, err := url.QueryUnescape(loc)
	if err != nil {
		t.Fatalf("location not url-unescapable: %v (loc=%q)", err, loc)
	}
	wantToken := campaigns.BuildClickToken(key, tenant.ID, "ck-pin")
	if !strings.Contains(decoded, "[crm:"+wantToken+"]") {
		t.Fatalf("decoded location = %q, want it to contain marker %q", decoded, "[crm:"+wantToken+"]")
	}
	if strings.Contains(decoded, "[crm:ck-pin]") {
		t.Fatalf("decoded location = %q still contains legacy unsigned marker", decoded)
	}
}

func TestHandler_EmitsLegacyMarkerWhenKeyUnset(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/5511999999999?text=%5Bcrm%3A{click_id}%5D", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	h := newSignedTestHandler(t, repo, now, []string{"wa.me"}, nil)

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.7:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	req.AddCookie(&http.Cookie{Name: campaign.CookieName, Value: "ck-pin"})

	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	loc := w.Result().Header.Get("Location")
	decoded, err := url.QueryUnescape(loc)
	if err != nil {
		t.Fatalf("location not url-unescapable: %v (loc=%q)", err, loc)
	}
	if !strings.Contains(decoded, "[crm:ck-pin]") {
		t.Fatalf("decoded location = %q, want legacy unsigned [crm:ck-pin]", decoded)
	}
	if strings.Contains(decoded, ".") && strings.Contains(decoded, "[crm:ck-pin.") {
		t.Fatalf("decoded location = %q unexpectedly carries an hmac suffix without a configured key", decoded)
	}
}

func TestHandler_SignedMarkerVerifiesAgainstSameKey(t *testing.T) {
	t.Parallel()
	repo := campaigns.NewInMemoryRepository()
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}
	c := mustCampaign(t, tenant.ID, "promo", "https://wa.me/x?t=%5Bcrm%3A{click_id}%5D", nil)
	_ = repo.CreateCampaign(context.Background(), c)
	now := func() time.Time { return time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC) }
	key := campaigns.MarkerKey("0123456789abcdef0123456789abcdef")
	h := newSignedTestHandler(t, repo, now, []string{"wa.me"}, key)

	// Cookie value matches the production click_id alphabet+length
	// (uuid-v4) so the inbox-side regex finds it once the redirect
	// re-decodes back from the percent-encoded redirect_url.
	clickID := "c0a5e0b7-3d3e-4f4a-9aa8-44e2f8b9b1f3"

	req := httptest.NewRequest(http.MethodGet, "/c/promo", nil)
	req.Host = "acme.crm.local"
	req.RemoteAddr = "203.0.113.8:80"
	req = req.WithContext(tenancy.WithContext(req.Context(), tenant))
	req.AddCookie(&http.Cookie{Name: campaign.CookieName, Value: clickID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	loc, _ := url.QueryUnescape(w.Result().Header.Get("Location"))
	parsed := campaigns.ExtractClickMarker(loc)
	if !parsed.Found {
		t.Fatalf("could not find marker in redirect (loc=%q)", loc)
	}
	if parsed.ClickID != clickID {
		t.Fatalf("parsed click_id = %q, want %q", parsed.ClickID, clickID)
	}
	if parsed.HMACHex == "" {
		t.Fatalf("parsed marker carried no hmac suffix — handler did not sign (loc=%q)", loc)
	}
	if !campaigns.VerifyClickToken(key, false, tenant.ID, parsed.ClickID, parsed.HMACHex) {
		t.Fatalf("redirect marker did not verify against the same key (parsed=%+v)", parsed)
	}
}
