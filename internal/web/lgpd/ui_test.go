package lgpd_test

// SIN-63191 / Fase 6 PR4 — table-driven tests for the HTMX admin
// pages on top of the SIN-63186 JSON/ZIP handlers. New file, no
// existing test is mutated.

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
	domain "github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/lgpd"
)

// fakeLister implements domain.DeletionLister with an in-memory slice.
type fakeLister struct {
	mu   sync.Mutex
	rows []domain.DeletionRequest
	err  error
}

func (f *fakeLister) ListByTenant(_ context.Context, tenant uuid.UUID, status domain.DeletionStatus, _ int) ([]domain.DeletionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	out := make([]domain.DeletionRequest, 0, len(f.rows))
	for _, r := range f.rows {
		if r.TenantID != tenant {
			continue
		}
		if status != "" && r.Status != status {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func newUIHandler(t *testing.T, lister domain.DeletionLister, del *fakeDeletions, aud *fakeAudit) *lgpd.UIHandler {
	t.Helper()
	parent, err := lgpd.New(lgpd.Deps{
		Export:    &fakeExport{},
		Deletions: del,
		Audit:     aud,
		Now:       func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("parent New: %v", err)
	}
	ui, err := lgpd.NewUI(parent, lgpd.UIDeps{
		Deletions: del,
		Lister:    lister,
		Audit:     aud,
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
		Now:       func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("NewUI: %v", err)
	}
	return ui
}

func TestNewUI_RequiresDeps(t *testing.T) {
	parent, err := lgpd.New(lgpd.Deps{
		Export:    &fakeExport{},
		Deletions: newFakeDeletions(),
		Audit:     &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("parent New: %v", err)
	}
	csrf := func(*http.Request) string { return "tok" }
	cases := map[string]lgpd.UIDeps{
		"missing deletions": {Lister: &fakeLister{}, CSRFToken: csrf},
		"missing lister":    {Deletions: newFakeDeletions(), CSRFToken: csrf},
		"missing csrf":      {Deletions: newFakeDeletions(), Lister: &fakeLister{}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := lgpd.NewUI(parent, d); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
	if _, err := lgpd.NewUI(nil, lgpd.UIDeps{}); err == nil {
		t.Fatalf("expected error with nil parent")
	}
}

func contactReq(t *testing.T, contactID, tenantID, userID uuid.UUID) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/admin/contacts/"+contactID.String()+"/lgpd", nil)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "acme", Host: "acme.crm"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: userID, TenantID: tenantID})
	r = r.WithContext(ctx)
	r.SetPathValue("contactID", contactID.String())
	return r
}

func TestUI_ContactPage_Renders(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	w := httptest.NewRecorder()
	ui.ContactPage(w, contactReq(t, contactID, tenantID, userID))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; want 200", w.Code)
	}
	body := w.Body.String()
	// AC #1: hx-confirm on the destructive form + visible labels.
	if !strings.Contains(body, "hx-confirm=") {
		t.Errorf("body missing hx-confirm:\n%s", body)
	}
	if !strings.Contains(body, "Exportar dados") {
		t.Errorf("body missing export button label")
	}
	if !strings.Contains(body, "Solicitar deleção") {
		t.Errorf("body missing delete button label")
	}
	if !strings.Contains(body, contactID.String()) {
		t.Errorf("body missing contact id")
	}
	// AC #6: form posts to /admin/lgpd/delete-form (progressive).
	if !strings.Contains(body, `action="/admin/lgpd/delete-form"`) {
		t.Errorf("body missing form action")
	}
	if !strings.Contains(body, `csrf-test-token`) {
		t.Errorf("body missing csrf token")
	}
	// AC #5: explicit aria-label on every button.
	if !strings.Contains(body, `aria-label="Exportar dados pessoais do contato como ZIP"`) {
		t.Errorf("export button missing aria-label")
	}
}

func TestUI_ContactPage_BadContactID(t *testing.T) {
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	r := httptest.NewRequest(http.MethodGet, "/admin/contacts/not-a-uuid/lgpd", nil)
	r.SetPathValue("contactID", "not-a-uuid")
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New(), Name: "x", Host: "x"})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	ui.ContactPage(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d; want 400", w.Code)
	}
}

func TestUI_ContactPage_NoTenant(t *testing.T) {
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	r := httptest.NewRequest(http.MethodGet, "/admin/contacts/"+uuid.New().String()+"/lgpd", nil)
	r.SetPathValue("contactID", uuid.New().String())
	w := httptest.NewRecorder()
	ui.ContactPage(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", w.Code)
	}
}

func TestUI_ContactPage_NoCSRF(t *testing.T) {
	parent, err := lgpd.New(lgpd.Deps{
		Export:    &fakeExport{},
		Deletions: newFakeDeletions(),
		Audit:     &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("parent: %v", err)
	}
	ui, err := lgpd.NewUI(parent, lgpd.UIDeps{
		Deletions: newFakeDeletions(),
		Lister:    &fakeLister{},
		CSRFToken: func(*http.Request) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewUI: %v", err)
	}
	contactID := uuid.New()
	w := httptest.NewRecorder()
	ui.ContactPage(w, contactReq(t, contactID, uuid.New(), uuid.New()))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500", w.Code)
	}
}

// TestUI_ContactPage_ContactNotFound_404 — SIN-63590. The RLS-bound
// ExportRepository.GetContact reports ErrDeletionRequestNotFound for
// both cross-tenant and nonexistent contact ids; the form-render route
// must surface that as 404 instead of 200 (ADR-letter conformance with
// the POST action endpoint).
func TestUI_ContactPage_ContactNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	parent, err := lgpd.New(lgpd.Deps{
		Export:    &fakeExport{getErr: domain.ErrDeletionRequestNotFound},
		Deletions: newFakeDeletions(),
		Audit:     &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("parent New: %v", err)
	}
	ui, err := lgpd.NewUI(parent, lgpd.UIDeps{
		Deletions: newFakeDeletions(),
		Lister:    &fakeLister{},
		Audit:     &fakeAudit{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
	})
	if err != nil {
		t.Fatalf("NewUI: %v", err)
	}
	w := httptest.NewRecorder()
	ui.ContactPage(w, contactReq(t, contactID, tenantID, userID))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d; want 404 (body=%s)", w.Code, w.Body.String())
	}
	// AC: no form / CSRF leaks into the 404 body — only the standard
	// http.Error text.
	if strings.Contains(w.Body.String(), "csrf-test-token") {
		t.Errorf("404 body leaked csrf token: %s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), contactID.String()) {
		t.Errorf("404 body leaked the requested contact id: %s", w.Body.String())
	}
}

// TestUI_ContactPage_ContactLookupError_500 — generic adapter errors
// (DB down, RLS misconfig) MUST surface as 500, not 404, so the 404
// stays a precise existence signal and the operator gets an alert.
func TestUI_ContactPage_ContactLookupError_500(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	parent, err := lgpd.New(lgpd.Deps{
		Export:    &fakeExport{getErr: errors.New("db down")},
		Deletions: newFakeDeletions(),
		Audit:     &fakeAudit{},
	})
	if err != nil {
		t.Fatalf("parent New: %v", err)
	}
	ui, err := lgpd.NewUI(parent, lgpd.UIDeps{
		Deletions: newFakeDeletions(),
		Lister:    &fakeLister{},
		Audit:     &fakeAudit{},
		CSRFToken: func(*http.Request) string { return "csrf-test-token" },
	})
	if err != nil {
		t.Fatalf("NewUI: %v", err)
	}
	w := httptest.NewRecorder()
	ui.ContactPage(w, contactReq(t, contactID, tenantID, userID))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; want 500 (body=%s)", w.Code, w.Body.String())
	}
}

func TestUI_RequestsPage_FilterAndRender(t *testing.T) {
	tenantID := uuid.New()
	now := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	completedAt := now.Add(-24 * time.Hour)
	lister := &fakeLister{rows: []domain.DeletionRequest{
		{
			ID: uuid.New(), TenantID: tenantID, ContactID: uuid.New(),
			Justification: "perform-erasure", Status: domain.DeletionStatusPending,
			RetentionUntil: now.Add(365 * 24 * time.Hour),
			CreatedAt:      now.Add(-time.Hour),
		},
		{
			ID: uuid.New(), TenantID: tenantID, ContactID: uuid.New(),
			Justification: "expired", Status: domain.DeletionStatusCompleted,
			RetentionUntil: now.Add(-24 * time.Hour),
			CompletedAt:    &completedAt,
			CreatedAt:      now.Add(-3 * 24 * time.Hour),
		},
	}}
	ui := newUIHandler(t, lister, newFakeDeletions(), &fakeAudit{})

	t.Run("all", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests", nil)
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		ui.RequestsPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "lgpd-badge--in_retention") {
			t.Errorf("missing in_retention badge:\n%s", body)
		}
		if !strings.Contains(body, "lgpd-badge--completed") {
			t.Errorf("missing completed badge")
		}
	})
	t.Run("only in_retention", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests?status=in_retention", nil)
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		ui.RequestsPage(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d", w.Code)
		}
		body := w.Body.String()
		if !strings.Contains(body, "lgpd-badge--in_retention") {
			t.Errorf("missing in_retention badge")
		}
		if strings.Contains(body, "lgpd-badge--completed") {
			t.Errorf("completed badge leaked into in_retention filter")
		}
	})
	t.Run("completed only", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests?status=completed", nil)
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		ui.RequestsPage(w, r)
		body := w.Body.String()
		if strings.Contains(body, "lgpd-badge--in_retention") {
			t.Errorf("in_retention badge leaked into completed filter")
		}
		if !strings.Contains(body, "lgpd-badge--completed") {
			t.Errorf("missing completed badge in completed filter")
		}
	})
	t.Run("invalid status", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests?status=banana", nil)
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
		r = r.WithContext(ctx)
		w := httptest.NewRecorder()
		ui.RequestsPage(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d; want 400", w.Code)
		}
	})
	t.Run("missing tenant", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests", nil)
		w := httptest.NewRecorder()
		ui.RequestsPage(w, r)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d; want 500", w.Code)
		}
	})
}

func TestUI_RequestsPage_Empty(t *testing.T) {
	tenantID := uuid.New()
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/requests", nil)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	ui.RequestsPage(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "lgpd-empty") {
		t.Errorf("missing empty-state marker")
	}
}

func TestUI_DeleteForm_AcceptsForm(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	del := newFakeDeletions()
	aud := &fakeAudit{}
	ui := newUIHandler(t, &fakeLister{}, del, aud)
	form := strings.NewReader("contact_id=" + contactID.String() + "&justification=" + "operator+requested+erasure")
	r := httptest.NewRequest(http.MethodPost, "/admin/lgpd/delete-form", form)
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: userID, TenantID: tenantID})
	r = r.WithContext(ctx)
	w := httptest.NewRecorder()
	ui.DeleteForm(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Solicitação registrada") {
		t.Errorf("ack template not rendered: %s", w.Body.String())
	}
	if got := len(del.upserts); got != 1 {
		t.Fatalf("upserts = %d; want 1", got)
	}
	if del.upserts[0].ContactID != contactID {
		t.Errorf("upsert contact = %v; want %v", del.upserts[0].ContactID, contactID)
	}
	if got := len(aud.events); got != 1 {
		t.Errorf("audit events = %d; want 1", got)
	}
}

func TestUI_DeleteForm_Validation(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	del := newFakeDeletions()
	ui := newUIHandler(t, &fakeLister{}, del, &fakeAudit{})
	withCtx := func(form string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/admin/lgpd/delete-form", strings.NewReader(form))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "x", Host: "x"})
		ctx = iam.WithPrincipal(ctx, iam.Principal{UserID: uuid.New(), TenantID: tenantID})
		return r.WithContext(ctx)
	}
	cases := []struct {
		name string
		form string
	}{
		{"missing contact id", "justification=fine"},
		{"bad uuid", "contact_id=not-a-uuid&justification=fine"},
		{"missing justification", "contact_id=" + contactID.String()},
		{"justification too long", "contact_id=" + contactID.String() + "&justification=" + strings.Repeat("a", 5000)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			ui.DeleteForm(w, withCtx(c.form))
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", w.Code)
			}
		})
	}
}

func TestUI_DeleteForm_MissingTenant(t *testing.T) {
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	r := httptest.NewRequest(http.MethodPost, "/admin/lgpd/delete-form", strings.NewReader("contact_id="+uuid.New().String()+"&justification=ok"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	ui.DeleteForm(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", w.Code)
	}
}

func TestUI_Routes_Mount(t *testing.T) {
	ui := newUIHandler(t, &fakeLister{}, newFakeDeletions(), &fakeAudit{})
	mux := http.NewServeMux()
	ui.Routes(mux)
	// Just verify the routes are registered by hitting them through the
	// mux; the handlers themselves are tested above.
	cases := []struct {
		method string
		path   string
		want   int
	}{
		{http.MethodGet, "/admin/contacts/" + uuid.New().String() + "/lgpd", http.StatusInternalServerError}, // no tenant
		{http.MethodGet, "/admin/lgpd/requests", http.StatusInternalServerError},                             // no tenant
		{http.MethodPost, "/admin/lgpd/delete-form", http.StatusInternalServerError},                         // no tenant
	}
	for _, c := range cases {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, nil)
		mux.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s %s: code = %d; want %d", c.method, c.path, w.Code, c.want)
		}
	}
}
