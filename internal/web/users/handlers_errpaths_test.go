package users_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/tenantusers"
)

func TestList_ServiceError_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{listErr: errors.New("db down")})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreate_InvalidEmail_422(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{createErr: tenantusers.ErrInvalidEmail})
	form := url.Values{"email": {"bad"}, "role": {"tenant_atendente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "e-mail válido") {
		t.Fatalf("expected invalid-email copy, got %q", rec.Body.String())
	}
}

func TestCreate_GenericError_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{createErr: errors.New("boom")})
	form := url.Values{"email": {"a@b.example"}, "role": {"tenant_atendente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreate_SuccessButListErrors_500(t *testing.T) {
	t.Parallel()
	// Create succeeds; the post-create list reload fails → 500.
	h := newHandler(t, &fakeService{listErr: errors.New("db down")})
	form := url.Values{"email": {"a@b.example"}, "role": {"tenant_atendente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestMutation_GenericError_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{deactErr: errors.New("boom")})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users/"+uuid.New().String()+"/deactivate", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUpdateRole_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/settings/users/"+uuid.New().String(), nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestReactivate_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/users/"+uuid.New().String()+"/reactivate", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestDeactivate_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/users/"+uuid.New().String()+"/deactivate", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestEditForm_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/settings/users/"+uuid.New().String()+"/edit", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreate_MissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/settings/users", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}
