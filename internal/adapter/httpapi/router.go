// Package httpapi assembles the chi router that exposes the CRM HTTP
// surface. It is the seam between cmd/server (which owns wireup) and the
// per-route handlers; it never touches the database, iam internals, or
// the filesystem directly.
//
// Middleware order is load-bearing (SIN-62217 §Middleware chain):
//
//	RequestID → RealIP → Logger → Recoverer → TenantScope → Auth
//
// Each link layers on the next; reordering breaks security guarantees
// (e.g. Auth without TenantScope cannot validate per-tenant sessions).
//
// SIN-62218 layered observability into the chain *without* altering
// the canonical security order: HTTPMetrics + OTelHTTP run AFTER
// TenantScope (so spans carry tenant.id), and a span enricher runs
// AFTER Auth (so spans carry user.id). The /metrics scrape endpoint
// and /internal/test-alert (smoke-alert) are mounted outside the
// tenanted group — they never touch tenant resolution or auth.
//
// Routing layout:
//
//	GET  /health               — liveness (NO tenant scope, NO auth)
//	GET  /metrics              — Prometheus scrape (NO tenant, NO auth)
//	POST /internal/test-alert  — smoke-alert seam (build tag `test` only)
//	GET  /login                — render form          (tenant scope, no auth)
//	POST /login                — submit credentials    (tenant scope, no auth)
//	GET  /logout               — clear session cookie  (tenant scope, no auth)
//	GET  /hello-tenant         — protected page        (tenant scope + auth)
package httpapi

import (
	"context"
	"log/slog"
	"net"
	"net/http"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// IAMService is the union of iam.Service slices the handlers need. The
// concrete *iam.Service satisfies it; tests inject fakes implementing the
// same shape.
type IAMService interface {
	Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent string) (iam.Session, error)
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
	ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error)
}

// Deps is the constructor-injected dependency bag for NewRouter. cmd/server
// builds it once at bootstrap; tests build it with fakes.
type Deps struct {
	IAM            IAMService
	TenantResolver tenancy.Resolver
	Logger         *slog.Logger
	// Metrics, when non-nil, mounts /metrics + the per-request
	// counters/histograms (SIN-62218). Nil keeps the router behaving
	// exactly as it did pre-PR10 — useful for tests that don't want
	// to assert against Prometheus output.
	Metrics *obs.Metrics
	// CookieSecure flips the Secure attribute on the session cookie.
	// Production MUST set true. Dev/integration tests pass false so a
	// plaintext httptest.Server can read the cookie back.
	CookieSecure bool
}

// NewRouter wires the chi router with the canonical middleware chain and
// the SIN-62217 routes. Handlers and middleware are stitched here; the
// individual files in handler/ and middleware/ have no awareness of each
// other's existence beyond the small ports they export.
func NewRouter(deps Deps) http.Handler {
	if deps.IAM == nil {
		panic("httpapi: Deps.IAM is nil")
	}
	if deps.TenantResolver == nil {
		panic("httpapi: Deps.TenantResolver is nil")
	}
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}

	r := chi.NewRouter()

	// Cross-cutting middleware. Order is fixed by SIN-62217 §Middleware
	// chain — never reorder without an ADR.
	r.Use(chimw.RequestID)
	r.Use(chimw.RealIP)
	r.Use(propagateRequestIDToObs)
	r.Use(slogRequestLogger(deps.Logger))
	r.Use(chimw.Recoverer)

	// /health is the only route that bypasses the tenant scope. The LB
	// must reach it without resolving a tenant by host (the host might
	// be the raw load-balancer DNS name).
	r.Get("/health", handler.Health)

	// /metrics is whitelist-mounted: no tenant, no auth, no metrics
	// recursion. Access control belongs at the network edge (firewall
	// / Caddy ACL); this router does not try to authenticate scrapes.
	if deps.Metrics != nil {
		r.Method(http.MethodGet, "/metrics", deps.Metrics.Handler())
	}

	// /internal/test-alert is the smoke-alert seam. In production
	// builds the handler is a 404 (see internal/obs/testalert_prod.go);
	// only `-tags test` binaries reach the real implementation that
	// increments rls_misses_total. Mounted outside tenant scope so the
	// `make smoke-alert` curl can target app:8080 by IP without DNS.
	if deps.Metrics != nil {
		r.Method(http.MethodPost, "/internal/test-alert",
			obs.TestAlertHandler(deps.Metrics))
	}

	// All other routes go through TenantScope. Public-but-tenanted
	// routes (login, logout) live in this group; the authenticated
	// subset is nested below.
	r.Group(func(tenanted chi.Router) {
		tenanted.Use(middleware.TenantScope(deps.TenantResolver))
		tenanted.Use(propagateTenantIDToObs)
		if deps.Metrics != nil {
			tenanted.Use(deps.Metrics.HTTPMetrics(httpTenantOf, httpRouteOf))
		}
		tenanted.Use(obs.OTelHTTP("http.request", httpRouteOf, httpTenantSpanEnricher))

		tenanted.Get("/login", handler.LoginGet)
		tenanted.Post("/login", handler.LoginPost(handler.LoginConfig{
			IAM:          deps.IAM,
			CookieSecure: deps.CookieSecure,
		}))
		tenanted.Get("/logout", handler.Logout(deps.IAM))

		tenanted.Group(func(authed chi.Router) {
			authed.Use(middleware.Auth(deps.IAM))
			authed.Use(propagateUserIDToObsAndSpan)
			authed.Get("/hello-tenant", handler.HelloTenant)
		})
	})

	return r
}

// slogRequestLogger emits one structured log line per request. It is
// intentionally minimal: it logs status, method, path, and request id —
// nothing that depends on a body parse, since that would interfere with
// the body-form interop pattern documented on handler.LoginConfig.
func slogRequestLogger(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := chimw.NewWrapResponseWriter(w, r.ProtoMajor)
			next.ServeHTTP(ww, r)
			logger.LogAttrs(r.Context(), slog.LevelInfo, "http: request",
				slog.String("request_id", chimw.GetReqID(r.Context())),
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", ww.Status()),
			)
		})
	}
}

// propagateRequestIDToObs copies chimw.GetReqID into the obs context
// key so obs.FromContext-derived loggers and the JSON handler include
// request_id in every log record. Run once at the top of the chain.
func propagateRequestIDToObs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if rid := chimw.GetReqID(r.Context()); rid != "" {
			r = r.WithContext(obs.WithRequestID(r.Context(), rid))
		}
		next.ServeHTTP(w, r)
	})
}

// propagateTenantIDToObs copies the resolved tenant.ID into the obs
// context key. Runs immediately after TenantScope so every downstream
// log line carries tenant_id.
func propagateTenantIDToObs(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t, err := tenancy.FromContext(r.Context()); err == nil && t != nil {
			r = r.WithContext(obs.WithTenantID(r.Context(), t.ID.String()))
		}
		next.ServeHTTP(w, r)
	})
}

// propagateUserIDToObsAndSpan copies the validated session user_id
// into the obs context key (so slog records carry it) AND onto the
// active OTel span as the standard user.id attribute. Runs once,
// AFTER middleware.Auth, in the authed group.
func propagateUserIDToObsAndSpan(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, ok := middleware.SessionFromContext(r.Context()); ok {
			uid := s.UserID.String()
			r = r.WithContext(obs.WithUserID(r.Context(), uid))
			oteltrace.SpanFromContext(r.Context()).SetAttributes(
				attribute.String("user.id", uid),
			)
		}
		next.ServeHTTP(w, r)
	})
}

// httpRouteOf returns the chi RoutePattern when available, falling
// back to the URL path. Used as the route dimension on metrics and
// span attributes.
func httpRouteOf(r *http.Request) string {
	if rc := chi.RouteContext(r.Context()); rc != nil && rc.RoutePattern() != "" {
		return rc.RoutePattern()
	}
	return r.URL.Path
}

// httpTenantOf returns the resolved tenant id, or "" when the request
// hasn't gone through TenantScope. Used as the tenant dimension on
// metrics; "" maps to an explicit "unknown" series so dashboards
// never silently drop pre-tenant requests.
func httpTenantOf(r *http.Request) string {
	t, err := tenancy.FromContext(r.Context())
	if err != nil || t == nil {
		return ""
	}
	return t.ID.String()
}

// httpTenantSpanEnricher contributes the tenant.id attribute to OTel
// spans. Passed to obs.OTelHTTP at wire time so the obs package stays
// unaware of the tenancy types.
func httpTenantSpanEnricher(r *http.Request) []attribute.KeyValue {
	t, err := tenancy.FromContext(r.Context())
	if err != nil || t == nil {
		return nil
	}
	return []attribute.KeyValue{attribute.String("tenant.id", t.ID.String())}
}
