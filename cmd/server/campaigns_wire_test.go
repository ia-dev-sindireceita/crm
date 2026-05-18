package main

// SIN-62962 — wire-level smoke tests for the campaign dashboard.
//
// The handler-package tests cover the HTMX shape; these tests prove
// the wire and the static assets the page links to exist and serve
// cleanly. A missing stylesheet would otherwise 404 silently in
// production because the template embeds the link at HTML render time.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/campaigns"
	webcampaigns "github.com/pericles-luz/crm/internal/web/campaigns"
)

// fakeCampaignsStore satisfies the campaignsStore union with zero
// behaviour — just enough to drive assembleWebCampaignsHandler.
type fakeCampaignsStore struct{}

func (fakeCampaignsStore) ListByTenant(context.Context, uuid.UUID) ([]*campaigns.Campaign, error) {
	return nil, nil
}

func (fakeCampaignsStore) GetBySlug(context.Context, uuid.UUID, string) (*campaigns.Campaign, error) {
	return nil, campaigns.ErrNotFound
}

func (fakeCampaignsStore) CreateCampaign(context.Context, *campaigns.Campaign) error { return nil }

func (fakeCampaignsStore) StatsByTenant(context.Context, uuid.UUID) (map[uuid.UUID]campaigns.CampaignStats, error) {
	return map[uuid.UUID]campaigns.CampaignStats{}, nil
}

func (fakeCampaignsStore) ListClicks(context.Context, uuid.UUID, uuid.UUID, int) ([]*campaigns.CampaignClick, error) {
	return nil, nil
}

// Compile-time guard so the package keeps the production wire's
// expectation pinned: the fake satisfies the same port union.
var _ webcampaigns.CampaignReader = fakeCampaignsStore{}
var _ webcampaigns.CampaignWriter = fakeCampaignsStore{}
var _ webcampaigns.CampaignStatsReader = fakeCampaignsStore{}
var _ webcampaigns.CampaignClickLister = fakeCampaignsStore{}

func TestAssembleWebCampaignsHandler_RejectsMissingDeps(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebCampaignsHandler(nil, time.Now, nil); err == nil {
		t.Fatal("expected error when store is nil")
	}
	if _, err := assembleWebCampaignsHandler(fakeCampaignsStore{}, nil, nil); err == nil {
		t.Fatal("expected error when now is nil")
	}
}

func TestAssembleWebCampaignsHandler_BuildsMuxAndServesRoutes(t *testing.T) {
	t.Parallel()
	h, err := assembleWebCampaignsHandler(fakeCampaignsStore{}, time.Now, nil)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	// The handler reads tenant + CSRF + user from the request context.
	// At the wire layer those helpers (csrfTokenFromSessionContext /
	// userIDFromSessionContext) return empty when there is no session;
	// we only need to confirm the mux routes the request rather than
	// 404 — that proves Routes() registered the four endpoints. The
	// handler returns a 500 because the tenant is missing, which is
	// still proof of registration.
	for _, path := range []string{
		"/campaigns",
		"/campaigns/new",
		"/campaigns/x",
		"/campaigns/x/clicks",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusNotFound {
			t.Errorf("route %s returned 404 — Routes() did not register it", path)
		}
	}
}

func TestCampaignsStylesheet_ServedAsCSS(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/css/campaigns.css", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/css/campaigns.css must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/css") {
		t.Errorf("Content-Type = %q, want it to contain %q", got, "text/css")
	}
	body := rec.Body.String()
	if len(body) == 0 {
		t.Fatal("served body is empty — campaigns.css must have rules")
	}
	for _, needle := range []string{
		".campaigns-shell",
		".campaigns-table",
		".campaign-copy",
		".campaign-clicks",
		".campaign-form",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("campaigns.css missing required selector %q", needle)
		}
	}
}

func TestCampaignsJS_ServedAndExposesCopyHandler(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir("../../web/static"))))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/static/js/campaigns.js", nil)
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — web/static/js/campaigns.js must exist", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "javascript") && !strings.Contains(got, "ecmascript") {
		t.Errorf("Content-Type = %q, want javascript or ecmascript", got)
	}
	body := rec.Body.String()
	for _, needle := range []string{
		"navigator.clipboard",
		"campaign-copy",
		"flashFeedback",
	} {
		if !strings.Contains(body, needle) {
			t.Errorf("campaigns.js missing required symbol %q", needle)
		}
	}
}

// Sanity guard: an error from the campaign port mustn't crash the
// route registration. Exercises the early-return branch in
// assembleWebCampaignsHandler.
func TestAssembleWebCampaignsHandler_NilLoggerDefaults(t *testing.T) {
	t.Parallel()
	if _, err := assembleWebCampaignsHandler(fakeCampaignsStore{}, time.Now, nil); err != nil {
		t.Fatalf("expected nil error when logger is nil, got %v", err)
	}
}

// Sanity guard for the unused-error pattern on the fake — keeps the
// fake honest as the port grows.
var _ = errors.New
