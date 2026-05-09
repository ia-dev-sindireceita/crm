package handler_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// TestLoginPost_Success_RawSetCookieHeader is the SIN-62374 / FAIL-3
// regression: assert the literal Set-Cookie header value carries every
// flag ADR 0073 §D2 requires for the tenant cookie. Cookies parsed via
// http.Response.Cookies normalise the wire format, so we read the raw
// header here to catch a future change that drops Secure or the
// __Host- prefix at the source.
func TestLoginPost_Success_RawSetCookieHeader(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	sessID := uuid.New()
	iamFake := &fakeIAM{loginSession: iam.Session{
		ID:        sessID,
		UserID:    uuid.New(),
		TenantID:  tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw")
	r := tenantedRequest(t, http.MethodPost, "/login",
		strings.NewReader(form.Encode()),
		&tenancy.Tenant{ID: tenantID},
	)
	rec := httptest.NewRecorder()

	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)

	if rec.Code != http.StatusFound {
		t.Fatalf("status=%d, want 302", rec.Code)
	}
	raw := rec.Header().Get("Set-Cookie")
	if raw == "" {
		t.Fatal("no Set-Cookie header on successful login")
	}

	// Each token is a substring on the raw header — order is not
	// strictly specified by RFC 6265 §4.1 so substring matching is the
	// stable assertion shape across stdlib versions.
	wantPrefix := sessioncookie.NameTenant + "=" + sessID.String()
	wantTokens := []string{
		wantPrefix,
		"Path=/",
		"HttpOnly",
		"Secure",
		"SameSite=Lax",
	}
	for _, tok := range wantTokens {
		if !strings.Contains(raw, tok) {
			t.Errorf("Set-Cookie missing %q\n\tgot: %s", tok, raw)
		}
	}

	// Defensive: a regression that drops the __Host- prefix would still
	// pass the substring check above if the new name happened to share
	// a suffix; pin the exact prefix.
	if !strings.HasPrefix(raw, "__Host-sess-tenant=") {
		t.Errorf("Set-Cookie does not start with __Host-sess-tenant=; got: %s", raw)
	}
}

// TestLoginPost_Success_NoEnvOverrideDropsSecure proves the Secure flag
// is hard-coded (sessioncookie.SetTenant) rather than env-toggleable: a
// LoginConfig with only IAM still writes Secure=true. This is the
// behavioural twin of removing CookieSecure from LoginConfig.
func TestLoginPost_Success_NoEnvOverrideDropsSecure(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	iamFake := &fakeIAM{loginSession: iam.Session{
		ID:        uuid.New(),
		UserID:    uuid.New(),
		TenantID:  tenantID,
		ExpiresAt: time.Now().Add(time.Hour),
	}}
	form := url.Values{}
	form.Set("email", "alice@acme.test")
	form.Set("password", "pw")
	r := tenantedRequest(t, http.MethodPost, "/login",
		strings.NewReader(form.Encode()),
		&tenancy.Tenant{ID: tenantID},
	)
	rec := httptest.NewRecorder()

	handler.LoginPost(handler.LoginConfig{IAM: iamFake})(rec, r)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if !c.Secure {
		t.Fatal("cookie Secure must be true regardless of caller config")
	}
	if c.Name != sessioncookie.NameTenant {
		t.Fatalf("cookie name=%q, want %q", c.Name, sessioncookie.NameTenant)
	}
}
