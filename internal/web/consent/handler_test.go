package consent_test

// SIN-63191 / Fase 6 PR4 — tests for the LGPD cookie consent banner.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	domainconsent "github.com/pericles-luz/crm/internal/iam/consent"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/consent"
)

type recordingRegistry struct {
	mu        sync.Mutex
	records   []domainconsent.ConsentRecord
	returnErr error
}

func (r *recordingRegistry) Record(_ context.Context, rec domainconsent.ConsentRecord) (domainconsent.ConsentRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.returnErr != nil {
		return domainconsent.ConsentRecord{}, false, r.returnErr
	}
	r.records = append(r.records, rec)
	return rec, true, nil
}

func newHandler(t *testing.T, reg consent.Recorder) *consent.Handler {
	t.Helper()
	h, err := consent.New(consent.Deps{
		Registry: reg,
		Now:      func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return h
}

func TestBanner_NoCookie_RendersHTML(t *testing.T) {
	h := newHandler(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/consent/cookies-banner", nil)
	w := httptest.NewRecorder()
	h.Banner(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	for _, want := range []string{
		`role="dialog"`,
		"Aceitar analytics",
		"Recusar analytics",
		`hx-post="/consent/cookies"`,
		`action="/consent/cookies"`,
		consent.PolicyVersion,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q\n%s", want, body)
		}
	}
}

func TestBanner_WithCookie_ReturnsNoContent(t *testing.T) {
	h := newHandler(t, nil)
	r := httptest.NewRequest(http.MethodGet, "/consent/cookies-banner", nil)
	r.AddCookie(&http.Cookie{Name: consent.CookieName, Value: "v1.accept.1715000000"})
	w := httptest.NewRecorder()
	h.Banner(w, r)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d; want 204", w.Code)
	}
}

func TestSubmit_Accept_SetsCookieAndRecords(t *testing.T) {
	reg := &recordingRegistry{}
	h := newHandler(t, reg)
	r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=accept"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("User-Agent", "Mozilla/Test")
	tenantID := uuid.New()
	userID := uuid.New()
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: userID, TenantID: tenantID})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	cookies := w.Result().Cookies()
	var got *http.Cookie
	for _, c := range cookies {
		if c.Name == consent.CookieName {
			got = c
			break
		}
	}
	if got == nil {
		t.Fatalf("cookie not set")
	}
	if !strings.HasPrefix(got.Value, consent.PolicyVersion+".accept.") {
		t.Errorf("cookie value = %q; want prefix %s.accept.", got.Value, consent.PolicyVersion)
	}
	if got.Path != "/" {
		t.Errorf("cookie path = %q; want /", got.Path)
	}
	if got.SameSite != http.SameSiteLaxMode {
		t.Errorf("cookie SameSite = %v; want Lax", got.SameSite)
	}
	if len(reg.records) != 1 {
		t.Fatalf("registry records = %d; want 1", len(reg.records))
	}
	rec := reg.records[0]
	if rec.TenantID != tenantID {
		t.Errorf("tenant = %v; want %v", rec.TenantID, tenantID)
	}
	if rec.Subject.ID != userID.String() {
		t.Errorf("subject = %v; want %v", rec.Subject.ID, userID.String())
	}
	if rec.Purpose != domainconsent.PurposeCookiesAnalytics {
		t.Errorf("purpose = %v", rec.Purpose)
	}
	if !rec.Granted {
		t.Errorf("granted should be true on accept")
	}
	if rec.IP.String() != "10.0.0.1" {
		t.Errorf("ip = %v; want 10.0.0.1", rec.IP)
	}
	if rec.UserAgent != "Mozilla/Test" {
		t.Errorf("user-agent = %v", rec.UserAgent)
	}
}

func TestSubmit_Decline_RecordsGrantedFalse(t *testing.T) {
	reg := &recordingRegistry{}
	h := newHandler(t, reg)
	r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=decline"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tenantID := uuid.New()
	userID := uuid.New()
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: userID, TenantID: tenantID})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(reg.records) != 1 {
		t.Fatalf("records = %d", len(reg.records))
	}
	if reg.records[0].Granted {
		t.Errorf("granted should be false on decline")
	}
}

func TestSubmit_Anonymous_SetsCookieOnly(t *testing.T) {
	reg := &recordingRegistry{}
	h := newHandler(t, reg)
	r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=accept"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(reg.records) != 0 {
		t.Errorf("anonymous visitor should not record; got %d entries", len(reg.records))
	}
}

func TestSubmit_RegistryError_StillSetsCookie(t *testing.T) {
	reg := &recordingRegistry{returnErr: errors.New("db dead")}
	h := newHandler(t, reg)
	r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=accept"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tenantID := uuid.New()
	userID := uuid.New()
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: userID, TenantID: tenantID})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// Cookie still set: the registry failure is non-blocking.
	if got := w.Result().Cookies(); len(got) == 0 {
		t.Fatalf("expected cookie to be set despite registry error")
	}
}

func TestSubmit_InvalidDecision_RejectsAt400(t *testing.T) {
	h := newHandler(t, nil)
	r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=banana"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.Submit(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

func TestRoutes_Mount(t *testing.T) {
	h := newHandler(t, nil)
	mux := http.NewServeMux()
	h.Routes(mux)
	t.Run("GET banner", func(t *testing.T) {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/consent/cookies-banner", nil))
		if w.Code != http.StatusOK {
			t.Errorf("banner via mux: %d", w.Code)
		}
	})
	t.Run("POST decision", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/consent/cookies", strings.NewReader("decision=accept"))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		mux.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Errorf("submit via mux: %d", w.Code)
		}
	})
}

func TestDecision_Valid(t *testing.T) {
	cases := map[consent.Decision]bool{
		consent.DecisionAccept:  true,
		consent.DecisionDecline: true,
		consent.Decision(""):    false,
		consent.Decision("foo"): false,
	}
	for d, want := range cases {
		if got := d.Valid(); got != want {
			t.Errorf("%q.Valid() = %v; want %v", d, got, want)
		}
	}
}
