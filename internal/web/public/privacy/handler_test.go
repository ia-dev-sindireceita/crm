package privacy_test

// SIN-63191 / Fase 6 PR4 — tests for the public LGPD-disclosure page.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/public/privacy"
)

type stubReader struct {
	settings tenancy.PrivacySettings
	err      error
}

func (s stubReader) LoadPrivacySettings(_ context.Context, _ uuid.UUID) (tenancy.PrivacySettings, error) {
	return s.settings, s.err
}

func newReq(t *testing.T, tenant *tenancy.Tenant) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/privacy", nil)
	if tenant != nil {
		r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
	}
	return r
}

func TestNew_Validates(t *testing.T) {
	if _, err := privacy.New(privacy.Deps{}); err == nil {
		t.Fatalf("expected error on missing Settings")
	}
	if _, err := privacy.New(privacy.Deps{Settings: stubReader{}}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestView_PublishedPolicy_RendersMarkdownAndDPO(t *testing.T) {
	updated := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	r := stubReader{settings: tenancy.PrivacySettings{
		DPOName:               "Marina Soares",
		DPOEmail:              "dpo@acme.example",
		PrivacyPolicyVersion:  "2026.05",
		PrivacyPolicyURL:      "https://acme.example/privacy.pdf",
		PrivacyPolicyMarkdown: "# Política\n\nConteúdo **sensível** *agora* da política.\n\n- item 1\n- item 2",
		PrivacyPolicyUpdated:  &updated,
	}}
	h, err := privacy.New(privacy.Deps{Settings: r, Now: func() time.Time { return updated }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "Acme", Host: "acme.crm"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		"<h1", // headings are rendered (legal docs need them)
		"<strong", "<em",
		"Conteúdo", "item 1",
		"Marina Soares",
		"mailto:dpo@acme.example",
		"2026.05",
		"https://acme.example/privacy.pdf",
		"2026-05-01",
		"acme",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
	// Raw HTML in the source MUST be escaped (no <script> on the page).
	if strings.Contains(body, "<script>") {
		t.Errorf("unsafe HTML leaked into output")
	}
	if got := w.Header().Get("Cache-Control"); !strings.Contains(got, "public") {
		t.Errorf("cache-control = %q; want public", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("nosniff missing: %q", got)
	}
}

func TestView_NoSettings_UsesFallbackText(t *testing.T) {
	h, err := privacy.New(privacy.Deps{Settings: stubReader{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "Default", Host: "default.crm"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, privacy.FallbackVersion) {
		t.Errorf("fallback version not rendered")
	}
	if !strings.Contains(body, "modelo padrão") {
		t.Errorf("fallback policy body not rendered")
	}
	if !strings.Contains(body, "ainda não foi publicado") {
		t.Errorf("empty DPO message not rendered")
	}
}

func TestView_RawHTMLEscaped(t *testing.T) {
	r := stubReader{settings: tenancy.PrivacySettings{
		PrivacyPolicyMarkdown: "<script>alert(1)</script>\n\nbody",
	}}
	h, err := privacy.New(privacy.Deps{Settings: r})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "x", Host: "x"}))
	body := w.Body.String()
	if strings.Contains(body, "<script>alert(1)") {
		t.Fatalf("raw script leaked into output: %s", body)
	}
	if !strings.Contains(body, "<p>body</p>") {
		t.Errorf("expected body paragraph, got: %s", body)
	}
}

func TestView_UnsafePolicyURL_Dropped(t *testing.T) {
	r := stubReader{settings: tenancy.PrivacySettings{
		PrivacyPolicyURL: "javascript:alert(1)",
		DPOEmail:         "not an email",
	}}
	h, err := privacy.New(privacy.Deps{Settings: r})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "x", Host: "x"}))
	body := w.Body.String()
	if strings.Contains(body, "javascript:") {
		t.Errorf("javascript: url leaked: %s", body)
	}
	if strings.Contains(body, "mailto:not an email") {
		t.Errorf("invalid email leaked into mailto")
	}
}

func TestView_NoTenant_FailsClosed(t *testing.T) {
	h, err := privacy.New(privacy.Deps{Settings: stubReader{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, nil))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", w.Code)
	}
}

func TestView_ReaderError_RendersFallback(t *testing.T) {
	r := stubReader{err: tenancy.ErrTenantNotFound}
	h, err := privacy.New(privacy.Deps{Settings: r})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "x", Host: "x"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200 (fallback)", w.Code)
	}
}

func TestRoutes_Mount(t *testing.T) {
	h, err := privacy.New(privacy.Deps{Settings: stubReader{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	w := httptest.NewRecorder()
	r := newReq(t, &tenancy.Tenant{ID: uuid.New(), Name: "x", Host: "x"})
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("routed status = %d", w.Code)
	}
}
