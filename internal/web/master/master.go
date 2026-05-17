// Package master is the HTMX UI surface for the master operator
// console "tenants" workflow (SIN-62882 / Fase 2.5 C9). It serves three
// endpoints:
//
//	GET   /master/tenants            — paginated tenant list + plan filter
//	POST  /master/tenants            — create tenant (+ optional plan + courtesy)
//	PATCH /master/tenants/{id}/plan  — assign / change a tenant's plan
//
// The package follows the same shape as internal/web/contacts and
// internal/web/privacy: html/template inlined into templates.go, only
// the small ports the handler actually needs declared on Deps, and the
// security envelope (RequireAuth → RequireMasterRoleAndMFA → the per-
// action RequireAction gate) layered on by the wire layer outside the
// handler. The handler itself trusts the iam.Principal already on the
// request context and limits itself to view / use-case orchestration.
//
// Per ADR 0073 the package emits a CSRF hidden input on every form and
// hx-headers on <body>, and per ADR 0090 the per-action RBAC decision
// is recorded by the AuditingAuthorizer wired into RequireAction — the
// handler does not write to audit_log_security directly. CA #2
// ("tenant gerente acessando /master/* → 403 + linha em audit_log") is
// satisfied by the RequireAction gate landing on the action constants
// added in SIN-62880 (master.tenant.read / .create / subscription.
// assign_plan), all of which are master-only in the ADR 0090 matrix.
package master

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/billing"
)

// TenantRow is the projection rendered on the list page and the
// re-rendered row partial after a successful create / assign-plan
// POST/PATCH. It deliberately carries only the fields the master
// console UI needs — the use-case adapter is responsible for joining
// the underlying tenant + subscription + invoice rows.
type TenantRow struct {
	ID                   uuid.UUID
	Name                 string
	Host                 string
	PlanSlug             string
	PlanName             string
	SubscriptionStatus   string
	LastInvoiceState     string
	LastInvoiceUpdatedAt time.Time
}

// ListOptions controls the GET /master/tenants query. Pagination is
// 1-indexed for the URL surface; the adapter is free to use 0-indexed
// internally. FilterPlanSlug = "" means "no filter".
type ListOptions struct {
	Page           int
	PageSize       int
	FilterPlanSlug string
}

// ListResult is the read-side return value. TotalCount is the unfiltered
// length of the dataset matching FilterPlanSlug — the template renders
// "página X de Y" from this plus PageSize.
type ListResult struct {
	Tenants    []TenantRow
	Page       int
	PageSize   int
	TotalCount int
}

// CreateTenantInput is the form body posted to POST /master/tenants.
// PlanSlug is optional ("" means "do not assign a plan now"); the same
// is true for InitialCourtesyTokens (0 means "do not bootstrap a wallet
// with a courtesy grant"). The actor's UserID is read from the request-
// scope iam.Principal and threaded through so the use case can audit
// the act of creation.
type CreateTenantInput struct {
	ActorUserID           uuid.UUID
	Name                  string
	Host                  string
	PlanSlug              string
	InitialCourtesyTokens int64
}

// CreateTenantResult is the row freshly produced by Create. The handler
// reuses the same renderer as the list page so the new row drops in
// without a template-shape mismatch.
type CreateTenantResult struct {
	Tenant TenantRow
}

// AssignPlanInput is the body of PATCH /master/tenants/{id}/plan. The
// caller supplies the path-derived tenant id and the form-derived plan
// slug; the use case is responsible for creating (or transitioning) the
// underlying billing.Subscription.
type AssignPlanInput struct {
	ActorUserID uuid.UUID
	TenantID    uuid.UUID
	PlanSlug    string
}

// AssignPlanResult mirrors CreateTenantResult.
type AssignPlanResult struct {
	Tenant TenantRow
}

// TenantLister is the read-side port. The adapter joins tenant +
// active subscription + most-recent invoice and applies the optional
// plan filter. The handler treats ErrNotFound from this port as an
// empty result, not an error.
type TenantLister interface {
	List(ctx context.Context, opts ListOptions) (ListResult, error)
}

// TenantCreator creates a new tenant, optionally bootstrapping the
// initial plan (a billing.Subscription) and a courtesy grant. The
// adapter MUST be idempotent on (host) and MUST surface
// ErrHostTaken when the host collides with an existing tenant.
type TenantCreator interface {
	Create(ctx context.Context, in CreateTenantInput) (CreateTenantResult, error)
}

// PlanLister wraps billing.PlanCatalog.ListPlans so the handler does
// not depend on the billing package's port directly — keeping the
// master package's outward surface small.
type PlanLister interface {
	List(ctx context.Context) ([]billing.Plan, error)
}

// PlanAssigner is the write-side port for the PATCH endpoint. The
// adapter creates a Subscription if none exists for the tenant or
// transitions the existing one. ErrNotFound here means the tenant id
// does not exist (handler maps to 404); ErrUnknownPlan means the slug
// is bad (handler maps to 422).
type PlanAssigner interface {
	Assign(ctx context.Context, in AssignPlanInput) (AssignPlanResult, error)
}

// Domain-level errors the ports can return. They keep the package self-
// contained — adapters wrap their internal errors with errors.Join /
// fmt.Errorf("%w", …) so the handler matches with errors.Is.
var (
	// ErrNotFound covers "tenant id does not exist" for the PATCH path
	// and "no rows" returned by the List port (treated as an empty
	// result, not surfaced to the client).
	ErrNotFound = errors.New("web/master: tenant not found")

	// ErrHostTaken signals POST /master/tenants encountered a unique-
	// constraint violation on (host). The handler renders the form with
	// a validation error rather than crashing on a 500.
	ErrHostTaken = errors.New("web/master: tenant host already in use")

	// ErrUnknownPlan means the plan slug submitted on POST/PATCH does
	// not match any row in the plan catalogue. Handler → 422.
	ErrUnknownPlan = errors.New("web/master: unknown plan slug")

	// ErrInvalidInput is the catch-all for form validation failures
	// (empty name, malformed host, negative courtesy tokens, etc).
	// Handler → 400 with an explanatory message.
	ErrInvalidInput = errors.New("web/master: invalid input")
)

// CSRFTokenFn returns the request's CSRF token. Empty token is a
// programmer error (RequireAuth + the CSRF middleware should have
// already populated the session); the handler 500s rather than emit a
// form without a CSRF guard.
type CSRFTokenFn func(*http.Request) string

// Deps bundles the handler collaborators. Logger defaults to slog.
// Default when nil; tenant-side fields are required; the grants port
// (Grants) is optional — nil disables the SIN-62884 grants surface so
// router tests and ad-hoc binaries that only need the tenants page do
// not have to plumb a wallet-backed adapter.
type Deps struct {
	Tenants   TenantLister
	Creator   TenantCreator
	Plans     PlanLister
	Assigner  PlanAssigner
	CSRFToken CSRFTokenFn
	Logger    *slog.Logger

	// Grants is the SIN-62884 grants surface (issue / revoke / list).
	// Optional: when nil, the three grants routes return 503 with an
	// explanatory message instead of panicking, so the rest of the
	// master console keeps working in deploys that haven't yet wired
	// the wallet adapter.
	Grants GrantPort

	// DefaultPageSize is applied when the request omits ?page_size or
	// passes a value outside (0, MaxPageSize]. Zero defaults to 25.
	DefaultPageSize int

	// MaxPageSize caps the page_size query param. Zero defaults to 100.
	MaxPageSize int
}

// GrantPort is the union of the three grants sub-ports used by the
// C10 handlers. cmd/server wires this with a single struct that
// embeds the wallet-backed adapter; tests can pass a hand-rolled
// stub.
type GrantPort interface {
	GrantIssuer
	GrantRevoker
	GrantLister
}

// Handler is the master/tenants HTMX handler. Mount with Routes for
// in-package tests, or attach the three exported HandlerFunc methods
// individually behind their RequireAction gate at the wire layer.
type Handler struct {
	deps            Deps
	defaultPageSize int
	maxPageSize     int
}

// New constructs a Handler. Nil required dependencies are rejected at
// boot so a misconfigured wire fails fast.
func New(deps Deps) (*Handler, error) {
	if deps.Tenants == nil {
		return nil, errors.New("web/master: Tenants is required")
	}
	if deps.Creator == nil {
		return nil, errors.New("web/master: Creator is required")
	}
	if deps.Plans == nil {
		return nil, errors.New("web/master: Plans is required")
	}
	if deps.Assigner == nil {
		return nil, errors.New("web/master: Assigner is required")
	}
	if deps.CSRFToken == nil {
		return nil, errors.New("web/master: CSRFToken is required")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	defaultPageSize := deps.DefaultPageSize
	if defaultPageSize <= 0 {
		defaultPageSize = 25
	}
	maxPageSize := deps.MaxPageSize
	if maxPageSize <= 0 {
		maxPageSize = 100
	}
	if defaultPageSize > maxPageSize {
		return nil, errors.New("web/master: DefaultPageSize exceeds MaxPageSize")
	}
	return &Handler{
		deps:            deps,
		defaultPageSize: defaultPageSize,
		maxPageSize:     maxPageSize,
	}, nil
}

// Routes mounts the tenants + grants endpoints on a stdlib mux.
// Production wires each handler individually so each can sit behind
// its own RequireAction gate; this method is the convenience path
// for in-package tests and ad-hoc local exploration.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /master/tenants", h.ListTenants)
	mux.HandleFunc("POST /master/tenants", h.CreateTenant)
	mux.HandleFunc("PATCH /master/tenants/{id}/plan", h.AssignPlan)
	// SIN-62884 C10 — grants surface. Conditionally mount; with
	// Grants nil the routes return 503 so the rest of the console
	// keeps working in early deploys.
	mux.HandleFunc("GET /master/tenants/{id}/grants/new", h.ShowGrantsForm)
	mux.HandleFunc("POST /master/tenants/{id}/grants", h.IssueGrant)
	mux.HandleFunc("POST /master/grants/{id}/revoke", h.RevokeGrant)
}
