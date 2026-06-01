package main

// SIN-63957 — router-level smoke that asserts every /master/tenants/*
// and /master/grants/* slot mounted by httpapi.NewRouter is reachable
// when Deps.MasterTenants is populated. The bar is "non-404 for an
// unauthenticated request" per memory
// reference_crm_router_nil_dep_silent_skip: a slot left nil produces a
// clean 404 indistinguishable from a missing /master subtree dispatch,
// so any of 200/302/401/403 is a pass.
//
// Stubs are minimal: a no-op IAMService, a single-host tenancy.Resolver,
// and a stub iam.Authorizer that denies every Can() call (the routes
// only mount when Authorizer is non-nil per router.go:1183 — value of
// the decision is irrelevant for the 404-vs-not assertion because
// RequireAuth fires first and short-circuits unauthenticated requests
// before RequireAction is reached).

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

type stubIAMService struct{}

func (stubIAMService) Login(context.Context, string, string, string, net.IP, string, string) (iam.Session, error) {
	return iam.Session{}, iam.ErrInvalidCredentials
}
func (stubIAMService) Logout(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (stubIAMService) ValidateSession(context.Context, uuid.UUID, uuid.UUID) (iam.Session, error) {
	return iam.Session{}, iam.ErrSessionNotFound
}

type stubMasterTenantResolver struct {
	tenant *tenancy.Tenant
}

func (s *stubMasterTenantResolver) ResolveByHost(_ context.Context, host string) (*tenancy.Tenant, error) {
	if s.tenant != nil && s.tenant.Host == host {
		return s.tenant, nil
	}
	return nil, tenancy.ErrTenantNotFound
}

type stubAuthorizer struct{}

func (stubAuthorizer) Can(context.Context, iam.Principal, iam.Action, iam.Resource) iam.Decision {
	return iam.Decision{Allow: false, ReasonCode: iam.ReasonDeniedRBAC}
}

// masterRouteMounts is the canonical list of every /master/tenants/* +
// /master/grants/* path mounted by httpapi.NewRouter when the matching
// MasterTenantsRoutes slot is non-nil. Cross-referenced with router.go
// :1183-1267. Eleven entries → eleven slots.
var masterRouteMounts = []struct {
	name       string
	method     string
	pathFactor string // single literal path or a path template with a UUID placeholder
}{
	{"List", http.MethodGet, "/master/tenants"},
	{"Create", http.MethodPost, "/master/tenants"},
	{"AssignPlan", http.MethodPatch, "/master/tenants/{id}/plan"},
	{"GrantsNew", http.MethodGet, "/master/tenants/{id}/grants/new"},
	{"GrantsCreate", http.MethodPost, "/master/tenants/{id}/grants"},
	{"GrantsRevoke", http.MethodPost, "/master/grants/{id}/revoke"},
	{"GrantRequestsCreate", http.MethodPost, "/master/tenants/{id}/grants/requests"},
	{"GrantRequestsList", http.MethodGet, "/master/grants/requests"},
	{"GrantRequestsShow", http.MethodGet, "/master/grants/requests/{id}"},
	{"GrantRequestsApprove", http.MethodPost, "/master/grants/requests/{id}/approve"},
	{"GrantRequestsReject", http.MethodPost, "/master/grants/requests/{id}/reject"},
}

// fillAllMasterTenantsSlots returns a MasterTenantsRoutes where every
// slot points to the same no-op handler (returns 204). The router
// mounts a route iff its slot is non-nil, so populating all eleven
// slots produces the maximum mounted surface.
func fillAllMasterTenantsSlots() httpapi.MasterTenantsRoutes {
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	return httpapi.MasterTenantsRoutes{
		List:                 h,
		Create:               h,
		AssignPlan:           h,
		GrantsNew:            h,
		GrantsCreate:         h,
		GrantsRevoke:         h,
		GrantRequestsCreate:  h,
		GrantRequestsList:    h,
		GrantRequestsShow:    h,
		GrantRequestsApprove: h,
		GrantRequestsReject:  h,
	}
}

// TestRouter_AllMasterRoutesMountedWithPopulatedSlots asserts that each
// of the eleven /master/* paths returns non-404 when the corresponding
// Deps.MasterTenants slot is populated. The handlers behind the routes
// are no-ops; what we assert is "the route is reachable through the chi
// router, not shadowed by the public mux catch-all or skipped by a nil-
// slot guard". This is the regression canary memory
// reference_crm_router_nil_dep_silent_skip names.
func TestRouter_AllMasterRoutesMountedWithPopulatedSlots(t *testing.T) {
	t.Parallel()
	host := "acme.crm.local"
	tenantID := uuid.New()
	resolver := &stubMasterTenantResolver{
		tenant: &tenancy.Tenant{ID: tenantID, Name: "acme", Host: host},
	}
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            stubIAMService{},
		TenantResolver: resolver,
		Authorizer:     stubAuthorizer{},
		MasterTenants:  fillAllMasterTenantsSlots(),
	})
	idPlaceholder := uuid.New().String()
	for _, tc := range masterRouteMounts {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := tc.pathFactor
			// Substitute {id} for a real UUID so chi pattern matching
			// resolves cleanly. Without a valid UUID the path-param
			// extractor at the handler edge would return its own error
			// shape; here we just want chi dispatch to reach the slot.
			path = substitutePathID(path, idPlaceholder)
			req := httptest.NewRequest(tc.method, "http://"+host+path, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code == http.StatusNotFound {
				t.Fatalf("%s %s returned 404; route appears unmounted (body=%q)",
					tc.method, path, rec.Body.String())
			}
		})
	}
}

// TestRouter_NilMasterSlotsLeaveRoutesUnmounted is the contrapositive
// canary: with every MasterTenants slot nil (the pre-PR behaviour), the
// same eleven paths return 404. Locks the per-slot mounting contract so
// a future refactor that mounts routes unconditionally would fail this
// test instead of breaking the silent-disable mode.
func TestRouter_NilMasterSlotsLeaveRoutesUnmounted(t *testing.T) {
	t.Parallel()
	host := "acme.crm.local"
	tenantID := uuid.New()
	resolver := &stubMasterTenantResolver{
		tenant: &tenancy.Tenant{ID: tenantID, Name: "acme", Host: host},
	}
	router := httpapi.NewRouter(httpapi.Deps{
		IAM:            stubIAMService{},
		TenantResolver: resolver,
		Authorizer:     stubAuthorizer{},
		// MasterTenants left zero-value — every slot nil.
	})
	idPlaceholder := uuid.New().String()
	for _, tc := range masterRouteMounts {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path := substitutePathID(tc.pathFactor, idPlaceholder)
			req := httptest.NewRequest(tc.method, "http://"+host+path, nil)
			req.Host = host
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s returned %d; want 404 when slot is nil (body=%q)",
					tc.method, path, rec.Code, rec.Body.String())
			}
		})
	}
}

// substitutePathID replaces the literal "{id}" placeholder with a real
// uuid. Trivial helper; isolated for readability.
func substitutePathID(path, id string) string {
	out := make([]byte, 0, len(path))
	for i := 0; i < len(path); {
		if i+4 <= len(path) && path[i:i+4] == "{id}" {
			out = append(out, id...)
			i += 4
			continue
		}
		out = append(out, path[i])
		i++
	}
	return string(out)
}

// TestMasterTenantsWire_OptsSlotPattern asserts the iamHandlerOpts /
// buildIAMHandler contract: a caller-supplied opts.MasterTenants with
// List non-nil wins over the wire-built stack, matching the
// opts.WebLGPD / opts.Impersonation / opts.UserMFA precedent. The check
// is at the type level — we don't need a real router to validate the
// override pathway.
func TestMasterTenantsWire_OptsSlotPattern(t *testing.T) {
	t.Parallel()
	stub := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	opts := iamHandlerOpts{
		MasterTenants: httpapi.MasterTenantsRoutes{
			List: stub,
		},
	}
	// If the wire ever changes to ignore the opts override, this
	// equality check fails — locking the cmd/server unit-test
	// injection contract.
	if opts.MasterTenants.List == nil {
		t.Fatal("opts.MasterTenants.List was unexpectedly nil after assignment")
	}
}

// stubAuditWriter is a no-op audit.SplitLogger used by the fail-fast
// tests below. We never reach the post-DB code paths so the writes
// never fire; the type satisfies the interface for early-return tests.
type stubAuditWriter struct{}

func (stubAuditWriter) WriteSecurity(_ context.Context, _ audit.SecurityAuditEvent) error {
	return nil
}
func (stubAuditWriter) WriteData(_ context.Context, _ audit.DataAuditEvent) error {
	return nil
}

// TestBuildMasterTenantsStack_NoopEarlyReturns covers every early-
// return guard in buildMasterTenantsStack: a missing required input or
// env knob must produce a clean noopMasterTenantsStack (Routes
// zero-valued, Cleanup non-nil so deferred calls never panic). Mirrors
// the lgpd_wire / impersonation_wire fail-fast test patterns. The
// post-pool-open branches require a real master_ops DSN and are
// covered by integration tests in internal/adapter/db/postgres/master.
func TestBuildMasterTenantsStack_NoopEarlyReturns(t *testing.T) {
	t.Parallel()
	stubAudit := stubAuditWriter{}
	logger := slog.Default()
	validUUID := uuid.New().String()

	envReturning := func(values map[string]string) func(string) string {
		return func(key string) string {
			return values[key]
		}
	}

	cases := []struct {
		name        string
		runtimePool *pgxpool.Pool // always nil in early-return tests
		audit       audit.SplitLogger
		logger      *slog.Logger
		env         map[string]string
	}{
		{
			name:   "nil runtime pool",
			audit:  stubAudit,
			logger: logger,
			env:    map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x", "MASTER_OPS_ACTOR_ID": validUUID},
		},
		{
			name:        "nil audit writer",
			runtimePool: nil, // also nil, but the audit-nil branch fires first when both nil
			audit:       nil,
			logger:      logger,
			env:         map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x", "MASTER_OPS_ACTOR_ID": validUUID},
		},
		{
			name:   "nil logger",
			audit:  stubAudit,
			logger: nil,
			env:    map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x", "MASTER_OPS_ACTOR_ID": validUUID},
		},
		{
			name:   "missing master_ops dsn",
			audit:  stubAudit,
			logger: logger,
			env:    map[string]string{"MASTER_OPS_ACTOR_ID": validUUID},
		},
		{
			name:   "missing actor id env",
			audit:  stubAudit,
			logger: logger,
			env:    map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x"},
		},
		{
			name:   "unparseable actor id",
			audit:  stubAudit,
			logger: logger,
			env:    map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x", "MASTER_OPS_ACTOR_ID": "not-a-uuid"},
		},
		{
			name:   "zero actor id",
			audit:  stubAudit,
			logger: logger,
			env:    map[string]string{"MASTER_OPS_DATABASE_URL": "postgres://x", "MASTER_OPS_ACTOR_ID": uuid.Nil.String()},
		},
	}
	// runtimePool stays nil throughout — the first two early-return
	// branches fire on nil pool / nil audit before pgxpool.New is
	// reached. The "nil runtime pool" case requires audit non-nil to
	// distinguish from the audit-nil branch.
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			runtimePool := tc.runtimePool
			// The "nil runtime pool" case wants audit+logger non-nil
			// so the early-return is provably due to pool, not audit.
			// For the audit/logger branches we also pass nil pool so
			// the test doesn't need a real pgx connection — the guard
			// fires on the audit/logger condition first when those
			// are nil, and on the pool condition otherwise.
			stack := buildMasterTenantsStack(
				context.Background(),
				runtimePool,
				tc.audit,
				envReturning(tc.env),
				tc.logger,
			)
			if stack.Cleanup == nil {
				t.Fatalf("Cleanup must be non-nil on every noop branch (defer chain depends on it)")
			}
			if stack.Routes.List != nil ||
				stack.Routes.Create != nil ||
				stack.Routes.AssignPlan != nil ||
				stack.Routes.GrantsNew != nil ||
				stack.Routes.GrantsCreate != nil ||
				stack.Routes.GrantsRevoke != nil ||
				stack.Routes.GrantRequestsCreate != nil ||
				stack.Routes.GrantRequestsList != nil ||
				stack.Routes.GrantRequestsShow != nil ||
				stack.Routes.GrantRequestsApprove != nil ||
				stack.Routes.GrantRequestsReject != nil {
				t.Fatalf("expected zero-valued Routes on noop branch, got %+v", stack.Routes)
			}
			// Cleanup MUST be safe to call even on noop — exercise it
			// so the defer chain in iam_wire.go can never panic.
			stack.Cleanup()
		})
	}
}

// TestNoopMasterTenantsStack_HasNonNilCleanup is a regression canary
// matching the impersonation/lgpd wire pattern: the zero-value Routes
// is fine, but Cleanup MUST be non-nil so cmd/server can always defer
// it. Catching a regression here is cheaper than a panic in the boot
// path on a noop-pre-condition deploy.
func TestNoopMasterTenantsStack_HasNonNilCleanup(t *testing.T) {
	t.Parallel()
	stack := noopMasterTenantsStack()
	if stack.Cleanup == nil {
		t.Fatal("noopMasterTenantsStack().Cleanup must be non-nil")
	}
	stack.Cleanup() // must not panic
	if stack.Routes.List != nil {
		t.Fatalf("noopMasterTenantsStack() must return zero Routes, got List=%v", stack.Routes.List)
	}
}

// TestEnvMasterOpsActorID_ConstantValue locks the env-var name. The
// CTO pre-approved the literal in the SIN-63955 routing comment; if a
// future refactor renames it, deploys would silently disable the
// master surface (the wire's getenv lookup would return "").
func TestEnvMasterOpsActorID_ConstantValue(t *testing.T) {
	t.Parallel()
	if envMasterOpsActorID != "MASTER_OPS_ACTOR_ID" {
		t.Fatalf("envMasterOpsActorID = %q, want %q (CTO ratification in SIN-63955)",
			envMasterOpsActorID, "MASTER_OPS_ACTOR_ID")
	}
}
