package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// These tests cover the wire-up surface that does not require a live
// Postgres/Redis. assembleDeps + runApp are exercised end-to-end by
// the staging smoke test (SIN-62348 §"Smoke test (staging)") and by
// the postgres/redis adapters' integration suites — repeating that
// here would just shadow real infra coverage.

func TestExecute_HealthOnlyMode_FallsBackToBareMux(t *testing.T) {
	t.Parallel()
	addr := freePort(t)
	getenv := func(k string) string {
		if k == "HTTP_ADDR" {
			return addr
		}
		// DATABASE_URL intentionally unset — execute() must take the
		// run() (health-only) branch.
		return ""
	}
	ctx, cancel := context.WithCancel(context.Background())
	codeCh := make(chan int, 1)
	go func() { codeCh <- execute(ctx, getenv) }()

	waitForListening(t, addr)

	// /login must NOT be mounted in health-only mode.
	resp, err := http.Get("http://" + addr + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("health-only mode: /login status = %d, want 404", resp.StatusCode)
	}

	cancel()
	select {
	case code := <-codeCh:
		if code != 0 {
			t.Fatalf("execute returned %d, want 0", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("execute did not return after cancel")
	}
}

func TestExecute_AppMode_ReturnsErrorOnUnreachableDB(t *testing.T) {
	t.Parallel()
	getenv := func(k string) string {
		switch k {
		case "DATABASE_URL":
			return "postgres://localhost:1/does-not-exist?connect_timeout=1&sslmode=disable"
		case "REDIS_URL":
			return "redis://localhost:1"
		case "HTTP_ADDR":
			return freePort(t)
		}
		return ""
	}
	// runApp must surface the postgres connection error and execute
	// must therefore return non-zero.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if code := execute(ctx, getenv); code != 1 {
		t.Fatalf("execute returned %d, want 1", code)
	}
}

func TestOpenRedis_EmptyURL_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := openRedis(context.Background(), "")
	if err == nil {
		t.Fatal("openRedis(\"\") returned nil error, want non-nil")
	}
}

func TestOpenRedis_InvalidURL_ReturnsError(t *testing.T) {
	t.Parallel()
	_, err := openRedis(context.Background(), "::: not a url :::")
	if err == nil {
		t.Fatal("openRedis(invalid) returned nil error, want non-nil")
	}
}

// stubResolver wires a static host→tenant table so tenantLoginAdapter
// + iamTenantResolver can be exercised without Postgres.
type stubResolver struct {
	tenants map[string]*tenancy.Tenant
}

func (r stubResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	t, ok := r.tenants[host]
	if !ok {
		return nil, tenancy.ErrTenantNotFound
	}
	return t, nil
}

func TestIAMTenantResolver_ResolveByHost_Found(t *testing.T) {
	t.Parallel()
	want := uuid.New()
	r := iamTenantResolver{inner: stubResolver{tenants: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: want, Name: "Acme", Host: "acme.crm.local"},
	}}}
	got, err := r.ResolveByHost(context.Background(), "acme.crm.local")
	if err != nil {
		t.Fatalf("ResolveByHost: %v", err)
	}
	if got != want {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestIAMTenantResolver_ResolveByHost_NotFound_TranslatesToIAMSentinel(t *testing.T) {
	t.Parallel()
	r := iamTenantResolver{inner: stubResolver{tenants: map[string]*tenancy.Tenant{}}}
	_, err := r.ResolveByHost(context.Background(), "ghost.crm.local")
	if !errors.Is(err, iam.ErrTenantNotFound) {
		t.Fatalf("err = %v, want iam.ErrTenantNotFound", err)
	}
}

func TestIAMTenantResolver_ResolveByHost_OtherErrorPropagates(t *testing.T) {
	t.Parallel()
	want := errors.New("postgres: timeout")
	r := iamTenantResolver{inner: errResolver{err: want}}
	_, err := r.ResolveByHost(context.Background(), "x")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want wrap of %v", err, want)
	}
}

type errResolver struct{ err error }

func (r errResolver) ResolveByHost(_ context.Context, _ string) (*tenancy.Tenant, error) {
	return nil, r.err
}

func TestTenantLoginAdapter_NoTenantInContext_Errors(t *testing.T) {
	t.Parallel()
	d := &deps{} // pool nil — but we never reach it without a tenant in ctx.
	fn := tenantLoginAdapter(d)
	_, err := fn(context.Background(), "x", "a@b.test", "pwd", nil, "", "")
	if err == nil {
		t.Fatal("tenantLoginAdapter without tenant context: err = nil, want non-nil")
	}
	if !errors.Is(err, tenancy.ErrNoTenantInContext) {
		t.Fatalf("err = %v, want wrap of tenancy.ErrNoTenantInContext", err)
	}
}

func TestNewMasterServiceFactory_RejectsZeroActor(t *testing.T) {
	t.Parallel()
	factory := newMasterServiceFactory(masterFactoryDeps{}) // pool nil — irrelevant: actor check first.
	_, err := factory(uuid.Nil)
	if err == nil {
		t.Fatal("master factory with uuid.Nil: err = nil, want non-nil")
	}
}

func TestNewAppMux_HealthRoute(t *testing.T) {
	t.Parallel()
	d := &deps{tenants: stubResolver{}, logger: nil}
	mux := newAppMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/health status = %d, want 200", rec.Code)
	}
}

func TestNewAppMux_LoginRouteMounted(t *testing.T) {
	t.Parallel()
	// stubResolver returns ErrTenantNotFound for any host — the
	// TenantScope middleware must convert that to a 404 with the
	// generic body, proving the middleware ran on /login.
	d := &deps{tenants: stubResolver{}}
	mux := newAppMux(d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader("email=a%40b.test&password=x"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/login (unknown host) status = %d, want 404 (TenantScope)", rec.Code)
	}
}

// TestNewAppMux_HelloTenantRequiresAuth proves the SIN-62217 chi router
// applies the Auth middleware to /hello-tenant — an unauthenticated
// request must redirect to /login?next=/hello-tenant (302), not 200.
func TestNewAppMux_HelloTenantRequiresAuth(t *testing.T) {
	t.Parallel()
	tenantID := uuid.New()
	d := &deps{tenants: stubResolver{tenants: map[string]*tenancy.Tenant{
		"acme.crm.local": {ID: tenantID, Name: "Acme", Host: "acme.crm.local"},
	}}}
	mux := newAppMux(d)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/hello-tenant", nil)
	req.Host = "acme.crm.local"
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("status = %d, want 302 (Auth → login redirect)", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "/login") {
		t.Fatalf("Location = %q, want /login redirect", loc)
	}
}

// TestNewAppMux_MasterLoginMounted verifies that GET /m/login returns a
// non-404 when MasterDeps is populated. When MASTER_MFA_KEY is unset the
// /m/* routes are skipped; this test constructs MasterDeps directly to
// confirm the router wires the group.
func TestNewAppMux_MasterLoginMounted(t *testing.T) {
	t.Parallel()
	d := &deps{
		tenants: stubResolver{},
		master:  newMasterDepsForTest(),
	}
	mux := newAppMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/login", nil))
	// The stub handler returns 200; 404 means the route is not mounted.
	if rec.Code == http.StatusNotFound {
		t.Fatalf("GET /m/login: status 404 — route not mounted")
	}
}

// TestNewAppMux_MasterLoginSkippedWhenDepsNil confirms /m/* routes are
// absent when MasterDeps is zero (MASTER_MFA_KEY unset).
func TestNewAppMux_MasterLoginSkippedWhenDepsNil(t *testing.T) {
	t.Parallel()
	d := &deps{tenants: stubResolver{}}
	mux := newAppMux(d)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/m/login", nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("GET /m/login: status 200 but routes should not be mounted")
	}
}

// newMasterDepsForTest returns a minimal httpapi.MasterDeps with nop
// handlers and passthrough middleware suitable for route-mount smoke tests.
func newMasterDepsForTest() httpapi.MasterDeps {
	nop := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	pass := func(next http.Handler) http.Handler { return next }
	return httpapi.MasterDeps{
		Login:             nop,
		Logout:            nop,
		Enroll:            nop,
		Verify:            nop,
		Regenerate:        nop,
		RequireMasterAuth: pass,
		RequireMasterMFA:  pass,
	}
}

// TestTenantIAMAdapter_LockoutFreeService_BuildsServiceLiteral covers
// the seam tenantIAMAdapter.Logout/ValidateSession share. Both go
// through lockoutFreeService(); a returned non-nil *iam.Service whose
// Lockouts field is nil proves the expected wiring (per the docstring,
// the lockout-free path is intentional because tenantID is explicit).
func TestTenantIAMAdapter_LockoutFreeService_BuildsServiceLiteral(t *testing.T) {
	t.Parallel()
	d := &deps{tenants: stubResolver{}, logger: nil}
	a := tenantIAMAdapter{deps: d}
	svc := a.lockoutFreeService()
	if svc == nil {
		t.Fatal("lockoutFreeService returned nil")
	}
	if svc.Lockouts != nil {
		t.Fatalf("Lockouts = %v, want nil (no lockout port on Logout/ValidateSession)", svc.Lockouts)
	}
}
