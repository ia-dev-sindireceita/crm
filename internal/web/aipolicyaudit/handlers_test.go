package aipolicyaudit_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/aipolicy"
	"github.com/pericles-luz/crm/internal/tenancy"
	webaudit "github.com/pericles-luz/crm/internal/web/aipolicyaudit"
)

// fakeQuery is the in-memory AuditQuery the handler tests run
// against. captured holds the last call so tests can assert the
// derived AuditPageQuery (tenant id, filters, cursor).
type fakeQuery struct {
	page     aipolicy.AuditPage
	err      error
	captured aipolicy.AuditPageQuery
}

func (f *fakeQuery) Page(_ context.Context, q aipolicy.AuditPageQuery) (aipolicy.AuditPage, error) {
	f.captured = q
	if f.err != nil {
		return aipolicy.AuditPage{}, f.err
	}
	return f.page, nil
}

func newHandler(t *testing.T, q aipolicy.AuditQuery) http.Handler {
	t.Helper()
	h, err := webaudit.New(webaudit.Deps{
		Query:  q,
		Now:    func() time.Time { return time.Date(2026, 5, 16, 12, 0, 0, 0, time.UTC) },
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func tenantFixture(t *testing.T) tenancy.Tenant {
	t.Helper()
	return tenancy.Tenant{
		ID:   uuid.MustParse("11111111-1111-1111-1111-111111111111"),
		Name: "tenant-fixture",
		Host: "tenant-fixture.example",
	}
}

// TestNew_RequiresQuery rejects a nil read port at construction.
func TestNew_RequiresQuery(t *testing.T) {
	if _, err := webaudit.New(webaudit.Deps{}); err == nil {
		t.Fatal("New(nil Query): err = nil, want rejection")
	}
}

// TestViewTenant_RendersTenantSlice covers the happy path: the
// handler renders a 200 with the table populated from Query.Page
// and propagates the request tenant into the AuditPageQuery.
func TestViewTenant_RendersTenantSlice(t *testing.T) {
	tenant := tenantFixture(t)
	q := &fakeQuery{page: aipolicy.AuditPage{Events: []aipolicy.AuditRecord{
		{
			ID:          uuid.New(),
			TenantID:    tenant.ID,
			ScopeType:   aipolicy.ScopeTenant,
			ScopeID:     tenant.ID.String(),
			Field:       "ai_enabled",
			OldValue:    true,
			NewValue:    false,
			ActorUserID: uuid.MustParse("22222222-2222-2222-2222-222222222222"),
			ActorMaster: false,
			CreatedAt:   time.Date(2026, 5, 10, 9, 0, 0, 0, time.UTC),
		},
	}}}
	srv := newHandler(t, q)

	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if q.captured.TenantID != tenant.ID {
		t.Fatalf("Query.TenantID = %v, want %v", q.captured.TenantID, tenant.ID)
	}
	if !strings.Contains(rec.Body.String(), "ai_enabled") {
		t.Fatalf("body missing ai_enabled field: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Auditoria de AI policy") {
		t.Fatalf("body missing tenant title: %s", rec.Body.String())
	}
}

// TestViewTenant_MasterPillRenderedOnMasterRow covers the AC #2
// visualisation: an audit row with actor_master = true picks up the
// audit-row--master CSS class and the "master" pill.
func TestViewTenant_MasterPillRenderedOnMasterRow(t *testing.T) {
	tenant := tenantFixture(t)
	q := &fakeQuery{page: aipolicy.AuditPage{Events: []aipolicy.AuditRecord{
		{
			ID: uuid.New(), TenantID: tenant.ID,
			ScopeType: aipolicy.ScopeTenant, ScopeID: tenant.ID.String(),
			Field: "ai_enabled", OldValue: true, NewValue: false,
			ActorUserID: uuid.New(), ActorMaster: true,
			CreatedAt: time.Now().UTC(),
		},
	}}}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "audit-row--master") {
		t.Fatalf("master row missing audit-row--master class:\n%s", body)
	}
	if !strings.Contains(body, ">master<") {
		t.Fatalf("master pill missing:\n%s", body)
	}
}

// TestViewTenant_PropagatesScopeAndPeriodFilters proves the query
// parameters land on the AuditPageQuery so the database does the
// filtering, not the renderer.
func TestViewTenant_PropagatesScopeAndPeriodFilters(t *testing.T) {
	tenant := tenantFixture(t)
	q := &fakeQuery{}
	srv := newHandler(t, q)
	u := url.URL{
		Path: "/settings/ai-policy/audit",
		RawQuery: url.Values{
			"scope_type": []string{"channel"},
			"scope_id":   []string{"whatsapp"},
			"since":      []string{"2026-05-01"},
			"until":      []string{"2026-05-15"},
		}.Encode(),
	}
	req := httptest.NewRequest(http.MethodGet, u.String(), nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	srv.ServeHTTP(httptest.NewRecorder(), req)

	if q.captured.ScopeType != aipolicy.ScopeChannel || q.captured.ScopeID != "whatsapp" {
		t.Fatalf("scope = %s/%s, want channel/whatsapp", q.captured.ScopeType, q.captured.ScopeID)
	}
	if q.captured.Since.IsZero() || q.captured.Until.IsZero() {
		t.Fatalf("since/until not propagated: %+v", q.captured)
	}
}

// TestViewTenant_RendersNextCursorLink confirms the pager appears
// whenever Query returns a non-zero Next cursor.
func TestViewTenant_RendersNextCursorLink(t *testing.T) {
	tenant := tenantFixture(t)
	q := &fakeQuery{page: aipolicy.AuditPage{
		Events: []aipolicy.AuditRecord{{ID: uuid.New(), TenantID: tenant.ID,
			ScopeType: aipolicy.ScopeTenant, ScopeID: tenant.ID.String(),
			Field: "model", OldValue: "a", NewValue: "b",
			ActorUserID: uuid.New(), CreatedAt: time.Now().UTC()}},
		Next: aipolicy.AuditCursor{CreatedAt: time.Now().UTC(), ID: uuid.New()},
	}}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "cursor=") {
		t.Fatalf("body missing pager link: %s", rec.Body.String())
	}
}

// TestViewTenant_TenantMissingIs500 fails-closed on a request that
// reached the handler without TenantScope middleware in front. The
// privacy handler does the same: a missing tenant is a wiring bug,
// not a user error, so we surface 500 rather than render an empty
// or wrong-tenant page.
func TestViewTenant_TenantMissingIs500(t *testing.T) {
	q := &fakeQuery{}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestViewTenant_QueryErrorIs500 propagates the read failure as a
// 500. The body is the generic http.StatusText to avoid leaking
// internal-error details.
func TestViewTenant_QueryErrorIs500(t *testing.T) {
	tenant := tenantFixture(t)
	q := &fakeQuery{err: errors.New("boom")}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestViewMaster_RequiresTenantParameter rejects /admin/audit calls
// without a tenant uuid — the master view is per-tenant and we do
// not silently default to "all tenants".
func TestViewMaster_RequiresTenantParameter(t *testing.T) {
	srv := newHandler(t, &fakeQuery{})
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?module=ai-policy", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestViewMaster_RejectsInvalidTenantUUID rejects garbage in the
// tenant parameter rather than passing uuid.Nil downstream.
func TestViewMaster_RejectsInvalidTenantUUID(t *testing.T) {
	srv := newHandler(t, &fakeQuery{})
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?module=ai-policy&tenant=not-a-uuid", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

// TestViewMaster_RendersTargetTenantSlice covers the happy master
// path: the parsed tenant param is what hits AuditPageQuery.
func TestViewMaster_RendersTargetTenantSlice(t *testing.T) {
	target := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	q := &fakeQuery{page: aipolicy.AuditPage{Events: []aipolicy.AuditRecord{{
		ID: uuid.New(), TenantID: target,
		ScopeType: aipolicy.ScopeTenant, ScopeID: target.String(),
		Field: "ai_enabled", OldValue: true, NewValue: false,
		ActorUserID: uuid.New(), ActorMaster: true,
		CreatedAt: time.Now().UTC(),
	}}}}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?module=ai-policy&tenant="+target.String(), nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if q.captured.TenantID != target {
		t.Fatalf("Query.TenantID = %v, want %v", q.captured.TenantID, target)
	}
	if !strings.Contains(rec.Body.String(), "Auditoria master") {
		t.Fatalf("master title missing: %s", rec.Body.String())
	}
}

// TestViewMaster_UnknownModuleReturns501 protects the reserved
// module=... surface so other modules can be added in follow-up
// child issues without quietly returning ai-policy data.
func TestViewMaster_UnknownModuleReturns501(t *testing.T) {
	srv := newHandler(t, &fakeQuery{})
	req := httptest.NewRequest(http.MethodGet, "/admin/audit?module=billing&tenant=44444444-4444-4444-4444-444444444444", nil)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", rec.Code)
	}
}

// TestCursorRoundTrip ensures a cursor minted by the handler decodes
// back to the same (CreatedAt, ID) pair so the pager link is stable.
func TestCursorRoundTrip(t *testing.T) {
	tenant := tenantFixture(t)
	want := aipolicy.AuditCursor{CreatedAt: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), ID: uuid.New()}
	q := &fakeQuery{page: aipolicy.AuditPage{
		Events: []aipolicy.AuditRecord{{ID: uuid.New(), TenantID: tenant.ID,
			ScopeType: aipolicy.ScopeTenant, ScopeID: tenant.ID.String(),
			Field: "model", OldValue: "x", NewValue: "y",
			ActorUserID: uuid.New(), CreatedAt: time.Now().UTC()}},
		Next: want,
	}}
	srv := newHandler(t, q)
	req := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit", nil)
	req = req.WithContext(tenancy.WithContext(req.Context(), &tenant))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	body := rec.Body.String()
	idx := strings.Index(body, "cursor=")
	if idx < 0 {
		t.Fatalf("cursor token missing: %s", body)
	}
	// Trim to the cursor token through to the next & or ".
	tail := body[idx+len("cursor="):]
	end := len(tail)
	for i, c := range tail {
		if c == '&' || c == '"' {
			end = i
			break
		}
	}
	token := tail[:end]
	// Follow the link to confirm decode → AuditPageQuery.Cursor matches.
	q.captured = aipolicy.AuditPageQuery{}
	req2 := httptest.NewRequest(http.MethodGet, "/settings/ai-policy/audit?cursor="+token, nil)
	req2 = req2.WithContext(tenancy.WithContext(req2.Context(), &tenant))
	srv.ServeHTTP(httptest.NewRecorder(), req2)
	if !q.captured.Cursor.CreatedAt.Equal(want.CreatedAt) || q.captured.Cursor.ID != want.ID {
		t.Fatalf("cursor round-trip drift: got %+v, want %+v", q.captured.Cursor, want)
	}
}
