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
//	POST /logout               — clear session cookie  (tenant scope + auth + CSRF)
//	GET  /hello-tenant         — protected page        (tenant scope + auth + RequireAuth + RequireAction[tenant.contact.read])
//
// SIN-62767 mounts the first production RequireAction gate on
// /hello-tenant. The action is iam.ActionTenantContactRead — the
// most-permissive tenant-scope read in the ADR 0090 matrix, so the
// three tenant roles (common/atendente/gerente) keep their
// pre-SIN-62767 access while empty-role / cross-role probes (the
// horizontal-probing pattern F10 is meant to surface) now produce a
// 403 + an audit_log_security row + an authz_user_deny_total
// increment via Deps.Authorizer (the SIN-62765 AuditingAuthorizer).
// The gate is conditional on Deps.Authorizer != nil so router tests
// that don't wire one keep behaving exactly as they did pre-PR; nil
// in production is impossible because cmd/server fails boot if the
// audit wrap cannot be constructed.
//
//	GET|POST /m/login               — master login form / submit (no auth)
//	GET      /m/logout              — master logout (no auth)
//	GET|POST /m/2fa/enroll          — enroll TOTP (RequireMasterAuth + RequireMasterMFA)
//	GET|POST /m/2fa/verify          — verify TOTP code (RequireMasterAuth only)
//	POST     /m/2fa/recovery/regenerate — regenerate codes (RequireMasterAuth + RequireMasterMFA)
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimw "github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	oteltrace "go.opentelemetry.io/otel/trace"

	csrfmw "github.com/pericles-luz/crm/internal/adapter/httpapi/csrf"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/handler"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/mastermfa"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	"github.com/pericles-luz/crm/internal/iam"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
)

// IAMService is the union of iam.Service slices the handlers need. The
// concrete *iam.Service satisfies it; tests inject fakes implementing the
// same shape.
type IAMService interface {
	Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error)
	Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error
	ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error)
}

// MasterDeps bundles the master-console handler and middleware
// instances the /m/* route group needs. Nil MasterDeps skips the
// entire /m/* group — existing tests that don't need master routes
// leave it at the zero value.
//
// All handler slots accept any http.Handler so tests can pass simple
// nop handlers without constructing full mastermfa.* structs. cmd/server
// passes the concrete *mastermfa.LoginHandler etc. which all implement
// http.Handler.
type MasterDeps struct {
	Login      http.Handler
	Logout     http.Handler
	Enroll     http.Handler
	Verify     http.Handler
	Regenerate http.Handler

	// RequireMasterAuth gates every /m/* route except /m/login and
	// /m/logout on a valid master session cookie.
	RequireMasterAuth func(http.Handler) http.Handler

	// RequireMasterMFA gates all authed /m/* routes except
	// /m/2fa/verify on an enrolled + at-least-once-verified TOTP.
	RequireMasterMFA func(http.Handler) http.Handler
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
	// Authorizer is the production iam.Authorizer routes use through
	// middleware.RequireAction. SIN-62765 wires this to the
	// authz.AuditingAuthorizer so every recorded Decision flows into
	// audit_log_security and Prometheus. The router itself does not
	// consult it — it is exposed here so per-handler RequireAction
	// wireup picks up the audited instance instead of a bare
	// *RBACAuthorizer. Nil is permitted for tests that don't gate any
	// route on RequireAction.
	Authorizer iam.Authorizer
	// Master, when non-zero, mounts the /m/* master-console routes.
	// Zero value skips the group.
	Master MasterDeps

	// Policies is the precomputed policy table from
	// iam/ratelimit.DefaultPolicies(). When set together with
	// RateLimiter, NewRouter mounts the per-route rate-limit
	// middleware on POST /login (SIN-62376 / FAIL-1 of SIN-62343).
	// Either field nil → no HTTP-boundary rate limiting (the
	// downstream lockout in iam.Service.Login still applies).
	Policies map[string]domainratelimit.Policy
	// RateLimiter is the shared limiter implementation
	// (typically *redis.SlidingWindow). Pairs with Policies — see
	// the Policies docstring for the activation contract.
	RateLimiter domainratelimit.RateLimiter
	// RateLimitDenyMetric, when non-nil, is invoked on every 429
	// emitted by the rate-limit middleware. cmd/server wires this
	// to the auth_ratelimit_deny_total Prometheus counter; tests can
	// pass a recording closure or leave it nil.
	RateLimitDenyMetric func(policy, bucket, key string, retryAfter time.Duration)

	// SessionToucher, when non-nil, enables the SIN-62377 / FAIL-4
	// activity middleware on the authenticated tenant group. Mounted
	// AFTER middleware.Auth so the activity gate sees the iam.Session
	// already loaded into the request context. Nil keeps the router
	// behaving exactly as it did pre-SIN-62377 — used by router tests
	// that don't wire a Touch port.
	SessionToucher middleware.SessionToucher

	// MasterHost is the operator-console hostname (e.g. "master.crm.local")
	// added to the CSRF Origin/Referer allowlist alongside the resolved
	// tenant host (ADR 0073 §D1). Empty means "no master host configured"
	// — the allowlist falls back to the tenant host alone, which is the
	// minimum-viable safe value.
	MasterHost string
	// CSRFRejectMetric, when non-nil, is invoked on every 403 emitted
	// by the RequireCSRF middleware. cmd/server wires this to a
	// Prometheus counter; tests can record the reasons directly.
	CSRFRejectMetric func(*http.Request, csrfmw.Reason)

	// WebContacts is the HTMX identity-split UI handler from
	// internal/web/contacts (SIN-62799 / Fase 2 F2-13). When non-nil,
	// the routes GET /contacts/{contactID} and
	// POST /contacts/identity/split are mounted in the authed group so
	// they inherit TenantScope + Auth + CSRF + RequireAuth. cmd/server
	// builds this via the SIN-62855 htmx wire and leaves it nil when
	// DATABASE_URL is unset (consistent with the IAM/internal handlers).
	WebContacts http.Handler

	// WebFunnel is the HTMX drag-and-drop funnel board handler from
	// internal/web/funnel (SIN-62797 / Fase 2 F2-12). When non-nil, the
	// four routes are mounted in the authed group so they inherit
	// TenantScope + Auth + CSRF + RequireAuth (same security envelope as
	// WebContacts). cmd/server builds this via the SIN-62862 funnel wire
	// and leaves it nil when DATABASE_URL is unset.
	//
	// Routes mounted:
	//   GET  /funnel
	//   POST /funnel/transitions
	//   GET  /funnel/conversations/{id}/history
	//   GET  /funnel/modal/close
	WebFunnel http.Handler

	// WebPrivacy is the HTMX privacy / DPA disclosure handler from
	// internal/web/privacy (SIN-62354 / Fase 3, decisão #8). When
	// non-nil, the two GET-only routes are mounted in the authed group
	// so they inherit TenantScope + Auth + RequireAuth. The page is
	// read-only (no POST surface), so the CSRF middleware short-circuits
	// on GET — the page never reaches the CSRF rejection path.
	//
	// Routes mounted:
	//   GET /settings/privacy
	//   GET /settings/privacy/dpa.md
	WebPrivacy http.Handler

	// WebAIPolicy is the HTMX admin UI handler for AI policy
	// configuration from internal/web/aipolicy (SIN-62906 / Fase 3
	// W4A). Mounted in the authed group with the same envelope as
	// WebContacts/WebFunnel plus an extra
	// RequireAction(iam.ActionTenantAIPolicyWrite) gate that applies
	// to every method (read and write — the admin who can mutate is
	// the only one who needs to see the page).
	//
	// Routes mounted:
	//   GET    /settings/ai-policy
	//   GET    /settings/ai-policy/new
	//   GET    /settings/ai-policy/preview
	//   GET    /settings/ai-policy/{scope_type}/{scope_id}/edit
	//   POST   /settings/ai-policy
	//   PATCH  /settings/ai-policy/{scope_type}/{scope_id}
	//   DELETE /settings/ai-policy/{scope_type}/{scope_id}
	WebAIPolicy http.Handler
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

		loginPost := http.Handler(handler.LoginPost(handler.LoginConfig{
			IAM: deps.IAM,
		}))
		if mw := buildLoginRateLimit(deps); mw != nil {
			loginPost = mw(loginPost)
		}
		tenanted.Method(http.MethodPost, "/login", loginPost)

		tenanted.Group(func(authed chi.Router) {
			authed.Use(middleware.Auth(deps.IAM))
			// SIN-62377 (FAIL-4) activity middleware. Mounted AFTER
			// middleware.Auth so it can read the validated iam.Session
			// from context. Skipped when SessionToucher is nil so
			// router tests that don't wire it keep their pre-PR
			// behaviour.
			if deps.SessionToucher != nil {
				authed.Use(middleware.Activity(middleware.ActivityConfig{
					Sessions: deps.SessionToucher,
					Logger:   deps.Logger,
				}))
			}
			authed.Use(propagateUserIDToObsAndSpan)
			// RequireCSRF gates every state-changing route in the
			// authenticated tenant chain (ADR 0073 §D1). The
			// SessionToken closure reads the session injected by
			// middleware.Auth above; AllowedHosts builds the
			// Origin/Referer allowlist from the resolved tenant plus
			// the (optional) master host. GET/HEAD/OPTIONS short-circuit
			// safely so /hello-tenant and other reads are unaffected.
			authed.Use(csrfmw.New(csrfmw.Config{
				SessionToken: csrfSessionTokenFromContext,
				AllowedHosts: csrfAllowedHosts(deps.MasterHost),
				OnReject:     deps.CSRFRejectMetric,
			}))
			// SIN-62767 — gate the first protected production route on
			// RequireAction(audited, ActionTenantContactRead). When
			// Deps.Authorizer is nil (router tests that don't exercise
			// the authz seam), the route mounts unchanged so existing
			// suites keep their pre-PR behaviour. The header docstring
			// covers the rationale (action choice, gating condition).
			helloTenant := http.Handler(http.HandlerFunc(handler.HelloTenant))
			if deps.Authorizer != nil {
				helloTenant = middleware.RequireAuth(middleware.RequireAuthDeps{})(
					middleware.RequireAction(deps.Authorizer, iam.ActionTenantContactRead, nil)(helloTenant),
				)
			}
			authed.Method(http.MethodGet, "/hello-tenant", helloTenant)
			authed.Method(http.MethodPost, "/logout", handler.Logout(deps.IAM))

			// SIN-62855 — HTMX identity-split UI (SIN-62799 follow-up).
			// Mount inside RequireAuth so the inner handler runs with an
			// iam.Principal in context (matches the security envelope of
			// /hello-tenant). The handler is the stdlib *http.ServeMux
			// returned by web/contacts.Handler.Routes; chi does the
			// outer route match, the inner mux re-matches via Go 1.22
			// patterns and sets r.PathValue("contactID").
			if deps.WebContacts != nil {
				webContacts := middleware.RequireAuth(middleware.RequireAuthDeps{})(deps.WebContacts)
				authed.Method(http.MethodGet, "/contacts/{contactID}", webContacts)
				authed.Method(http.MethodPost, "/contacts/identity/split", webContacts)
			}

			// SIN-62862 — HTMX funnel board UI (SIN-62797 follow-up).
			// Same envelope as WebContacts: the chi authed group already
			// stitches TenantScope + Auth + CSRF; RequireAuth installs
			// iam.Principal in context before the inner handler runs. The
			// inner http.Handler is a stdlib *http.ServeMux produced by
			// web/funnel.Handler.Routes — Go 1.22 method+pattern syntax
			// re-matches each route inside the mux so r.PathValue("id")
			// resolves for the history modal.
			if deps.WebFunnel != nil {
				webFunnel := middleware.RequireAuth(middleware.RequireAuthDeps{})(deps.WebFunnel)
				authed.Method(http.MethodGet, "/funnel", webFunnel)
				authed.Method(http.MethodPost, "/funnel/transitions", webFunnel)
				authed.Method(http.MethodGet, "/funnel/conversations/{id}/history", webFunnel)
				authed.Method(http.MethodGet, "/funnel/modal/close", webFunnel)
			}

			// SIN-62354 — HTMX privacy / DPA disclosure (Fase 3, decisão #8).
			// Same envelope as WebContacts / WebFunnel. The two routes are
			// GET-only so the CSRF middleware short-circuits naturally.
			// AC #1: any authenticated tenant user reaches the page — no
			// extra RequireAction check beyond the inherited RequireAuth.
			if deps.WebPrivacy != nil {
				webPrivacy := middleware.RequireAuth(middleware.RequireAuthDeps{})(deps.WebPrivacy)
				authed.Method(http.MethodGet, "/settings/privacy", webPrivacy)
				authed.Method(http.MethodGet, "/settings/privacy/dpa.md", webPrivacy)
			}

			// SIN-62906 — HTMX AI policy admin UI (Fase 3 W4A). Same
			// envelope as the other web/* handlers plus an explicit
			// RequireAction gate on every method (the admin who can
			// mutate the configuration is the only one who needs to
			// see it). The gate is mounted only when Authorizer is
			// wired; router tests that don't exercise authz keep their
			// pre-PR behaviour.
			if deps.WebAIPolicy != nil {
				// RequireAuth runs OUTSIDE RequireAction so the
				// principal is in context when the authz gate
				// consults it. Mirrors the /hello-tenant wireup
				// above. When Authorizer is nil (router tests),
				// the route mounts with RequireAuth only — the
				// gate skips and the handler still sees a
				// Principal.
				webAIPolicy := http.Handler(deps.WebAIPolicy)
				if deps.Authorizer != nil {
					webAIPolicy = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantAIPolicyWrite, nil)(webAIPolicy),
					)
				} else {
					webAIPolicy = middleware.RequireAuth(middleware.RequireAuthDeps{})(webAIPolicy)
				}
				authed.Method(http.MethodGet, "/settings/ai-policy", webAIPolicy)
				authed.Method(http.MethodGet, "/settings/ai-policy/new", webAIPolicy)
				authed.Method(http.MethodGet, "/settings/ai-policy/preview", webAIPolicy)
				authed.Method(http.MethodGet, "/settings/ai-policy/{scope_type}/{scope_id}/edit", webAIPolicy)
				authed.Method(http.MethodPost, "/settings/ai-policy", webAIPolicy)
				authed.Method(http.MethodPatch, "/settings/ai-policy/{scope_type}/{scope_id}", webAIPolicy)
				authed.Method(http.MethodDelete, "/settings/ai-policy/{scope_type}/{scope_id}", webAIPolicy)
			}
		})
	})

	// /m/* master-console routes. Skipped when MasterDeps is zero so
	// existing tests and health-only mode are unaffected.
	if deps.Master.Login != nil {
		r.Route("/m", func(m chi.Router) {
			// Bootstrap routes — no session required.
			m.Method(http.MethodGet, "/login", deps.Master.Login)
			m.Method(http.MethodPost, "/login", deps.Master.Login)
			m.Method(http.MethodGet, "/logout", deps.Master.Logout)

			// All remaining routes require a valid master session.
			m.Group(func(authed chi.Router) {
				authed.Use(deps.Master.RequireMasterAuth)

				// /m/2fa/verify is reachable without MFA (that is the
				// point — the user is submitting the code to gain MFA).
				authed.Method(http.MethodGet, "/2fa/verify", deps.Master.Verify)
				postVerify := http.Handler(deps.Master.Verify)
				if mw := buildMaster2FAVerifyRateLimit(deps); mw != nil {
					postVerify = mw(postVerify)
				}
				authed.Method(http.MethodPost, "/2fa/verify", postVerify)

				// Everything else needs a completed MFA pass in this
				// session. RequireMasterMFA redirects to /m/2fa/verify when
				// the bit is not set.
				authed.Group(func(mfa chi.Router) {
					mfa.Use(deps.Master.RequireMasterMFA)

					mfa.Method(http.MethodGet, "/2fa/enroll", deps.Master.Enroll)
					mfa.Method(http.MethodPost, "/2fa/enroll", deps.Master.Enroll)
					mfa.Method(http.MethodPost, "/2fa/recovery/regenerate", deps.Master.Regenerate)
				})
			})
		})
	}

	return r
}

// buildMaster2FAVerifyRateLimit assembles the SIN-62380 middleware
// that fronts POST /m/2fa/verify with the m_2fa_verify policy
// (3/min/session, 10/h/user — ADR 0074 §6). It returns nil when the
// policy table or the limiter is missing OR when the policies map
// does not contain the m_2fa_verify entry — older test wireups (e.g.
// router_master_mfa_test) leave the master verify route un-throttled
// at the HTTP boundary and rely on the failure counter alone, which
// matches the iam.Service-only path used elsewhere.
//
// A non-nil Policies map containing "m_2fa_verify" but missing one
// of the declared buckets ("session" / "user") panics: the policy
// declared the bucket, so a missing extractor is a wireup bug, not
// an opt-out.
func buildMaster2FAVerifyRateLimit(deps Deps) func(http.Handler) http.Handler {
	if deps.Policies == nil || deps.RateLimiter == nil {
		return nil
	}
	policy, ok := deps.Policies["m_2fa_verify"]
	if !ok {
		return nil
	}
	mw, err := httpratelimit.New(httpratelimit.Config{
		Policy:  policy,
		Limiter: deps.RateLimiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "session", Extractor: mastermfa.SessionIDExtractor},
			{Name: "user", Extractor: mastermfa.MasterUserIDExtractor},
		},
		OnDeny: deps.RateLimitDenyMetric,
		Logger: deps.Logger,
	})
	if err != nil {
		panic(fmt.Errorf("httpapi: build m_2fa_verify rate-limit: %w", err))
	}
	return mw
}

// buildLoginRateLimit assembles the SIN-62376 middleware that fronts
// POST /login with the per-IP / per-email policy. It returns nil when
// either the policy table or the limiter is missing (existing tests
// that don't wire ratelimit MUST keep behaving as before — the
// downstream iam.Service.Login lockout still bounds abuse, and the
// HTTP layer just skips the pre-check). A non-nil Policies map that
// happens to lack the "login" key is treated as a programmer error
// and panics: cmd/server is the only legitimate constructor and it
// uses domainratelimit.DefaultPolicies which always emits "login".
func buildLoginRateLimit(deps Deps) func(http.Handler) http.Handler {
	if deps.Policies == nil || deps.RateLimiter == nil {
		return nil
	}
	policy, ok := deps.Policies["login"]
	if !ok {
		panic(`httpapi: Deps.Policies missing "login" entry`)
	}
	mw, err := httpratelimit.New(httpratelimit.Config{
		Policy:  policy,
		Limiter: deps.RateLimiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "ip", Extractor: httpratelimit.IPKeyExtractor},
			{Name: "email", Extractor: httpratelimit.FormFieldExtractor("email")},
		},
		OnDeny: deps.RateLimitDenyMetric,
		Logger: deps.Logger,
	})
	if err != nil {
		panic(fmt.Errorf("httpapi: build login rate-limit: %w", err))
	}
	return mw
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

// errCSRFNoSessionInContext signals that the RequireCSRF middleware
// was reached without a session in context. The CSRF middleware sits
// behind middleware.Auth, so this indicates a wiring bug rather than
// an unauthenticated request — the middleware surfaces it as
// csrf.session_lookup_error and 403, which is what we want.
var errCSRFNoSessionInContext = errors.New("httpapi: csrf middleware reached without session in context")

// csrfSessionTokenFromContext is the SessionToken closure passed to
// csrfmw.New. It reads the session injected by middleware.Auth and
// returns its CSRFToken. A missing session is a programmer error
// surfaced as a 403 with reason csrf.session_lookup_error so the
// middleware fails closed.
func csrfSessionTokenFromContext(r *http.Request) (string, error) {
	sess, ok := middleware.SessionFromContext(r.Context())
	if !ok {
		return "", errCSRFNoSessionInContext
	}
	return sess.CSRFToken, nil
}

// csrfAllowedHosts returns the AllowedHosts closure passed to
// csrfmw.New. The list is master-host (when configured) plus the
// resolved tenant host on the request. Both inputs come from server
// state — the request itself supplies only the host that already
// matched a tenant in TenantScope, so user-controlled headers cannot
// expand the allowlist.
func csrfAllowedHosts(masterHost string) func(*http.Request) []string {
	return func(r *http.Request) []string {
		hosts := make([]string, 0, 2)
		if masterHost != "" {
			hosts = append(hosts, masterHost)
		}
		if t, err := tenancy.FromContext(r.Context()); err == nil && t != nil && t.Host != "" {
			hosts = append(hosts, t.Host)
		}
		return hosts
	}
}
