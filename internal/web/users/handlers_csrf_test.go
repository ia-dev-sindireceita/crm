package users_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	webusers "github.com/pericles-luz/crm/internal/web/users"
)

// newHandlerWithCSRF builds the surface with a real CSRF token resolver so
// the contract test below can assert the token is transported to HTMX.
func newHandlerWithCSRF(t *testing.T, svc webusers.Service, token string) http.Handler {
	t.Helper()
	h, err := webusers.New(webusers.Deps{
		Service:   svc,
		CSRFToken: func(*http.Request) string { return token },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// TestList_EmitsCSRFMetaAndHXHeaders is the contract test for the CSRF
// transport the authed group's middleware (double-submit, ADR 0073)
// requires. Without <meta name="csrf-token"> in <head> and hx-headers on
// <body>, every HTMX mutation on /settings/users (create, role change,
// deactivate, reactivate) 403s in production even though these unit tests
// bypass the router CSRF check (SIN-67232). Mirrors catalog's
// TestListAndDetail_EmitCSRFMetaAndHXHeaders.
func TestList_EmitsCSRFMetaAndHXHeaders(t *testing.T) {
	t.Parallel()
	const token = "csrf-tok-users-123"
	tenantID, userID := uuid.New(), uuid.New()
	h := newHandlerWithCSRF(t, &fakeService{}, token)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), tenantID, userID)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="csrf-token" content="` + token + `">`,
		`hx-headers='{"X-CSRF-Token": "` + token + `"}'`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing CSRF transport %q", want)
		}
	}
	// The hx-headers attribute must live on <body> so the create form and the
	// row action buttons (all HTMX requests nested inside <body>) inherit the
	// token. Assert the attribute is on the opening body tag.
	if !strings.Contains(body, `<body hx-headers=`) {
		t.Error("hx-headers must be on the <body> tag so mutations inherit the token")
	}
}

// TestList_DefaultCSRFResolver_StillEmitsHXHeaders proves the attribute is
// always rendered even when no resolver is wired (New defaults it), so the
// template never silently drops the transport. The token value is empty but
// the attribute — the thing HTMX keys on — is present.
func TestList_DefaultCSRFResolver_StillEmitsHXHeaders(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{}) // no CSRFToken wired -> default resolver
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `<body hx-headers='{"X-CSRF-Token": "`) {
		t.Error("default resolver must still render the hx-headers attribute on <body>")
	}
	if !strings.Contains(body, `<meta name="csrf-token"`) {
		t.Error("default resolver must still render the csrf-token meta tag")
	}
}
