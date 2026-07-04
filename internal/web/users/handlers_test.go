package users_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webusers "github.com/pericles-luz/crm/internal/web/users"
)

// fakeService implements the web/users.Service seam without a DB.
type fakeService struct {
	list      []tenantusers.User
	listErr   error
	createErr error
	updateErr error
	deactErr  error
	created   tenantusers.User
	lastEmail string
	lastRole  iam.Role
}

func (f *fakeService) List(_ context.Context, _ tenantusers.Actor) ([]tenantusers.User, error) {
	return f.list, f.listErr
}
func (f *fakeService) Get(_ context.Context, _ tenantusers.Actor, id uuid.UUID) (tenantusers.User, error) {
	for _, u := range f.list {
		if u.ID == id {
			return u, nil
		}
	}
	return tenantusers.User{}, tenantusers.ErrUserNotFound
}
func (f *fakeService) Create(_ context.Context, actor tenantusers.Actor, email string, role iam.Role) (tenantusers.User, tenantusers.Token, error) {
	f.lastEmail, f.lastRole = email, role
	if f.createErr != nil {
		return tenantusers.User{}, tenantusers.Token{}, f.createErr
	}
	u := tenantusers.User{ID: uuid.New(), TenantID: actor.TenantID, Email: email, Role: role}
	f.created = u
	f.list = append(f.list, u)
	return u, tenantusers.Token{Plaintext: "x"}, nil
}
func (f *fakeService) UpdateRole(_ context.Context, _ tenantusers.Actor, id uuid.UUID, role iam.Role) (tenantusers.User, error) {
	if f.updateErr != nil {
		return tenantusers.User{}, f.updateErr
	}
	return tenantusers.User{ID: id, Role: role}, nil
}
func (f *fakeService) Deactivate(_ context.Context, _ tenantusers.Actor, id uuid.UUID) (tenantusers.User, error) {
	if f.deactErr != nil {
		return tenantusers.User{}, f.deactErr
	}
	return tenantusers.User{ID: id}, nil
}
func (f *fakeService) Reactivate(_ context.Context, _ tenantusers.Actor, id uuid.UUID) (tenantusers.User, error) {
	return tenantusers.User{ID: id}, nil
}

func newHandler(t *testing.T, svc webusers.Service) http.Handler {
	t.Helper()
	h, err := webusers.New(webusers.Deps{Service: svc})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

// withCtx installs a Principal (session-derived tenant/user) and a tenancy
// context, mirroring what RequireAuth + tenant middleware provide in prod.
func withCtx(r *http.Request, tenantID, userID uuid.UUID) *http.Request {
	ctx := iam.WithPrincipal(r.Context(), iam.Principal{
		UserID:   userID,
		TenantID: tenantID,
		Roles:    []iam.Role{iam.RoleTenantGerente},
	})
	ctx = tenancy.WithContext(ctx, &tenancy.Tenant{ID: tenantID, Name: "Acme"})
	return r.WithContext(ctx)
}

func TestList_RendersPageWithHTMXAndHead(t *testing.T) {
	t.Parallel()
	tenantID, userID := uuid.New(), uuid.New()
	svc := &fakeService{list: []tenantusers.User{
		{ID: userID, TenantID: tenantID, Email: "boss@acme.example", Role: iam.RoleTenantGerente},
		{ID: uuid.New(), TenantID: tenantID, Email: "agent@acme.example", Role: iam.RoleTenantAtendente},
	}}
	h := newHandler(t, svc)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), tenantID, userID)
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`href="/static/css/tokens.css"`,
		`href="/static/css/components.css"`,
		`href="/static/css/users.css"`,
		`/static/vendor/htmx/2.0.9/htmx.min.js`,
		`id="users-list-inner"`,
		`boss@acme.example`,
		`agent@acme.example`,
		`>você<`, // the acting gerente row is flagged
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing %q", want)
		}
	}
	// The acting gerente is the only gerente → Desativar must be disabled
	// (last-active-gerente guard).
	if !strings.Contains(body, `disabled`) {
		t.Error("expected the last gerente's Desativar button to be disabled")
	}
}

func TestList_EmptyState(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if !strings.Contains(rec.Body.String(), "Nenhum usuário ainda") {
		t.Fatalf("expected empty state, got %q", rec.Body.String())
	}
}

func TestNewForm_DefaultsToAtendente(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodGet, "/settings/users/new", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	body := rec.Body.String()
	if !strings.Contains(body, `value="tenant_atendente" checked`) {
		t.Fatalf("least-privilege default (Atendente) not pre-selected: %q", body)
	}
	if strings.Contains(body, `value="master"`) {
		t.Fatal("form must never offer the master role")
	}
}

func TestCreate_Success_ReRendersList(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	svc := &fakeService{}
	h := newHandler(t, svc)
	form := url.Values{"email": {"new@acme.example"}, "role": {"tenant_atendente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), tenantID, uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if svc.lastEmail != "new@acme.example" || svc.lastRole != iam.RoleTenantAtendente {
		t.Fatalf("service got (%q,%q)", svc.lastEmail, svc.lastRole)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="users-list-inner"`) {
		t.Fatal("success must re-render the list partial")
	}
	if !strings.Contains(body, "criado") {
		t.Fatal("expected a success flash")
	}
}

func TestCreate_DuplicateEmail_422RetargetsForm(t *testing.T) {
	t.Parallel()
	svc := &fakeService{createErr: tenantusers.ErrEmailTaken}
	h := newHandler(t, svc)
	form := url.Values{"email": {"dup@acme.example"}, "role": {"tenant_atendente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if rec.Header().Get("HX-Retarget") != "#users-form" {
		t.Fatalf("HX-Retarget = %q, want #users-form", rec.Header().Get("HX-Retarget"))
	}
	if !strings.Contains(rec.Body.String(), "Já existe um usuário") {
		t.Fatalf("expected duplicate-email error copy, got %q", rec.Body.String())
	}
}

func TestCreate_InvalidRole_422(t *testing.T) {
	t.Parallel()
	svc := &fakeService{createErr: tenantusers.ErrInvalidRole}
	h := newHandler(t, svc)
	form := url.Values{"email": {"x@acme.example"}, "role": {"master"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users", strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
}

func TestDeactivate_LastGerente_409(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	svc := &fakeService{deactErr: tenantusers.ErrLastGerente}
	h := newHandler(t, svc)
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPost, "/settings/users/"+id.String()+"/deactivate", nil), uuid.New(), uuid.New())
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "último gerente") {
		t.Fatalf("expected anti-lockout copy, got %q", rec.Body.String())
	}
}

func TestUpdateRole_Success(t *testing.T) {
	t.Parallel()
	id := uuid.New()
	h := newHandler(t, &fakeService{})
	form := url.Values{"role": {"tenant_gerente"}}
	rec := httptest.NewRecorder()
	r := withCtx(httptest.NewRequest(http.MethodPatch, "/settings/users/"+id.String(), strings.NewReader(form.Encode())), uuid.New(), uuid.New())
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `id="users-list-inner"`) {
		t.Fatal("expected list re-render after role change")
	}
}

func TestMissingPrincipal_500(t *testing.T) {
	t.Parallel()
	h := newHandler(t, &fakeService{})
	rec := httptest.NewRecorder()
	// No principal in context.
	r := httptest.NewRequest(http.MethodGet, "/settings/users", nil)
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when principal missing", rec.Code)
	}
}
