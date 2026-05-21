package lgpd_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	domainaudit "github.com/pericles-luz/crm/internal/iam/audit"
	domain "github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/web/lgpd"
)

// fakeExport implements domain.ExportRepository with in-memory data.
type fakeExport struct {
	contact  domain.ExportContact
	getErr   error
	idents   []domain.ExportIdentity
	convs    []domain.ExportConversation
	msgs     []domain.ExportMessage
	billing  []domain.ExportBillingEvent
	consents []domain.ExportConsent
}

func (f *fakeExport) GetContact(_ context.Context, _, _ uuid.UUID) (domain.ExportContact, error) {
	if f.getErr != nil {
		return domain.ExportContact{}, f.getErr
	}
	return f.contact, nil
}
func (f *fakeExport) ListIdentities(_ context.Context, _, _ uuid.UUID) ([]domain.ExportIdentity, error) {
	return f.idents, nil
}
func (f *fakeExport) ListConversations(_ context.Context, _, _ uuid.UUID) ([]domain.ExportConversation, error) {
	return f.convs, nil
}
func (f *fakeExport) ListMessages(_ context.Context, _, _ uuid.UUID) ([]domain.ExportMessage, error) {
	return f.msgs, nil
}
func (f *fakeExport) ListBillingEvents(_ context.Context, _, _ uuid.UUID) ([]domain.ExportBillingEvent, error) {
	return f.billing, nil
}
func (f *fakeExport) ListConsents(_ context.Context, _ uuid.UUID) ([]domain.ExportConsent, error) {
	return f.consents, nil
}

// fakeDeletions records every Upsert call.
type fakeDeletions struct {
	mu      sync.Mutex
	upserts []domain.DeletionRequest
	pending map[uuid.UUID]domain.DeletionRequest
	upErr   error
}

func newFakeDeletions() *fakeDeletions {
	return &fakeDeletions{pending: map[uuid.UUID]domain.DeletionRequest{}}
}
func (f *fakeDeletions) Upsert(_ context.Context, req domain.DeletionRequest) (domain.DeletionRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.upErr != nil {
		return domain.DeletionRequest{}, f.upErr
	}
	// Idempotency: if a pending row exists for the contact, update it.
	if existing, ok := f.pending[req.ContactID]; ok {
		existing.Justification = req.Justification
		existing.RetentionUntil = req.RetentionUntil
		f.pending[req.ContactID] = existing
		f.upserts = append(f.upserts, existing)
		return existing, nil
	}
	if req.ID == uuid.Nil {
		req.ID = uuid.New()
	}
	f.pending[req.ContactID] = req
	f.upserts = append(f.upserts, req)
	return req, nil
}
func (f *fakeDeletions) Get(context.Context, uuid.UUID) (domain.DeletionRequest, error) {
	return domain.DeletionRequest{}, domain.ErrDeletionRequestNotFound
}
func (f *fakeDeletions) ListReady(context.Context, time.Time, int) ([]domain.DeletionRequest, error) {
	return nil, nil
}
func (f *fakeDeletions) MarkCompleted(context.Context, uuid.UUID, time.Time) error { return nil }
func (f *fakeDeletions) MarkFailed(context.Context, uuid.UUID, time.Time) error    { return nil }

type fakeAudit struct {
	mu     sync.Mutex
	events []domainaudit.DataAuditEvent
}

func (f *fakeAudit) WriteData(_ context.Context, e domainaudit.DataAuditEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}

func buildHandler(t *testing.T, exp *fakeExport, del *fakeDeletions, aud *fakeAudit) *lgpd.Handler {
	t.Helper()
	h, err := lgpd.New(lgpd.Deps{
		Export:    exp,
		Deletions: del,
		Audit:     aud,
		Now:       func() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	return h
}

func reqWithCtx(method, path string, body io.Reader, tenantID, userID uuid.UUID) *http.Request {
	r := httptest.NewRequest(method, path, body)
	ctx := tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: tenantID, Name: "tenant-A"})
	ctx = iam.WithPrincipal(ctx, iam.Principal{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    []iam.Role{iam.RoleTenantGerente},
	})
	return r.WithContext(ctx)
}

func TestNew_RequiresPorts(t *testing.T) {
	if _, err := lgpd.New(lgpd.Deps{}); err == nil {
		t.Fatal("New(empty) err = nil")
	}
	if _, err := lgpd.New(lgpd.Deps{Export: &fakeExport{}}); err == nil {
		t.Fatal("New(no deletions) err = nil")
	}
	if _, err := lgpd.New(lgpd.Deps{Export: &fakeExport{}, Deletions: newFakeDeletions()}); err == nil {
		t.Fatal("New(no audit) err = nil")
	}
}

func TestExport_ReturnsZipWithBothFiles(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	exp := &fakeExport{
		contact: domain.ExportContact{ID: contactID, TenantID: tenantID, DisplayName: "Maria"},
	}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)

	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodGet, "/admin/lgpd/export?contact_id="+contactID.String(), nil, tenantID, uuid.New())
	h.Export(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); got != "application/zip" {
		t.Errorf("Content-Type = %q, want application/zip", got)
	}
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment;") {
		t.Errorf("Content-Disposition = %q, want attachment; …", w.Header().Get("Content-Disposition"))
	}

	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader err = %v", err)
	}
	if len(zr.File) != 2 {
		t.Errorf("zip file count = %d, want 2", len(zr.File))
	}

	if len(aud.events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(aud.events))
	}
	if aud.events[0].Event != domainaudit.DataEventLGPDExport {
		t.Errorf("audit event = %q, want lgpd_export", aud.events[0].Event)
	}
	if aud.events[0].Target["contact_id"] != contactID.String() {
		t.Errorf("audit target contact_id = %v, want %s", aud.events[0].Target["contact_id"], contactID)
	}
}

func TestExport_MissingContactID_400(t *testing.T) {
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodGet, "/admin/lgpd/export", nil, uuid.New(), uuid.New())
	h.Export(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestExport_ContactNotFound_404(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	exp := &fakeExport{getErr: domain.ErrDeletionRequestNotFound}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodGet, "/admin/lgpd/export?contact_id="+contactID.String(), nil, tenantID, uuid.New())
	h.Export(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
	if len(aud.events) != 0 {
		t.Errorf("audit events on 404 = %d, want 0", len(aud.events))
	}
}

func TestExport_AuditFailure_500(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	exp := &fakeExport{contact: domain.ExportContact{ID: contactID, TenantID: tenantID}}
	del := newFakeDeletions()
	aud := failingAudit{}
	h, err := lgpd.New(lgpd.Deps{Export: exp, Deletions: del, Audit: aud})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodGet, "/admin/lgpd/export?contact_id="+contactID.String(), nil, tenantID, uuid.New())
	h.Export(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestDelete_PersistsRequestAndAudits(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)

	body := `{"contact_id":"` + contactID.String() + `","justification":"data subject request 2026-05-21"}`
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, userID)
	h.Delete(w, r)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID             string `json:"id"`
		Status         string `json:"status"`
		RetentionUntil string `json:"retention_until"`
		ContactID      string `json:"contact_id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp err = %v", err)
	}
	if resp.ContactID != contactID.String() {
		t.Errorf("resp.ContactID = %s, want %s", resp.ContactID, contactID)
	}
	if resp.Status != "pending" {
		t.Errorf("resp.Status = %s, want pending", resp.Status)
	}
	if resp.RetentionUntil == "" {
		t.Error("resp.RetentionUntil empty")
	}
	if len(del.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(del.upserts))
	}
	if del.upserts[0].RequestedByUserID != userID {
		t.Errorf("RequestedByUserID = %s, want %s", del.upserts[0].RequestedByUserID, userID)
	}
	if len(aud.events) != 1 || aud.events[0].Event != domainaudit.DataEventLGPDForget {
		t.Errorf("audit events = %+v, want one lgpd_forget", aud.events)
	}
}

func TestDelete_IdempotentReturnsSameID(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	userID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)

	body := `{"contact_id":"` + contactID.String() + `","justification":"first"}`
	w1 := httptest.NewRecorder()
	h.Delete(w1, reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, userID))
	var resp1 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w1.Body.Bytes(), &resp1)

	body2 := `{"contact_id":"` + contactID.String() + `","justification":"second"}`
	w2 := httptest.NewRecorder()
	h.Delete(w2, reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body2), tenantID, userID))
	var resp2 struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)

	if resp1.ID == "" || resp1.ID != resp2.ID {
		t.Errorf("idempotency broken: first=%s second=%s", resp1.ID, resp2.ID)
	}
}

func TestDelete_ValidatesBody(t *testing.T) {
	tenantID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)

	cases := map[string]string{
		"missing contact":       `{"justification":"x"}`,
		"missing justification": `{"contact_id":"` + uuid.New().String() + `"}`,
		"bad json":              `{`,
		"unknown field":         `{"contact_id":"` + uuid.New().String() + `","justification":"x","foo":1}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, uuid.New())
			h.Delete(w, r)
			if w.Code != http.StatusBadRequest {
				t.Errorf("%s: status = %d, want 400", name, w.Code)
			}
		})
	}
}

func TestDelete_UpsertError_500(t *testing.T) {
	tenantID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	del.upErr = errors.New("db down")
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	body := `{"contact_id":"` + uuid.New().String() + `","justification":"x"}`
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, uuid.New())
	h.Delete(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// failingAudit always errors so TestExport_AuditFailure can assert the
// non-repudiation contract.
type failingAudit struct{}

func (failingAudit) WriteData(context.Context, domainaudit.DataAuditEvent) error {
	return errors.New("audit down")
}

func TestRoutes_RegistersBothEndpoints(t *testing.T) {
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	mux := http.NewServeMux()
	h.Routes(mux)
	for _, route := range []struct {
		method, path string
		wantStatus   int
	}{
		{http.MethodGet, "/admin/lgpd/export?contact_id=" + uuid.New().String(), http.StatusInternalServerError}, // tenant missing
		{http.MethodPost, "/admin/lgpd/delete", http.StatusInternalServerError},
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
		mux.ServeHTTP(w, r)
		if w.Code == http.StatusNotFound {
			t.Errorf("%s %s = 404, want a real handler response", route.method, route.path)
		}
	}
}

func TestExport_FailsWithoutTenant(t *testing.T) {
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	w := httptest.NewRecorder()
	// no tenancy ctx
	r := httptest.NewRequest(http.MethodGet, "/admin/lgpd/export?contact_id="+uuid.New().String(), nil)
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{UserID: uuid.New()}))
	h.Export(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestDelete_FailsWithoutTenant(t *testing.T) {
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	w := httptest.NewRecorder()
	body := `{"contact_id":"` + uuid.New().String() + `","justification":"x"}`
	r := httptest.NewRequest(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body))
	r = r.WithContext(iam.WithPrincipal(r.Context(), iam.Principal{UserID: uuid.New()}))
	h.Delete(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

// listFailingExport flips one of the list calls into an error so
// buildBundle's per-step error wrapping is exercised.
type listFailingExport struct {
	failOn string
}

func (l *listFailingExport) GetContact(context.Context, uuid.UUID, uuid.UUID) (domain.ExportContact, error) {
	if l.failOn == "contact" {
		return domain.ExportContact{}, errors.New("boom")
	}
	return domain.ExportContact{}, nil
}
func (l *listFailingExport) ListIdentities(context.Context, uuid.UUID, uuid.UUID) ([]domain.ExportIdentity, error) {
	if l.failOn == "identities" {
		return nil, errors.New("boom")
	}
	return nil, nil
}
func (l *listFailingExport) ListConversations(context.Context, uuid.UUID, uuid.UUID) ([]domain.ExportConversation, error) {
	if l.failOn == "conversations" {
		return nil, errors.New("boom")
	}
	return nil, nil
}
func (l *listFailingExport) ListMessages(context.Context, uuid.UUID, uuid.UUID) ([]domain.ExportMessage, error) {
	if l.failOn == "messages" {
		return nil, errors.New("boom")
	}
	return nil, nil
}
func (l *listFailingExport) ListBillingEvents(context.Context, uuid.UUID, uuid.UUID) ([]domain.ExportBillingEvent, error) {
	if l.failOn == "billing" {
		return nil, errors.New("boom")
	}
	return nil, nil
}
func (l *listFailingExport) ListConsents(context.Context, uuid.UUID) ([]domain.ExportConsent, error) {
	if l.failOn == "consents" {
		return nil, errors.New("boom")
	}
	return nil, nil
}

func TestExport_PerStepFailure_500(t *testing.T) {
	for _, step := range []string{"contact", "identities", "conversations", "messages", "billing", "consents"} {
		t.Run(step, func(t *testing.T) {
			tenantID := uuid.New()
			contactID := uuid.New()
			del := newFakeDeletions()
			aud := &fakeAudit{}
			h, err := lgpd.New(lgpd.Deps{Export: &listFailingExport{failOn: step}, Deletions: del, Audit: aud})
			if err != nil {
				t.Fatalf("New err = %v", err)
			}
			w := httptest.NewRecorder()
			r := reqWithCtx(http.MethodGet, "/admin/lgpd/export?contact_id="+contactID.String(), nil, tenantID, uuid.New())
			h.Export(w, r)
			if w.Code != http.StatusInternalServerError {
				t.Errorf("failOn=%s status = %d, want 500", step, w.Code)
			}
		})
	}
}

func TestDelete_AuditFailure_500(t *testing.T) {
	tenantID := uuid.New()
	contactID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	h, err := lgpd.New(lgpd.Deps{Export: exp, Deletions: del, Audit: failingAudit{}})
	if err != nil {
		t.Fatalf("New err = %v", err)
	}
	body := `{"contact_id":"` + contactID.String() + `","justification":"x"}`
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, uuid.New())
	h.Delete(w, r)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestParseContactQuery_BadUUID(t *testing.T) {
	tenantID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodGet, "/admin/lgpd/export?contact_id=not-a-uuid", nil, tenantID, uuid.New())
	h.Export(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestDelete_JustificationTooLong(t *testing.T) {
	tenantID := uuid.New()
	exp := &fakeExport{}
	del := newFakeDeletions()
	aud := &fakeAudit{}
	h := buildHandler(t, exp, del, aud)
	long := strings.Repeat("x", 4097)
	body := `{"contact_id":"` + uuid.New().String() + `","justification":"` + long + `"}`
	w := httptest.NewRecorder()
	r := reqWithCtx(http.MethodPost, "/admin/lgpd/delete", strings.NewReader(body), tenantID, uuid.New())
	h.Delete(w, r)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for oversize justification", w.Code)
	}
}
