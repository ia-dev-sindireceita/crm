package handler_test

// SIN-63774 — NewHelloTenant renders a navigable index of the Fase 3–6
// surfaces on /hello-tenant so a freshly-logged-in operator can reach
// every mounted area without typing a URL. The tests below pin the
// rendering contract on the three flag mixes that matter:
//
//   - all flags true  → every surface is an <a href="/path">Label</a>,
//   - all flags false → every surface is an aria-disabled <span> with
//                       the "(indisponível neste ambiente)" hint,
//   - mixed           → enabled surfaces render as links, disabled ones
//                       render as spans; the ordering is stable.
//
// Backward-compat for the zero-deps HelloTenant shim is covered by the
// existing TestHelloTenant_RendersTenantName in handlers_test.go (which
// still calls handler.HelloTenant directly).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// helloIndexRequest builds the authenticated GET /hello-tenant request
// used by every test in this file: a tenant in context, a session in
// context, and the standard "acme" tenant fixture so the body's tenant
// name assertion stays consistent with handlers_test.go.
func helloIndexRequest(t *testing.T) *http.Request {
	t.Helper()
	tenantID := uuid.New()
	userID := uuid.New()
	tenant := &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm.local"}
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, tenant)
	return r.WithContext(middleware.WithSession(r.Context(), iam.Session{
		ID: uuid.New(), UserID: userID, TenantID: tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
	}))
}

// allEnabledSurfaces is the seven (label, path) pairs that the post-
// login index MUST render in this exact order — SIN-63774 §Approach 4.
// Changing the slice ordering here intentionally breaks the test so a
// future refactor cannot silently reshuffle the operator's nav.
var allEnabledSurfaces = []struct{ label, path string }{
	{"Funil de conversas", "/funnel"},
	{"Regras do funil", "/funnel/rules"},
	{"Catálogo de produtos", "/catalog"},
	{"Campanhas", "/campaigns"},
	{"Privacidade e DPA", "/settings/privacy"},
	{"Política de IA", "/settings/ai-policy"},
	{"Banner de consentimento", "/consent/cookies-banner"},
}

func TestNewHelloTenant_AllFlagsTrue_RendersLinks(t *testing.T) {
	t.Parallel()
	deps := handler.HelloTenantDeps{
		FunnelEnabled:      true,
		FunnelRulesEnabled: true,
		CatalogEnabled:     true,
		CampaignsEnabled:   true,
		PrivacyEnabled:     true,
		AIPolicyEnabled:    true,
		ConsentEnabled:     true,
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, helloIndexRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="surfaces-nav"`) {
		t.Fatalf("body missing surfaces-nav: %q", body)
	}
	for _, s := range allEnabledSurfaces {
		want := `<a href="` + s.path + `">` + s.label + `</a>`
		if !strings.Contains(body, want) {
			t.Fatalf("body missing live link %q\nbody=%q", want, body)
		}
	}
	if strings.Contains(body, `aria-disabled="true"`) {
		t.Fatalf("body unexpectedly rendered an aria-disabled span when every flag is true: %q", body)
	}
}

func TestNewHelloTenant_AllFlagsFalse_RendersDisabledSpans(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, helloIndexRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="surfaces-nav"`) {
		t.Fatalf("body missing surfaces-nav: %q", body)
	}
	for _, s := range allEnabledSurfaces {
		want := `<span aria-disabled="true">` + s.label + ` (indisponível neste ambiente)</span>`
		if !strings.Contains(body, want) {
			t.Fatalf("body missing disabled span %q\nbody=%q", want, body)
		}
		// A disabled surface MUST NOT also be rendered as a clickable
		// anchor — that would defeat the "(indisponível neste ambiente)"
		// hint by leaving a dead link next to it.
		dead := `<a href="` + s.path + `">`
		if strings.Contains(body, dead) {
			t.Fatalf("body rendered dead link %q for disabled surface %q\nbody=%q", dead, s.label, body)
		}
	}
}

func TestNewHelloTenant_MixedFlags_RespectsPerSurfaceFlag(t *testing.T) {
	t.Parallel()
	deps := handler.HelloTenantDeps{
		FunnelEnabled:  true,
		CatalogEnabled: true,
		PrivacyEnabled: true,
		// FunnelRulesEnabled, CampaignsEnabled, AIPolicyEnabled,
		// ConsentEnabled deliberately left zero/false.
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, helloIndexRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	enabled := []struct{ label, path string }{
		{"Funil de conversas", "/funnel"},
		{"Catálogo de produtos", "/catalog"},
		{"Privacidade e DPA", "/settings/privacy"},
	}
	for _, s := range enabled {
		want := `<a href="` + s.path + `">` + s.label + `</a>`
		if !strings.Contains(body, want) {
			t.Fatalf("body missing enabled link %q\nbody=%q", want, body)
		}
	}

	disabled := []struct{ label, path string }{
		{"Regras do funil", "/funnel/rules"},
		{"Campanhas", "/campaigns"},
		{"Política de IA", "/settings/ai-policy"},
		{"Banner de consentimento", "/consent/cookies-banner"},
	}
	for _, s := range disabled {
		want := `<span aria-disabled="true">` + s.label + ` (indisponível neste ambiente)</span>`
		if !strings.Contains(body, want) {
			t.Fatalf("body missing disabled span %q\nbody=%q", want, body)
		}
		dead := `<a href="` + s.path + `">`
		if strings.Contains(body, dead) {
			t.Fatalf("body rendered dead link %q for disabled surface %q\nbody=%q", dead, s.label, body)
		}
	}
}

// TestNewHelloTenant_PreservesSurfaceOrder pins the documented ordering
// (SIN-63774 §Approach 4) by reading the rendered body once and walking
// label indices. The list MUST appear in the canonical order regardless
// of which subset of flags is enabled.
func TestNewHelloTenant_PreservesSurfaceOrder(t *testing.T) {
	t.Parallel()
	deps := handler.HelloTenantDeps{
		FunnelEnabled:      true,
		FunnelRulesEnabled: false,
		CatalogEnabled:     true,
		CampaignsEnabled:   false,
		PrivacyEnabled:     true,
		AIPolicyEnabled:    false,
		ConsentEnabled:     true,
	}
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(deps)(rec, helloIndexRequest(t))

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 (body=%q)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	prev := -1
	for _, s := range allEnabledSurfaces {
		idx := strings.Index(body, s.label)
		if idx < 0 {
			t.Fatalf("label %q not found in body", s.label)
		}
		if idx <= prev {
			t.Fatalf("surface %q rendered out of order: idx=%d, prev=%d", s.label, idx, prev)
		}
		prev = idx
	}
}

// TestNewHelloTenant_MissingTenantContext_500 mirrors the legacy
// HelloTenant guarantee on the constructor wrapper: a request with no
// tenant in context is a programmer error and must fail fast with 500.
func TestNewHelloTenant_MissingTenantContext_500(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/hello-tenant", nil)
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// TestNewHelloTenant_MissingSessionContext_500 mirrors the legacy
// HelloTenant guarantee: tenant-in-context but no session is also a
// programmer error and must fail fast with 500.
func TestNewHelloTenant_MissingSessionContext_500(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	r := tenantedRequest(t, http.MethodGet, "/hello-tenant", nil, &tenancy.Tenant{ID: tenantID, Name: "acme"})
	rec := httptest.NewRecorder()
	handler.NewHelloTenant(handler.HelloTenantDeps{})(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}
