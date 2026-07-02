package tenantusers_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/tenantusers"
	webtenantusers "github.com/pericles-luz/crm/internal/web/tenantusers"
)

var testTenant = &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"}

// fakeSvc is an in-memory userService. The real domain + storage behaviour
// is covered by internal/tenantusers/service_test.go and the postgres
// adapter integration tests; this fake exercises the HTTP transport only.
type fakeSvc struct {
	users        []*tenantusers.User
	tempPassword string
	listErr      error
	createErr    error
	updateErr    error
	deactErr     error

	lastRole   iam.Role
	lastTarget uuid.UUID
}

func (f *fakeSvc) ListUsers(_ context.Context, _ uuid.UUID) ([]*tenantusers.User, error) {
	return f.users, f.listErr
}

func (f *fakeSvc) CreateUser(_ context.Context, tenantID, _ uuid.UUID, email string, role iam.Role) (*tenantusers.CreateResult, error) {
	if f.createErr != nil {
		return nil, f.createErr
	}
	u := tenantusers.Hydrate(uuid.New(), tenantID, tenantusers.NormalizeEmail(email), role, true, true, time.Now())
	f.users = append(f.users, u)
	pw := f.tempPassword
	if pw == "" {
		pw = "Temp-Pass-Value-23"
	}
	return &tenantusers.CreateResult{User: u, TempPassword: pw}, nil
}

func (f *fakeSvc) UpdateUserRole(_ context.Context, _, _, targetID uuid.UUID, newRole iam.Role) error {
	f.lastTarget = targetID
	f.lastRole = newRole
	return f.updateErr
}

func (f *fakeSvc) DeactivateUser(_ context.Context, _, _, targetID uuid.UUID) error {
	f.lastTarget = targetID
	return f.deactErr
}

func newHandler(t *testing.T, svc webtenantusers.Deps) http.Handler {
	t.Helper()
	h, err := webtenantusers.New(svc)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	return mux
}

func do(t *testing.T, mux http.Handler, method, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	body := strings.NewReader("")
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	r = r.WithContext(tenancy.WithContext(r.Context(), testTenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	return rec
}

func seedUser(role iam.Role, active bool) *tenantusers.User {
	id := uuid.New()
	return tenantusers.Hydrate(id, testTenant.ID, "u-"+id.String()[:8]+"@acme.com", role, active, false, time.Now())
}

func TestNew_RequiresUsers(t *testing.T) {
	t.Parallel()
	if _, err := webtenantusers.New(webtenantusers.Deps{}); err == nil {
		t.Fatal("expected error when Users is nil")
	}
}

func TestPage_RendersShellAndTable(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{users: []*tenantusers.User{seedUser(iam.RoleTenantGerente, true)}}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodGet, "/settings/users", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-testid="tenantusers"`,
		`/static/css/users.css`,
		`/static/vendor/htmx/2.0.9/htmx.min.js`,
		"Gerente",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("page body missing %q", want)
		}
	}
	// Strict-CSP: no inline event handlers.
	for _, forbidden := range []string{"onclick=", "onchange=", "onsubmit=", "hx-on:"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("page body contains forbidden inline handler %q", forbidden)
		}
	}
}

func TestNewForm_RendersRoleSelect(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	rec := do(t, mux, http.MethodGet, "/settings/users/new", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`name="email"`, `name="role"`, "tenant_gerente", "tenant_atendente", "Novo usuário"} {
		if !strings.Contains(body, want) {
			t.Errorf("new form missing %q", want)
		}
	}
}

func TestCreate_ShowsTempPasswordOnce(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{tempPassword: "Zk7Rp9Tn2Wq4Xv6Bh3M"}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users", url.Values{
		"email": {"newbie@acme.com"},
		"role":  {"tenant_atendente"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-testid="tenantusers-credential"`) {
		t.Fatalf("missing credential card: %s", body)
	}
	if !strings.Contains(body, "Zk7Rp9Tn2Wq4Xv6Bh3M") {
		t.Errorf("temp password not shown: %s", body)
	}
	if !strings.Contains(body, "newbie@acme.com") {
		t.Errorf("email not shown in credential card")
	}
	// OOB list refresh present.
	if !strings.Contains(body, `id="tenantusers-list"`) {
		t.Errorf("expected OOB list refresh")
	}
}

func TestCreate_ValidationErrors(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		form  url.Values
		field string
	}{
		{"empty email", url.Values{"email": {""}, "role": {"tenant_atendente"}}, "email"},
		{"bad role", url.Values{"email": {"a@acme.com"}, "role": {"master"}}, "role"},
		{"escalation is_master", url.Values{"email": {"a@acme.com"}, "role": {"is_master"}}, "role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
			rec := do(t, mux, http.MethodPost, "/settings/users", tc.form)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, `class="field__error"`) {
				t.Errorf("expected an inline field error, got %s", body)
			}
			// A validation bounce must not leak a credential card.
			if strings.Contains(body, `data-testid="tenantusers-credential"`) {
				t.Errorf("validation bounce leaked a credential card")
			}
		})
	}
}

func TestCreate_EmailConflict(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{createErr: tenantusers.ErrEmailConflict}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users", url.Values{
		"email": {"dup@acme.com"}, "role": {"tenant_atendente"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Já existe um usuário") {
		t.Errorf("expected conflict message, got %s", rec.Body.String())
	}
}

func TestEditForm_PreselectsRole(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantGerente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodGet, "/settings/users/"+u.ID.String()+"/edit", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `value="tenant_gerente" selected`) {
		t.Errorf("gerente role not preselected: %s", body)
	}
	if !strings.Contains(body, u.Email) {
		t.Errorf("email not shown in edit form")
	}
}

func TestEditForm_UnknownUser404(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	rec := do(t, mux, http.MethodGet, "/settings/users/"+uuid.New().String()+"/edit", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestUpdateRole_Success(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantAtendente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+u.ID.String()+"/role", url.Values{"role": {"tenant_gerente"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if svc.lastRole != iam.RoleTenantGerente || svc.lastTarget != u.ID {
		t.Errorf("service not called with new role: role=%q target=%v", svc.lastRole, svc.lastTarget)
	}
	if !strings.Contains(rec.Body.String(), "Papel atualizado") {
		t.Errorf("expected success toast")
	}
}

func TestUpdateRole_LastGerenteRejected(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantGerente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}, updateErr: tenantusers.ErrLastGerente}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+u.ID.String()+"/role", url.Values{"role": {"tenant_atendente"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "último gerente") {
		t.Errorf("expected last-gerente guardrail message, got %s", rec.Body.String())
	}
}

func TestUpdateRole_BadRoleRejected(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantAtendente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+u.ID.String()+"/role", url.Values{"role": {"master"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `class="field__error"`) {
		t.Errorf("expected inline role error for escalation attempt")
	}
	if svc.lastRole == iam.RoleMaster {
		t.Errorf("escalation reached the service — must be rejected at boundary")
	}
}

func TestDeactivate_Success(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantAtendente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+u.ID.String()+"/deactivate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if svc.lastTarget != u.ID {
		t.Errorf("deactivate not called for target")
	}
	if !strings.Contains(rec.Body.String(), "desativado") {
		t.Errorf("expected deactivation toast")
	}
}

func TestDeactivate_LastGerenteRejected(t *testing.T) {
	t.Parallel()
	u := seedUser(iam.RoleTenantGerente, true)
	svc := &fakeSvc{users: []*tenantusers.User{u}, deactErr: tenantusers.ErrLastGerente}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+u.ID.String()+"/deactivate", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "último gerente") {
		t.Errorf("expected last-gerente guardrail message")
	}
}

func TestRowActions_BadUUID404(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	for _, target := range []string{
		"/settings/users/not-a-uuid/edit",
		"/settings/users/not-a-uuid/role",
		"/settings/users/not-a-uuid/deactivate",
	} {
		rec := do(t, mux, http.MethodPost, target, url.Values{"role": {"tenant_gerente"}})
		if target == "/settings/users/not-a-uuid/edit" {
			rec = do(t, mux, http.MethodGet, target, nil)
		}
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", target, rec.Code)
		}
	}
}

// stubCSRF / stubUserID / stubDir exercise the optional chrome
// collaborators so newPageData's non-nil branches are covered.
type stubDir struct{}

func (stubDir) LabelsByID(_ context.Context, _ uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	for _, id := range ids {
		out[id] = "Fulana Gerente"
	}
	return out, nil
}

func TestPage_WithChromeCollaborators(t *testing.T) {
	t.Parallel()
	// A user set spanning several roles exercises every roleLabel branch.
	users := []*tenantusers.User{
		seedUser(iam.RoleTenantGerente, true),
		seedUser(iam.RoleTenantAtendente, true),
		tenantusers.Hydrate(uuid.New(), testTenant.ID, "c@acme.com", iam.RoleTenantCommon, false, false, time.Now()),
		tenantusers.Hydrate(uuid.New(), testTenant.ID, "l@acme.com", iam.RoleTenantLider, true, false, time.Now()),
		tenantusers.Hydrate(uuid.New(), testTenant.ID, "adm@acme.com", iam.Role("admin"), true, true, time.Now()),
	}
	svc := &fakeSvc{users: users}
	mux := newHandler(t, webtenantusers.Deps{
		Users:      svc,
		CSRFToken:  func(*http.Request) string { return "csrf-token-xyz" },
		UserID:     func(*http.Request) uuid.UUID { return uuid.New() },
		UserLabels: stubDir{},
	})
	rec := do(t, mux, http.MethodGet, "/settings/users", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Gerente", "Atendente", "Comum", "Líder", "Admin (MFA)", "Senha temporária"} {
		if !strings.Contains(body, want) {
			t.Errorf("page missing role/label %q", want)
		}
	}
}

func TestPage_TenantMissing500(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	// No tenancy context on the request → tenant() fails → 500.
	r := httptest.NewRequest(http.MethodGet, "/settings/users", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestPage_ListError500(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{listErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodGet, "/settings/users", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCreate_ReloadError500(t *testing.T) {
	t.Parallel()
	// Create succeeds but the post-create list reload fails → 500.
	svc := &errAfterCreate{}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users", url.Values{"email": {"x@acme.com"}, "role": {"tenant_atendente"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// errAfterCreate returns success on CreateUser but errors on ListUsers so
// the create handler's reload-error branch is exercised.
type errAfterCreate struct{ fakeSvc }

func (e *errAfterCreate) ListUsers(context.Context, uuid.UUID) ([]*tenantusers.User, error) {
	return nil, errStub
}
func (e *errAfterCreate) CreateUser(_ context.Context, tenantID, _ uuid.UUID, email string, role iam.Role) (*tenantusers.CreateResult, error) {
	u := tenantusers.Hydrate(uuid.New(), tenantID, email, role, true, true, time.Now())
	return &tenantusers.CreateResult{User: u, TempPassword: "Temp-Pass-Value-23"}, nil
}

var errStub = stubError("boom")

type stubError string

func (e stubError) Error() string { return string(e) }

func TestNewForm_TenantMissing500(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	r := httptest.NewRequest(http.MethodGet, "/settings/users/new", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUpdateRole_InfraError500(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{updateErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+uuid.New().String()+"/role", url.Values{"role": {"tenant_gerente"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUpdateRole_ReloadError500(t *testing.T) {
	t.Parallel()
	// Update succeeds, then the list reload fails → renderRefresh 500 path.
	svc := &fakeSvc{listErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+uuid.New().String()+"/role", url.Values{"role": {"tenant_gerente"}})
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUpdateRole_LastGerente_LookupDegradesGracefully(t *testing.T) {
	t.Parallel()
	// Guardrail rejects AND the lookup for the bounced form fails: the
	// handler still renders a 200 form (empty email) rather than 500.
	svc := &fakeSvc{updateErr: tenantusers.ErrLastGerente, listErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+uuid.New().String()+"/role", url.Values{"role": {"tenant_atendente"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "último gerente") {
		t.Errorf("expected guardrail message")
	}
}

func TestDeactivate_InfraError500(t *testing.T) {
	t.Parallel()
	svc := &fakeSvc{deactErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+uuid.New().String()+"/deactivate", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestDeactivate_LastGerente_ReloadError500(t *testing.T) {
	t.Parallel()
	// Guardrail rejects, then the list reload for the error toast fails.
	svc := &fakeSvc{deactErr: tenantusers.ErrLastGerente, listErr: errStub}
	mux := newHandler(t, webtenantusers.Deps{Users: svc})
	rec := do(t, mux, http.MethodPost, "/settings/users/"+uuid.New().String()+"/deactivate", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestCancel_EmptyResponse(t *testing.T) {
	t.Parallel()
	mux := newHandler(t, webtenantusers.Deps{Users: &fakeSvc{}})
	rec := do(t, mux, http.MethodGet, "/settings/users/cancel", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Errorf("cancel should return empty body, got %q", rec.Body.String())
	}
}
