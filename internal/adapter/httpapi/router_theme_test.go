package httpapi_test

// SIN-63101 — router-level coverage for the per-tenant theme middleware
// mount. Two scenarios pin the contract:
//
//   - Nil Deps.Theme leaves the chain unchanged (every request sees
//     branding.DefaultThemeStyle via ThemeStyleFromContext, matching
//     the pre-SIN-63101 behaviour).
//   - Non-nil Deps.Theme runs AFTER TenantScope and propagates the
//     resolved palette into the request context so downstream renderers
//     see the per-tenant style on every authed page.

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// themeStore is a minimal branding.PaletteStore for the router test. It
// pins a single per-tenant palette so the test asserts on the exact
// style string the middleware attaches downstream.
type themeStore struct {
	mu       sync.Mutex
	palettes map[uuid.UUID]branding.Palette
}

func (s *themeStore) GetByTenantID(_ context.Context, id uuid.UUID) (branding.Palette, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.palettes[id]
	if !ok {
		return branding.Palette{}, branding.ErrPaletteNotFound
	}
	return p, nil
}

// captureHandler is a test http.Handler that records the
// ThemeStyleFromContext value the router resolved before dispatching
// the request. It always returns 200 so chi's status path is exercised
// uniformly.
type captureHandler struct {
	mu    sync.Mutex
	style template.CSS
}

func (h *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.style = branding.ThemeStyleFromContext(r.Context())
	h.mu.Unlock()
	w.WriteHeader(http.StatusOK)
}

func (h *captureHandler) snapshot() template.CSS {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.style
}

// themeRouter wires a single capture endpoint inside the tenanted authed
// group via the WebPrivacy slot — the GET-only privacy routes mount
// without an Authorizer requirement, so the test only needs to drive
// login + cookie replay just like the existing SIN-62217 fixtures.
func themeRouter(t *testing.T, theme *middleware.ThemeMiddleware, capture *captureHandler) (http.Handler, *inmemIAM, uuid.UUID) {
	t.Helper()
	acmeID := uuid.New()
	tenants := map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: acmeID, Name: "acme", Host: "acme.crm.local"},
	}
	tenantIDs := map[string]uuid.UUID{"acme.crm.local": acmeID}
	store := newInmemIAM(tenantIDs)
	store.addUser("acme.crm.local", "alice@acme.test", "pw-alice", uuid.New())
	r := httpapi.NewRouter(httpapi.Deps{
		IAM:            store,
		TenantResolver: &fakeResolver{byHost: tenants},
		Theme:          theme,
		WebPrivacy:     capture,
	})
	return r, store, acmeID
}

func loginAcme(t *testing.T, h http.Handler) *http.Cookie {
	t.Helper()
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw-alice")
	rec := do(t, h, http.MethodPost, "acme.crm.local", "/login", strings.NewReader(form.Encode()))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == middleware.SessionCookieName {
			return c
		}
	}
	t.Fatalf("login did not set %s cookie", middleware.SessionCookieName)
	return nil
}

func TestRouter_Theme_NilMiddleware_FallsBackToDefaultStyle(t *testing.T) {
	t.Parallel()
	capture := &captureHandler{}
	r, _, _ := themeRouter(t, nil, capture)
	cookie := loginAcme(t, r)
	rec := do(t, r, http.MethodGet, "acme.crm.local", "/settings/privacy", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := capture.snapshot(); got != branding.DefaultThemeStyle {
		t.Fatalf("style = %q, want DefaultThemeStyle (nil middleware must be pass-through)", got)
	}
}

func TestRouter_Theme_NonNil_AttachesPerTenantPaletteStyle(t *testing.T) {
	t.Parallel()
	store := &themeStore{palettes: map[uuid.UUID]branding.Palette{}}
	mw := middleware.NewTheme(middleware.ThemeConfig{Store: store})
	capture := &captureHandler{}
	r, _, tenantID := themeRouter(t, mw, capture)

	// Persist a palette for the tenant the router resolves so the
	// middleware's first lookup returns a non-default style.
	store.palettes[tenantID] = branding.Palette{
		Primary:       branding.RGB{R: 0x11, G: 0x22, B: 0x33},
		Secondary:     branding.RGB{R: 0x44, G: 0x55, B: 0x66},
		Accent:        branding.RGB{R: 0x77, G: 0x88, B: 0x99},
		Foreground:    branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Background:    branding.RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		TextOnPrimary: branding.RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		Source:        branding.PaletteSourceManual,
	}

	cookie := loginAcme(t, r)
	rec := do(t, r, http.MethodGet, "acme.crm.local", "/settings/privacy", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	style := strings.ToLower(string(capture.snapshot()))
	if style == strings.ToLower(string(branding.DefaultThemeStyle)) {
		t.Fatalf("style = DefaultThemeStyle, want per-tenant palette style")
	}
	if !strings.Contains(style, "#112233") {
		t.Fatalf("style = %q, want primary #112233 from persisted palette", style)
	}

	// AC #4 — Invalidate evicts the cached entry so the next lookup
	// re-reads the store. Update the store and assert the new value
	// is observed after Invalidate (without TTL wait).
	store.mu.Lock()
	store.palettes[tenantID] = branding.Palette{
		Primary:       branding.RGB{R: 0xAA, G: 0xBB, B: 0xCC},
		Secondary:     branding.RGB{R: 0x44, G: 0x55, B: 0x66},
		Accent:        branding.RGB{R: 0x77, G: 0x88, B: 0x99},
		Foreground:    branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Background:    branding.RGB{R: 0xFF, G: 0xFF, B: 0xFF},
		TextOnPrimary: branding.RGB{R: 0x00, G: 0x00, B: 0x00},
		Source:        branding.PaletteSourceManual,
	}
	store.mu.Unlock()
	if !mw.Invalidate(tenantID) {
		t.Fatalf("Invalidate returned false, want true (entry was cached on prior request)")
	}
	rec = do(t, r, http.MethodGet, "acme.crm.local", "/settings/privacy", nil, cookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after invalidate = %d, want 200", rec.Code)
	}
	if got := strings.ToLower(string(capture.snapshot())); !strings.Contains(got, "#aabbcc") {
		t.Fatalf("style after invalidate = %q, want updated #aabbcc", got)
	}
}
