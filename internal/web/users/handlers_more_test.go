package users_test

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webusers "github.com/pericles-luz/crm/internal/web/users"
)

func TestEditForm_RendersRoleAndReadonlyEmail(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	id := uuid.New()
	svc := &fakeService{list: []tenantusers.User{
		{ID: id, TenantID: tenantID, Email: "agent@acme.example", Role: iam.RoleTenantAtendente},
		// a second gerente so the edited gerente is not "last"
		{ID: uuid.New(), TenantID: tenantID, Email: "g2@acme.example", Role: iam.RoleTenantGerente},
	}}
	h := newHandler(t, svc)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users/"+id.String()+"/edit", nil), tenantID, uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `readonly`) {
		t.Error("edit form email must be readonly")
	}
	if !strings.Contains(body, `value="tenant_atendente" checked`) {
		t.Error("edit form must pre-select the current role")
	}
	if !strings.Contains(body, `hx-patch="/settings/users/`+id.String()) {
		t.Error("edit form must PATCH the user id")
	}
}

func TestEditForm_LastGerenteLocksDemote(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	id := uuid.New()
	svc := &fakeService{list: []tenantusers.User{
		{ID: id, TenantID: tenantID, Email: "boss@acme.example", Role: iam.RoleTenantGerente},
	}}
	h := newHandler(t, svc)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users/"+id.String()+"/edit", nil), tenantID, id)
	h.ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, `value="tenant_atendente" checked disabled`) &&
		!strings.Contains(body, `value="tenant_atendente"`) {
		t.Fatalf("body: %q", body)
	}
	if !strings.Contains(body, "último gerente") {
		t.Error("expected last-gerente demote hint")
	}
}

func TestEditForm_NotFound(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users/"+uuid.New().String()+"/edit", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestEditForm_BadID(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users/not-a-uuid/edit", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestList_InactiveUserRendersReactivate(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	past := time.Unix(0, 0)
	svc := &fakeService{list: []tenantusers.User{
		{ID: uuid.New(), TenantID: tenantID, Email: "gone@acme.example", Role: iam.RoleTenantAtendente, DeactivatedAt: &past},
	}}
	h := newHandler(t, svc)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), tenantID, uuid.New())
	h.ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, `/reactivate`) {
		t.Error("inactive user must show a Reactivate button")
	}
	if !strings.Contains(body, "Desativado") {
		t.Error("inactive user must show Desativado badge")
	}
}

func TestReactivate_Success(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users/"+id.String()+"/reactivate", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="users-list-inner"`) {
		t.Error("reactivate must re-render the list")
	}
}

func TestDeactivate_Success(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users/"+id.String()+"/deactivate", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestUpdateRole_NotFound(t *testing.T) {
	t.Parallel()
	svc := &fakeService{updateErr: tenantusers.ErrUserNotFound}
	h := newHandler(t, svc)
	form := url.Values{"role": {"tenant_gerente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPatch, "/settings/users/"+uuid.New().String(), strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUpdateRole_InvalidRole_422(t *testing.T) {
	t.Parallel()
	svc := &fakeService{updateErr: tenantusers.ErrInvalidRole}
	h := newHandler(t, svc)
	form := url.Values{"role": {"master"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPatch, "/settings/users/"+uuid.New().String(), strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestBadID_Mutations(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPatch, "/settings/users/bad"},
		{http.MethodPost, "/settings/users/bad/deactivate"},
		{http.MethodPost, "/settings/users/bad/reactivate"},
	} {
		rec := httptest.NewRecorder()
		r := withCtx(httptest.NewRequest(tc.method, tc.path, nil), uuid.New(), uuid.New())
		h.ServeHTTP(rec, r)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s %s: status = %d, want 400", tc.method, tc.path, rec.Code)
		}
	}
}

func TestNewForm_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/users/new", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestNew_RejectsNilService(t *testing.T) {
	t.Parallel()
	if _, err := webusers.New(webusers.Deps{}); err == nil {
		t.Fatal("want error for nil Service")
	}
}
