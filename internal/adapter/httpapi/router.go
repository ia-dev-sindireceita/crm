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
	"os"
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
	"github.com/pericles-luz/crm/internal/iam/audit"
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

	// EnrollStart renders GET /m/2fa/enroll — the server-rendered
	// bootstrap start page (SIN-65264). Split from Enroll so the mint
	// stays POST-only; nil falls back to mounting Enroll on GET too
	// (legacy behaviour for wireups that predate the split).
	EnrollStart http.Handler

	// RequireMasterAuth gates every /m/* route except /m/login and
	// /m/logout on a valid master session cookie.
	RequireMasterAuth func(http.Handler) http.Handler

	// RequireMasterMFA gates all authed /m/* routes except
	// /m/2fa/verify on an enrolled + at-least-once-verified TOTP.
	RequireMasterMFA func(http.Handler) http.Handler

	// RequirePrincipalFromMaster synthesizes an iam.Principal{RoleMaster}
	// from the verified master session so the relocated /master/* operator
	// handlers (which read iam.PrincipalFromContext) work on the master
	// host without a tenant session (SIN-65264 Gap 3 bridge; SecEng C1-C6,
	// SIN-65266). Composed after RequireMasterAuth + RequireMasterMFA on
	// the master-host operator group. nil leaves the operator surface
	// un-mounted (fail closed).
	RequirePrincipalFromMaster func(http.Handler) http.Handler
}

// Deps is the constructor-injected dependency bag for NewRouter. cmd/server
// builds it once at bootstrap; tests build it with fakes.
type Deps struct {
	IAM            IAMService
	TenantResolver tenancy.Resolver
	// LoginBranding is the optional tenant-settings read port (SIN-63963 /
	// UX-F4) the pre-auth /login surface uses to fill TenantLogo +
	// WhiteLabel. Nil keeps the word-mark + platform-footer fallback, so
	// router tests and deploys that haven't wired the port render exactly
	// as before. The same reader feeds the legacy handler.LoginPost
	// credential-failure re-render and the MFA-aware wrapper (the latter
	// is wired in cmd/server's usermfa stack).
	LoginBranding tenancy.BrandingReader
	Logger        *slog.Logger
	// CommitSHA is the build-time identifier injected at link time via
	// the internal/version package (SIN-63146). It is surfaced verbatim
	// by the /health handler so cd-stg.yml's smoke gate can detect a
	// stale `docker compose pull`. Empty string is safe — the handler
	// renders "unknown" so JSON consumers never see an empty field.
	CommitSHA string
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
	// AuditLogger, when non-nil, is the audit.SplitLogger handed to
	// router-mounted handlers that need to emit a security ledger row.
	// SIN-63214 wires the tenant POST /logout handler with this logger
	// so a SecurityEventLogout row lands in audit_log_security after
	// the session row is deleted (handler-side seam added in PR #234 /
	// SIN-63188). Nil keeps the route unchanged — the handler option
	// short-circuits on a nil writer. Tests that don't exercise audit
	// leave it at the zero value.
	AuditLogger audit.SplitLogger

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

	// Theme, when non-nil, mounts the SIN-63085 per-tenant theme
	// middleware inside the tenanted group (AFTER TenantScope so the
	// resolved tenant is available on the request context). Every
	// downstream renderer reads the resolved style via
	// branding.ThemeStyleFromContext; a nil Theme keeps the chain
	// behaving as it did pre-SIN-63101 — the helpers fall back to
	// branding.DefaultThemeStyle. cmd/server constructs the
	// middleware on top of the SIN-63075 palette store so reads from
	// the middleware and writes from the SIN-63084 branding admin
	// handler share state; the same instance is also handed to
	// webbranding.Deps.ThemeCache so the AC #4 cache-invalidation
	// seam fires after every save / revert.
	Theme *middleware.ThemeMiddleware

	// MasterHost is the operator-console hostname (e.g. "master.crm.local")
	// added to the CSRF Origin/Referer allowlist alongside the resolved
	// tenant host (ADR 0073 §D1). Empty means "no master host configured"
	// — the allowlist falls back to the tenant host alone, which is the
	// minimum-viable safe value. It also pins the relocated /master/*
	// operator surface (SIN-65264): the surface mounts only when MasterHost
	// is set and is served exclusively on that host.
	MasterHost string

	// MasterAccessDeniedAuditor re-homes CA #2's deny-audit for the
	// relocated /master/* surface (SIN-65269 R2): the router hands it to
	// the MasterHostOnly host pin so an off-host probe (404) leaves an
	// audit_log_security row. RequireMasterAuth's matching rejection-audit
	// is wired separately on deps.Master.RequireMasterAuth in cmd/server.
	// nil = no-op.
	MasterAccessDeniedAuditor mastermfa.MasterAccessDeniedAuditor

	// MasterCSRFOnReject, when non-nil, is fired with the request and the
	// OriginCSRFReason on every RequireMasterOriginCSRF rejection, BEFORE
	// the 403 is written. cmd/server wires it to a Prometheus counter
	// (master_csrf_rejected_total{reason}) so a CSRF-probe campaign against
	// the master operator surface is visible on dashboards/alerts without
	// grepping the master_csrf_rejected slog event (SIN-65277). It is shared
	// by both gate instances — the /m/* auth POSTs and the relocated
	// /master/* operator surface — so one counter covers the whole master
	// console. nil = metrics-free (the pre-SIN-65277 behaviour); the gate
	// itself is unaffected.
	MasterCSRFOnReject func(*http.Request, mastermfa.OriginCSRFReason)

	// TrustedProxyMiddleware overrides the chimw.RealIP wrapper that
	// NewRouter installs as the second middleware in the chain
	// (RequestID → RealIP → …). Production wires it via
	// NewTrustedRealIP(os.Getenv) so only requests from the
	// trusted-proxy CIDR allowlist (TRUSTED_PROXY_CIDRS env, default
	// loopback + RFC1918) have their r.RemoteAddr rewritten from the
	// True-Client-IP / X-Real-IP / X-Forwarded-For headers. Tests can
	// inject a custom middleware (e.g. one that always trusts the
	// loopback peer the httptest harness uses) to exercise the rewrite
	// path; nil falls back to the secure-by-default
	// NewTrustedRealIP(os.Getenv) so router tests that don't wire it
	// keep behaving as they did pre-SIN-62978 when the immediate peer
	// is a loopback address (the default trusted set covers 127.0.0.1
	// and ::1).
	TrustedProxyMiddleware func(http.Handler) http.Handler
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

	// WebCatalog is the HTMX admin UI handler for the per-tenant
	// product catalog from internal/web/catalog (SIN-62907 / Fase 3
	// W4C). Mounted in the authed group with the same envelope as
	// WebAIPolicy: RequireAuth → RequireAction(iam.
	// ActionTenantCatalogManage). One action gates every method
	// because the gerente who manages the catalog is the only role
	// that needs to see it.
	//
	// Routes mounted:
	//   GET    /catalog
	//   GET    /catalog/new
	//   POST   /catalog
	//   GET    /catalog/{id}
	//   GET    /catalog/{id}/edit
	//   PATCH  /catalog/{id}
	//   DELETE /catalog/{id}
	//   GET    /catalog/{id}/preview
	//   GET    /catalog/{id}/arguments/new
	//   POST   /catalog/{id}/arguments
	//   GET    /catalog/{id}/arguments/{arg_id}/edit
	//   PATCH  /catalog/{id}/arguments/{arg_id}
	//   DELETE /catalog/{id}/arguments/{arg_id}
	WebCatalog http.Handler

	// WebCampaigns is the HTMX admin UI handler for the per-tenant
	// marketing-campaign dashboard from internal/web/campaigns
	// (SIN-62962 / Fase 4). Same envelope as WebCatalog: RequireAuth
	// → RequireAction(iam.ActionTenantCampaignManage). One action
	// gates every method because gerente is the only role allowed to
	// publish short links / inspect the click ledger.
	//
	// Routes mounted:
	//   GET    /campaigns
	//   GET    /campaigns/new
	//   POST   /campaigns
	//   GET    /campaigns/{slug}
	//   GET    /campaigns/{slug}/clicks
	WebCampaigns http.Handler

	// WebFunnelRules is the HTMX admin UI handler for the per-tenant
	// funnel-rules editor from internal/web/funnel/rules
	// (SIN-62961 / Fase 4). Same envelope as WebCampaigns: RequireAuth
	// → RequireAction(iam.ActionTenantFunnelRuleManage). One action
	// gates every method because gerente is the only role allowed to
	// author / mutate the rules that fire auto-handoffs.
	//
	// Routes mounted:
	//   GET    /funnel/rules
	//   GET    /funnel/rules/new
	//   POST   /funnel/rules
	//   GET    /funnel/rules/trigger-fields
	//   GET    /funnel/rules/action-fields
	//   GET    /funnel/rules/preview
	//   GET    /funnel/rules/{id}/edit
	//   PATCH  /funnel/rules/{id}
	//   PATCH  /funnel/rules/{id}/toggle
	//   DELETE /funnel/rules/{id}
	WebFunnelRules http.Handler

	// WebBillingInvoices is the HTMX UI for the per-tenant PIX-invoice
	// surface from internal/web/billing/invoices (SIN-62963 / Fase 4).
	// Same envelope as WebCatalog and WebCampaigns: RequireAuth →
	// RequireAction(iam.ActionTenantBillingView) — the tenant-side
	// billing action reused from the master billing console (SIN-62880
	// matrix). Mounted only when the wire layer supplies a non-nil
	// handler so a deploy that has not yet wired the PIX postgres
	// adapter (SIN-62958 / C7) skips the routes cleanly.
	//
	// Routes mounted:
	//   GET    /billing/invoices
	//   GET    /billing/invoices/{id}
	//   GET    /billing/invoices/{id}/status
	//   GET    /billing/dunning-banner
	WebBillingInvoices http.Handler

	// WebCampaignPublic is the SIN-62959 public redirect endpoint
	// from internal/web/public/campaign. Mounted inside the tenanted
	// group BUT outside the authed sub-group — the redirect is
	// unauthenticated by design (AC #1) and protected by per-IP rate
	// limit + cookie idempotency + open-redirect allowlist. The wire
	// in cmd/server/campaigns_public_wire.go pre-wraps the handler
	// with httpratelimit.New, so the slot here is the
	// already-throttled http.Handler.
	//
	// Nil keeps GET /c/{slug} unmounted; cmd/server passes nil when
	// DATABASE_URL or REDIS_URL is unset so partial-stack boots stay
	// green.
	//
	// Routes mounted:
	//   GET    /c/{slug}
	WebCampaignPublic http.Handler

	// WebChat is the SIN-64972 public webchat widget surface from
	// internal/adapter/channels/webchat (ADR-0021). Mounted inside the
	// tenanted group BUT outside the authed sub-group: the visitor is
	// anonymous by design, so middleware.TenantScope resolves the
	// tenant from Host (making tenancy.FromContext work in the handler)
	// while the standard cookie-CSRF middleware — which lives only on
	// the authed sub-group — never double-applies over the widget's own
	// X-Webchat-CSRF double-submit (D3). The wire in
	// cmd/server/webchat_wire.go bundles the per-tenant origin allowlist
	// (D2), origin-signature (D4), windowed rate limiter (D5) and the
	// ReceiveInbound stack, so the slot here is the ready http.Handler.
	//
	// Nil keeps /widget/v1/* unmounted; cmd/server passes nil when
	// DATABASE_URL is unset so partial-stack boots stay green. When
	// mounted, the per-tenant feature flag still gates every request to
	// 404 until the tenant is allow-listed (D7).
	//
	// Routes mounted:
	//   POST /widget/v1/session
	//   POST /widget/v1/message
	//   GET  /widget/v1/stream
	WebChat http.Handler

	// WebBranding is the HTMX admin UI for the tenant branding surface
	// (SIN-63084 / Fase 5). Same envelope as WebCatalog / WebAIPolicy:
	// RequireAuth installs the principal, RequireAction(iam.
	// ActionTenantBrandingManage) gates every method. Gerente only,
	// matching the visual-identity blast radius (the palette feeds
	// every authenticated page via the runtime theme middleware).
	//
	// Mounted only when the wire layer supplies a non-nil handler so
	// existing router tests that don't exercise the branding surface
	// keep their pre-PR behaviour.
	//
	// Routes mounted:
	//   GET    /branding
	//   POST   /branding/logo
	//   POST   /branding/palette/override
	//   POST   /branding/palette/save
	//   POST   /branding/palette/revert
	WebBranding http.Handler

	// WebLGPD is the LGPD data-subject admin surface (SIN-63186 /
	// Fase 6 PR3). Two routes with DIFFERENT actions: export gates on
	// ActionTenantLGPDExport, delete on ActionTenantLGPDDelete. Each
	// inner handler is the per-method func extracted from
	// internal/web/lgpd.Handler; mounting them separately (rather than
	// the handler-owned mux) lets the router wrap each verb with the
	// correct per-route Action constant so the AuditingAuthorizer
	// records distinct audit_log_security event_type values for an
	// export attempt vs a delete attempt.
	//
	// RateLimit is the shared lgpd_admin policy (10/min/tenant — AC #7)
	// pre-built by the wire layer. Applied as the OUTERMOST middleware
	// on both routes so a burst exceeding the cap is rejected before
	// RequireAuth / RequireAction run — preventing audit-log spam under
	// the same conditions that trip the limiter. Nil keeps the chain
	// unchanged so router tests that don't exercise the rate-limit seam
	// behave the same as the pre-PR build.
	//
	// Routes mounted (when Export and Delete are both non-nil):
	//   GET  /admin/lgpd/export   — ActionTenantLGPDExport
	//   POST /admin/lgpd/delete   — ActionTenantLGPDDelete
	WebLGPD LGPDRoutes

	// SIN-63191 / Fase 6 PR4 — public LGPD-disclosure page. Mounted in
	// the tenanted group BUT outside the authed sub-group (same envelope
	// as WebCampaignPublic). The page is unauthenticated by design —
	// LGPD art. 9 obliges the controller to publish the policy in a form
	// accessible to any data subject; gating it behind login defeats the
	// obligation.
	//
	// Routes mounted:
	//   GET /privacy
	WebPublicPrivacy http.Handler

	// SIN-63191 / Fase 6 PR4 — cookie consent banner. Two routes, both
	// mounted in the tenanted group BUT outside the authed sub-group so
	// the banner is reachable from public /privacy as well as from
	// authenticated layouts. The handler self-decides whether to record
	// a ConsentRegistry row based on whether a Principal happens to be
	// on the context (set by middleware.Auth when the visitor is
	// logged in).
	//
	// Routes mounted:
	//   GET  /consent/cookies-banner
	//   POST /consent/cookies
	WebConsent http.Handler

	// WebInbox is the SIN-63793 operator inbox HTMX UI handler from
	// internal/web/inbox (SIN-63821 / W1). When non-nil, the four
	// /inbox/* routes are mounted in the authed group so they inherit
	// TenantScope + Auth + CSRF, and each is additionally gated by
	// RequireAction(iam.ActionTenantInboxRead). Atendente is the
	// minimum role; Common is denied at the gate (CEO ACK 2026-05-31
	// on SIN-63808).
	//
	// The cmd/server wire layer constructs this with stub use cases
	// in W1 so the routes render the empty-inbox shell while the
	// real channel adapter + WalletDebitor land in W2/W4/W5. Nil
	// keeps every /inbox/* route unmounted (chi emits 404) so router
	// tests that don't exercise the surface keep their pre-PR
	// behaviour.
	//
	// Routes mounted:
	//   GET  /inbox
	//   GET  /inbox/conversations/{id}
	//   POST /inbox/conversations/{id}/messages
	//   GET  /inbox/conversations/{id}/messages/{msgID}/status
	WebInbox http.Handler

	// WebAIPanel is the SIN-65364 LGPD consent accept/cancel handler
	// (internal/web/aipanel). The inbox AI-assist gate renders a consent
	// modal that POSTs to /aipanel/consent/{accept,cancel}; without this
	// mount those POSTs 404 and the operator is stuck. When non-nil the
	// two routes are mounted in the authed group (TenantScope + Auth +
	// CSRF) behind RequireAction(iam.ActionTenantInboxRead) — same
	// minimum-role envelope as the inbox itself, since consent accept is
	// the inbox AI-assist continuation. The accept handler resolves the
	// tenant from context and the actor from the session, never from the
	// body, so authz never trusts a forgeable form field (OWASP A01).
	// Nil keeps both routes unmounted (chi emits 404).
	//
	// Routes mounted:
	//   POST /aipanel/consent/accept
	//   POST /aipanel/consent/cancel
	WebAIPanel http.Handler

	// WebDashboard is the SIN-65008 managerial dashboard HTMX UI handler
	// from internal/web/dashboard (frontend half of SIN-64963; the
	// SIN-65007 read-model backs it). When non-nil, the two routes are
	// mounted in the authed group so they inherit TenantScope + Auth +
	// CSRF, and each is gated by RequireAction(iam.ActionTenantContactRead)
	// — satisfied by tenant_atendente AND tenant_gerente, so the only
	// HTTP-loginable seed user (agent@acme = atendente) reaches it (a
	// gerente-only gate would 403 the staging smoke; see SIN-63793/63858).
	//
	// Routes mounted:
	//   GET /dashboard
	//   GET /dashboard/export.csv
	//
	// Nil keeps both routes unmounted (chi emits 404) so router tests
	// that don't exercise the surface keep their pre-PR behaviour; the
	// wire layer in cmd/server/dashboard_wire.go returns nil when the
	// metrics read-model is not wired (DATABASE_URL unset).
	WebDashboard http.Handler

	// WebWallet is the SIN-63942 / UX-F5 gerente wallet UI handler
	// from internal/web/walletui. When non-nil, the four /wallet*
	// routes are mounted in the authed group so they inherit
	// TenantScope + Auth + CSRF, and each is additionally gated by
	// RequireAction(iam.ActionTenantWalletViewLedger). Gerente is
	// the only role on the matrix that may access the wallet — the
	// gate denies atendente / common with a 403 and an audit row.
	//
	// Routes mounted:
	//   GET /wallet
	//   GET /wallet/topup
	//   GET /wallet/ledger
	//   GET /wallet/ledger.csv
	//
	// Nil keeps every /wallet* route unmounted (chi emits 404) so
	// router tests that don't exercise the surface keep their pre-PR
	// behaviour. The wire layer in cmd/server/walletui_wire.go
	// returns nil when DATABASE_URL is unset.
	WebWallet http.Handler

	// WebWASession is the SIN-66259 / Fase 4 WhatsApp non-official session
	// provisioning HTMX handler from internal/web/wasession. When non-nil,
	// the five /settings/whatsapp-session* routes are mounted in the authed
	// group so they inherit TenantScope + Auth + CSRF, and each is gated by
	// RequireAction(iam.ActionTenantWASessionManage). Gerente is the only
	// role on the matrix that may provision the session — activating it
	// accepts a board-acknowledged ToS / ban risk on behalf of the tenant,
	// so the gate denies atendente / common with a 403 and an audit row.
	//
	// Routes mounted:
	//   GET  /settings/whatsapp-session             (page)
	//   GET  /settings/whatsapp-session/status      (QR + status poll)
	//   POST /settings/whatsapp-session/consent     (record informed consent)
	//   POST /settings/whatsapp-session/connect     (activate — gated on consent)
	//   POST /settings/whatsapp-session/disconnect  (tear down)
	//
	// Nil keeps every route unmounted (chi emits 404). The wire layer in
	// cmd/server/wa_session_ui_wire.go returns nil unless the session
	// transport is mounted (FEATURE_WA_SESSION_ENABLED + DSNs) and the
	// audited consent registry can be built — deny-by-default.
	WebWASession http.Handler

	// MasterTenants bundles the three master-console tenant routes
	// from internal/web/master (SIN-62882 / Fase 2.5 C9). Each slot
	// is the inner http.Handler the wire layer hands the router;
	// NewRouter wraps each with the canonical RequireAuth →
	// RequireAction gate using the per-route Action constant from
	// SIN-62880. Any nil slot (or a nil Authorizer at the router
	// level) causes that specific route to be skipped — router tests
	// that don't exercise the master surface keep their pre-PR
	// behaviour.
	//
	// Routes mounted (all in the tenanted+authed group so they
	// inherit TenantScope + Auth + CSRF):
	//   GET   /master/tenants            — ActionMasterTenantRead
	//   POST  /master/tenants            — ActionMasterTenantCreate
	//   PATCH /master/tenants/{id}/plan  — ActionMasterSubscriptionAssignPlan
	MasterTenants MasterTenantsRoutes

	// Impersonation bundles the SIN-63958 session-bound impersonation
	// envelope routes (Start, End, Feed). The bundle is a sibling of
	// MasterTenants because the impersonation surface has heterogeneous
	// auth posture (Start = ActionMasterTenantImpersonate; End =
	// RoleMaster only so the operator can exit a stale envelope; Feed
	// = RoleMaster + owner check). Conflating with MasterTenants would
	// force the uniform RequireAction pattern onto handlers that need
	// looser / stricter gates.
	//
	// The optional middleware ImpersonationFromSession (also non-nil
	// when this bundle is wired) is applied on routes that consume the
	// active envelope. Nil slots are skipped — router tests and deploys
	// that don't wire the impersonation adapter keep their pre-PR
	// behaviour.
	Impersonation ImpersonationRoutes

	// UserMFA is the SIN-63361 user-side TOTP enforcement surface.
	// When LoginPost is non-nil the router REPLACES the legacy
	// handler.LoginPost mount on POST /login with the MFA-aware
	// wrapper from internal/adapter/httpapi/usermfa — that wrapper
	// consults the tenant-user MFARequirement before minting the
	// session cookie and 303s to /admin/2fa/{setup,verify} when the
	// principal is required to enroll/verify. Setup, Verify, and
	// Regenerate are mounted on /admin/2fa/{setup,verify,regenerate}
	// inside the tenanted group BUT outside the authed sub-group
	// (the handlers gate on the __Host-mfa-pending cookie, not on
	// the post-MFA tenant session cookie). Any nil slot causes the
	// corresponding route to be skipped — router tests that don't
	// exercise the MFA surface keep their pre-PR behaviour and the
	// legacy single-step handler.LoginPost stays mounted.
	UserMFA UserMFARoutes

	// CustomDomainEnabled reports whether the public-mux custom-domain
	// handler is mounted in the current process (SIN-62259). The actual
	// route subtree (`/tenant/custom-domains*`) lives on the public mux,
	// not in this chi router, so there is no http.Handler slot here —
	// only the boolean presence signal. SIN-63940 / UX-F3 surfaces the
	// flag on /hello-tenant so an operator who has CUSTOM_DOMAIN_UI_
	// ENABLED=1 in their env sees the link go live; when the env flag
	// is unset the card renders disabled with the standard "Indisponível
	// neste ambiente" hint.
	CustomDomainEnabled bool
}

// WebSurfaces (SIN-64985) returns the mounted/not-mounted state of every
// auth-gated web surface whose route registration is gated on a
// `deps.WebX != nil` check in NewRouter. It is the single source of
// truth for that predicate set: /health (via handler.WithSurfaces)
// reports it as booleans so an operator can `curl /health` and tell
// whether a fail-soft `build*Handler` returned nil at boot (router skips
// the mount → bare 404). The same predicates drive the route gates
// below and the /hello-tenant index, so this map and those mounts cannot
// drift.
//
// SECURITY: the returned values are booleans only. The wire failure
// reason (e.g. "web/aipolicy disabled — <DSN error>") is NEVER surfaced
// here — "mounted | not" is the information ceiling for the
// unauthenticated /health endpoint. This holds for the PUBLIC surfaces
// too (campaign_public, public_privacy, chat): whether they are mounted
// is already inferable by an unauthenticated HTTP probe — mounted yields
// the surface's own non-404 semantics, nil yields a bare 404 — so the
// boolean does not widen the attack surface. This does NOT cover the
// stale-image case (binary built from older source than the running
// deploy): the map reflects the running binary's wireup, not the source
// tree.
//
// The map MUST enumerate every simple `deps.WebX != nil`-gated web
// surface so an operator never mistakes an untracked surface for an
// unmounted one (the false-confidence this guardrail exists to kill).
// Composite gates (WebLGPD sub-fields, UserMFA sub-fields) are
// intentionally excluded: they are not a single nil-handler predicate
// and would need their own multi-field shape — out of scope here.
func (d Deps) WebSurfaces() map[string]bool {
	return map[string]bool{
		"ai_policy":        d.WebAIPolicy != nil,
		"catalog":          d.WebCatalog != nil,
		"funnel":           d.WebFunnel != nil,
		"funnel_rules":     d.WebFunnelRules != nil,
		"privacy":          d.WebPrivacy != nil,
		"campaigns":        d.WebCampaigns != nil,
		"consent":          d.WebConsent != nil,
		"inbox":            d.WebInbox != nil,
		"aipanel":          d.WebAIPanel != nil,
		"contacts":         d.WebContacts != nil,
		"dashboard":        d.WebDashboard != nil,
		"campaign_public":  d.WebCampaignPublic != nil,
		"public_privacy":   d.WebPublicPrivacy != nil,
		"chat":             d.WebChat != nil,
		"branding":         d.WebBranding != nil,
		"wallet":           d.WebWallet != nil,
		"billing_invoices": d.WebBillingInvoices != nil,
		// SIN-66259 / Fase 4 — WhatsApp session provisioning surface.
		"wa_session": d.WebWASession != nil,
	}
}

// LGPDRoutes bundles the two inner handlers and the shared rate-limit
// middleware for the LGPD data-subject admin surface (SIN-63186). Each
// http.Handler slot is the per-method func extracted from
// internal/web/lgpd.Handler so the router can apply a DIFFERENT
// RequireAction gate on each verb (export vs delete). RateLimit, when
// non-nil, is applied as the outermost wrapper on both routes so the
// 10/min/tenant cap is enforced before authz logs anything. Nil slots
// cause that specific route to be skipped — router tests that don't
// exercise the LGPD surface keep their pre-PR behaviour.
//
// SIN-63191 / Fase 6 PR4 extends the bundle with the HTMX admin pages:
//
//   - ContactPage backs GET /admin/contacts/{contactID}/lgpd — gated on
//     ActionTenantLGPDDelete (the destructive verb owns the surface).
//   - RequestsPage backs GET /admin/lgpd/requests — gated on
//     ActionTenantLGPDDelete for the same reason.
//   - DeleteForm backs POST /admin/lgpd/delete-form — form-encoded twin
//     of /admin/lgpd/delete so the page degrades to a non-HTMX POST
//     when JS is off; gated on ActionTenantLGPDDelete.
//
// All four UI routes share the lgpd_admin rate limit (RateLimit field).
type LGPDRoutes struct {
	Export       http.Handler
	Delete       http.Handler
	ContactPage  http.Handler
	RequestsPage http.Handler
	DeleteForm   http.Handler
	RateLimit    func(http.Handler) http.Handler
}

// UserMFARoutes bundles the four handlers from
// internal/adapter/httpapi/usermfa that together gate the tenant
// admin / opt-in user TOTP flow. cmd/server/usermfa_wire.go builds
// them on top of the shared IAM pgxpool and supplies all four slots
// together; router tests can inject any subset.
//
//   - LoginPost replaces handler.LoginPost on POST /login when set.
//     The MFA-aware wrapper validates credentials via the same
//     iam.Service.Login as the legacy handler, then consults the
//     TenantUserMFARequirement reader: TOTP-required principals are
//     handed a __Host-mfa-pending cookie and 303'd to /admin/2fa/setup
//     (when not yet enrolled) or /admin/2fa/verify (when enrolled);
//     non-MFA principals proceed exactly as today (302 to /hello-tenant
//     with __Host-sess-tenant). The pre-/login rate-limit middleware
//     from buildLoginRateLimit still wraps this handler.
//   - Setup renders GET/POST /admin/2fa/setup — the QR + recovery
//     codes pane. Gated on the __Host-mfa-pending cookie, not the
//     tenant session cookie, so a half-authenticated user can complete
//     enrolment.
//   - Verify renders GET/POST /admin/2fa/verify — submits the TOTP
//     or a recovery code. On success the handler mints the real
//     tenant session and 303s to the originally-requested ?next=.
//   - Regenerate renders POST /admin/2fa/regenerate — issues a fresh
//     recovery-code set after a recent successful verify.
type UserMFARoutes struct {
	LoginPost  http.Handler
	Setup      http.Handler
	Verify     http.Handler
	Regenerate http.Handler
}

// MasterTenantsRoutes bundles the three inner handlers for the master
// /master/tenants surface so NewRouter can wrap each one with its
// per-action RequireAction gate. cmd/server constructs the inner
// handlers from internal/web/master.Handler and passes them through
// via Deps.MasterTenants; tests can pass simple http.Handler stubs.
type MasterTenantsRoutes struct {
	List       http.Handler
	Create     http.Handler
	AssignPlan http.Handler
	// Detail backs GET /master/tenants/{id} (SIN-63956 / spec §9.5).
	// Hosts the "Impersonar tenant" trigger + reason modal. Gated on
	// ActionMasterTenantRead — same as List — because the page does
	// not reveal anything the list view doesn't already, and the
	// impersonation handler enforces its own ActionMasterTenantImpersonate
	// gate on the actual envelope POST.
	Detail http.Handler
	// SIN-62884 C10 — grants surface. Each handler is conditionally
	// mounted; nil slots are skipped so deploys that haven't wired
	// the wallet adapter behave the same as the pre-C10 router. The
	// two write slots (GrantsCreate / GrantsRevoke) are additionally
	// expected to be wrapped with mastermfa.RequireRecentMFA by the
	// caller before being assigned here — see the wire layer in
	// cmd/server for the canonical composition.
	GrantsNew    http.Handler
	GrantsCreate http.Handler
	GrantsRevoke http.Handler
	// SIN-63605 C? — 4-eyes approval surface for over-cap grants.
	// Each slot is the inner handler from internal/web/master; the
	// router gates each verb behind its own RequireAction action
	// constant (master.grant.request.create / .approve / .reject).
	// The four POST verbs are additionally expected to be wrapped
	// with mastermfa.RequireRecentMFA at the wire layer before being
	// assigned here; the read-only GET routes ride the existing
	// master-MFA session bit only.
	GrantRequestsCreate  http.Handler // POST /master/tenants/{id}/grants/requests
	GrantRequestsList    http.Handler // GET  /master/grants/requests
	GrantRequestsShow    http.Handler // GET  /master/grants/requests/{id}
	GrantRequestsApprove http.Handler // POST /master/grants/requests/{id}/approve
	GrantRequestsReject  http.Handler // POST /master/grants/requests/{id}/reject
}

// ImpersonationRoutes is the SIN-63958 Scope 3 surface — three master-
// console handlers that drive the session-bound impersonation envelope:
//
//	Start: POST /master/tenants/{id}/impersonate
//	       RequireAuth → RequireAction(ActionMasterTenantImpersonate) →
//	       handler.
//
//	End:   POST /master/impersonation/end
//	       RequireAuth → RequireRoleMaster → handler. NOT behind
//	       ImpersonationFromSession so an expired operator can still
//	       exit a stale envelope (AC #3 from SIN-63955; spec §2.7).
//
//	Feed:  GET  /master/impersonation/feed
//	       RequireAuth → RequireRoleMaster → handler. Owner check is
//	       enforced inside the handler.
//
// FromSession is the middleware that consumes any active envelope on
// the master subtree. Mounted by NewRouter as a wrapper on every
// /master/* route except End; nil means "no impersonation wired" and
// the wrapper is skipped (the legacy header-based middleware on /master/
// stays the only path).
type ImpersonationRoutes struct {
	Start       http.Handler
	End         http.Handler
	Feed        http.Handler
	FromSession func(http.Handler) http.Handler
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
	// SIN-62978 — trusted-proxy-aware RealIP wrapper. Bare chimw.RealIP
	// blindly trusts client-supplied True-Client-IP / X-Real-IP /
	// X-Forwarded-For headers and rewrites r.RemoteAddr; that lets a
	// caller forge the per-IP rate-limit bucket key for every public
	// endpoint (most acutely GET /c/{slug} introduced by SIN-62959).
	// The wrapper only honours the headers when the immediate TCP peer
	// is inside the trusted-proxy CIDR allowlist (Caddy on the docker
	// bridge in production). Edge strip in deploy/caddy/Caddyfile +
	// Caddyfile.stg is the first line of defence; this wrapper is the
	// belt-and-braces fallback.
	r.Use(deps.trustedRealIPMiddleware())
	r.Use(propagateRequestIDToObs)
	r.Use(slogRequestLogger(deps.Logger))
	r.Use(chimw.Recoverer)

	// /health is the only route that bypasses the tenant scope. The LB
	// must reach it without resolving a tenant by host (the host might
	// be the raw load-balancer DNS name). The commit SHA is injected
	// at boot from internal/version (SIN-63146) so cd-stg can detect a
	// stale `docker compose pull`; empty Deps.CommitSHA renders as
	// "unknown" inside the handler.
	r.Get("/health", handler.Health(deps.CommitSHA, handler.WithSurfaces(deps.WebSurfaces())))

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
		// SIN-63101 — per-tenant theme attachment. Mounted AFTER
		// TenantScope so the resolved tenant is on the context; nil
		// keeps the chain unchanged so router tests that don't wire
		// the middleware continue rendering DefaultThemeStyle.
		if deps.Theme != nil {
			tenanted.Use(deps.Theme.Handler)
		}

		// SIN-63963 / UX-F4 — GET /login renders the tenant-branded card.
		// LoginGetHandler fills TenantLogo + WhiteLabel from the
		// tenant-settings read port when deps.LoginBranding is wired; nil
		// degrades to the platform word-mark + footer (LoginGet behaviour).
		tenanted.Get("/login", handler.LoginGetHandler(handler.LoginConfig{
			Branding: deps.LoginBranding,
		}))

		// SIN-62959 — public campaign redirect (GET /c/{slug}). Mounted
		// inside the tenanted group so middleware.TenantScope resolves
		// the Host header to a Tenant BEFORE the handler runs (AC #1
		// secure-by-default exception: the endpoint is unauthenticated
		// by design, the host gate is the cross-tenant boundary). The
		// rate-limit middleware is pre-wrapped by the wire layer so
		// the slot here is the already-throttled http.Handler — see
		// cmd/server/campaigns_public_wire.go.
		if deps.WebCampaignPublic != nil {
			tenanted.Method(http.MethodGet, "/c/{slug}", deps.WebCampaignPublic)
		}

		// SIN-63191 / Fase 6 PR4 — public LGPD-disclosure page.
		// Unauthenticated by design (LGPD art. 9). Mounted alongside
		// /c/{slug} so middleware.TenantScope resolves the tenant from
		// the request host before the renderer runs.
		if deps.WebPublicPrivacy != nil {
			tenanted.Method(http.MethodGet, "/privacy", deps.WebPublicPrivacy)
		}

		// SIN-63191 / Fase 6 PR4 — cookie consent banner. Same
		// rationale as /privacy: the banner must be reachable to
		// anonymous visitors on /privacy as well as to logged-in
		// users on every authenticated layout, so the routes live
		// outside the authed sub-group. ConsentRegistry recording
		// happens only when iam.PrincipalFromContext succeeds.
		if deps.WebConsent != nil {
			tenanted.Method(http.MethodGet, "/consent/cookies-banner", deps.WebConsent)
			tenanted.Method(http.MethodPost, "/consent/cookies", deps.WebConsent)
		}

		// SIN-64972 / ADR-0021 — public webchat widget. Mounted in the
		// tenanted group, outside the authed sub-group: the visitor is
		// anonymous so TenantScope resolves the tenant from Host before
		// the handler runs, and the standard cookie-CSRF middleware
		// (authed-only) does not shadow the widget's X-Webchat-CSRF
		// double-submit. deps.WebChat re-dispatches on the exact
		// method+path it registered, so the three routes share one slot.
		if deps.WebChat != nil {
			tenanted.Method(http.MethodPost, "/widget/v1/session", deps.WebChat)
			tenanted.Method(http.MethodPost, "/widget/v1/message", deps.WebChat)
			tenanted.Method(http.MethodGet, "/widget/v1/stream", deps.WebChat)
		}

		// SIN-63361 — MFA-aware POST /login. When deps.UserMFA.LoginPost
		// is non-nil the router REPLACES the legacy handler.LoginPost
		// mount with the wrapper from internal/adapter/httpapi/usermfa.
		// The wrapper enforces the TenantUserMFARequirement contract
		// (TOTP-required users get a pending cookie + 303 to
		// /admin/2fa/{setup,verify}) before minting the tenant session.
		// The same buildLoginRateLimit wrap applies either way, so the
		// per-IP / per-email throttle that protects POST /login is
		// preserved through the MFA wireup.
		var loginPost http.Handler
		if deps.UserMFA.LoginPost != nil {
			loginPost = deps.UserMFA.LoginPost
		} else {
			loginPost = handler.LoginPost(handler.LoginConfig{
				IAM:      deps.IAM,
				Branding: deps.LoginBranding,
			})
		}
		if mw := buildLoginRateLimit(deps); mw != nil {
			loginPost = mw(loginPost)
		}
		tenanted.Method(http.MethodPost, "/login", loginPost)

		// SIN-63361 — /admin/2fa/{setup,verify,regenerate}. Mounted in
		// the tenanted group BUT outside the authed sub-group: the
		// handlers gate on the __Host-mfa-pending cookie (the proof
		// of credential success that has not yet completed TOTP), not
		// on the post-MFA __Host-sess-tenant session cookie. Reaching
		// these routes without the pending cookie returns 401 with a
		// 2fa_required audit row. Each slot is nil-safe so router
		// tests that don't wire the surface keep their pre-PR
		// behaviour and the chi router falls through to its 404 for
		// the missing routes.
		if deps.UserMFA.Setup != nil {
			tenanted.Method(http.MethodGet, "/admin/2fa/setup", deps.UserMFA.Setup)
			tenanted.Method(http.MethodPost, "/admin/2fa/setup", deps.UserMFA.Setup)
		}
		if deps.UserMFA.Verify != nil {
			tenanted.Method(http.MethodGet, "/admin/2fa/verify", deps.UserMFA.Verify)
			tenanted.Method(http.MethodPost, "/admin/2fa/verify", deps.UserMFA.Verify)
		}
		if deps.UserMFA.Regenerate != nil {
			tenanted.Method(http.MethodPost, "/admin/2fa/regenerate", deps.UserMFA.Regenerate)
		}

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
			//
			// SIN-63774 — the constructor receives the Fase 3–6 surface
			// presence flags so /hello-tenant renders a navigable index
			// of every mounted area (and an aria-disabled span for each
			// absent one). The flags read deps.WebX != nil so router
			// tests that don't wire the per-feature handlers keep their
			// pre-PR behaviour (every surface disabled, body still
			// contains the tenant name).
			helloTenant := http.Handler(handler.NewHelloTenant(handler.HelloTenantDeps{
				FunnelEnabled:      deps.WebFunnel != nil,
				FunnelRulesEnabled: deps.WebFunnelRules != nil,
				CatalogEnabled:     deps.WebCatalog != nil,
				CampaignsEnabled:   deps.WebCampaigns != nil,
				PrivacyEnabled:     deps.WebPrivacy != nil,
				AIPolicyEnabled:    deps.WebAIPolicy != nil,
				ConsentEnabled:     deps.WebConsent != nil,
				// SIN-63940 / UX-F3 — Fase 6 surfaces. Each flag reads
				// the matching dep slot the wire layer fills in
				// cmd/server; an empty/nil dep falls back to a disabled
				// card on the dashboard ("Indisponível neste ambiente —
				// verifique configuração do servidor.") rather than a
				// dead link, so the gap is visible to the operator.
				// The non-nil Extended pointer is the explicit opt-in
				// to the 13-entry index — legacy router_test.go fixtures
				// that build HelloTenantDeps{} keep the 7-entry SIN-
				// 63774 baseline.
				Extended: &handler.HelloTenantExtendedDeps{
					InboxEnabled:        deps.WebInbox != nil,
					BillingEnabled:      deps.WebBillingInvoices != nil,
					BrandingEnabled:     deps.WebBranding != nil,
					LGPDEnabled:         deps.WebLGPD.RequestsPage != nil,
					MFAEnabled:          deps.UserMFA.Setup != nil,
					CustomDomainEnabled: deps.CustomDomainEnabled,
					// SIN-63942 / UX-F5 — wallet UI presence flag.
					WalletEnabled: deps.WebWallet != nil,
					// SIN-65008 — managerial dashboard presence flag. Keeps
					// the post-login index in sync with the /dashboard mount
					// (memory hello_tenant_sync_on_mount).
					DashboardEnabled: deps.WebDashboard != nil,
					// SIN-64977 — contacts management list link.
					ContactsEnabled: deps.WebContacts != nil,
				},
			}))
			if deps.Authorizer != nil {
				helloTenant = middleware.RequireAuth(middleware.RequireAuthDeps{})(
					middleware.RequireAction(deps.Authorizer, iam.ActionTenantContactRead, nil)(helloTenant),
				)
			}
			authed.Method(http.MethodGet, "/hello-tenant", helloTenant)
			// SIN-63214 — opt the tenant /logout handler into the
			// SecurityEventLogout audit row added in PR #234. Both
			// options are nil-safe: WithLogoutAudit(nil) leaves the
			// handler in its pre-PR shape (no audit write) and
			// WithLogoutLogger(nil) falls back to slog.Default()
			// inside the constructor. Router tests that don't wire
			// either dep keep their pre-PR behaviour.
			authed.Method(http.MethodPost, "/logout", handler.Logout(
				deps.IAM,
				handler.WithLogoutAudit(deps.AuditLogger),
				handler.WithLogoutLogger(deps.Logger),
			))

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

				// SIN-64977 — contacts management surface (list/search +
				// edit). The read route (GET /contacts) gates on
				// ActionTenantContactRead exactly like /hello-tenant; the
				// edit routes gate on ActionTenantContactUpdate. The seed
				// "atendente" role holds both actions (authorizer.go),
				// avoiding the SIN-63793/63858 first-login 403. When
				// Authorizer is nil (router tests that don't exercise the
				// authz seam) the routes fall back to RequireAuth-only so
				// existing suites keep their pre-PR behaviour.
				contactsRead := webContacts
				contactsWrite := webContacts
				if deps.Authorizer != nil {
					contactsRead = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantContactRead, nil)(deps.WebContacts),
					)
					contactsWrite = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantContactUpdate, nil)(deps.WebContacts),
					)
				}
				authed.Method(http.MethodGet, "/contacts", contactsRead)
				authed.Method(http.MethodGet, "/contacts/{contactID}/edit", contactsWrite)
				authed.Method(http.MethodPost, "/contacts/{contactID}/edit", contactsWrite)
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

			// SIN-62961 — HTMX funnel-rules editor (Fase 4). Same
			// envelope as WebCampaigns: RequireAuth installs the
			// principal, RequireAction(ActionTenantFunnelRuleManage)
			// gates every method. gerente is the only role allowed
			// to author the rules that fire auto-handoffs.
			if deps.WebFunnelRules != nil {
				webFunnelRules := http.Handler(deps.WebFunnelRules)
				if deps.Authorizer != nil {
					webFunnelRules = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantFunnelRuleManage, nil)(webFunnelRules),
					)
				} else {
					webFunnelRules = middleware.RequireAuth(middleware.RequireAuthDeps{})(webFunnelRules)
				}
				authed.Method(http.MethodGet, "/funnel/rules", webFunnelRules)
				authed.Method(http.MethodGet, "/funnel/rules/new", webFunnelRules)
				authed.Method(http.MethodPost, "/funnel/rules", webFunnelRules)
				authed.Method(http.MethodGet, "/funnel/rules/trigger-fields", webFunnelRules)
				authed.Method(http.MethodGet, "/funnel/rules/action-fields", webFunnelRules)
				authed.Method(http.MethodGet, "/funnel/rules/preview", webFunnelRules)
				authed.Method(http.MethodGet, "/funnel/rules/{id}/edit", webFunnelRules)
				authed.Method(http.MethodPatch, "/funnel/rules/{id}", webFunnelRules)
				authed.Method(http.MethodPatch, "/funnel/rules/{id}/toggle", webFunnelRules)
				authed.Method(http.MethodDelete, "/funnel/rules/{id}", webFunnelRules)
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

			// SIN-62907 — HTMX catalog admin UI (Fase 3 W4C). Same
			// envelope as WebAIPolicy: RequireAuth installs the
			// principal, RequireAction(ActionTenantCatalogManage) gates
			// every method. When Authorizer is nil (router tests that
			// don't exercise the authz seam) the gate skips and the
			// inner mux still runs with a Principal.
			if deps.WebCatalog != nil {
				webCatalog := http.Handler(deps.WebCatalog)
				if deps.Authorizer != nil {
					webCatalog = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantCatalogManage, nil)(webCatalog),
					)
				} else {
					webCatalog = middleware.RequireAuth(middleware.RequireAuthDeps{})(webCatalog)
				}
				authed.Method(http.MethodGet, "/catalog", webCatalog)
				authed.Method(http.MethodGet, "/catalog/new", webCatalog)
				authed.Method(http.MethodPost, "/catalog", webCatalog)
				authed.Method(http.MethodGet, "/catalog/{id}", webCatalog)
				authed.Method(http.MethodGet, "/catalog/{id}/edit", webCatalog)
				authed.Method(http.MethodPatch, "/catalog/{id}", webCatalog)
				authed.Method(http.MethodDelete, "/catalog/{id}", webCatalog)
				authed.Method(http.MethodGet, "/catalog/{id}/preview", webCatalog)
				authed.Method(http.MethodGet, "/catalog/{id}/arguments/new", webCatalog)
				authed.Method(http.MethodPost, "/catalog/{id}/arguments", webCatalog)
				authed.Method(http.MethodGet, "/catalog/{id}/arguments/{arg_id}/edit", webCatalog)
				authed.Method(http.MethodPatch, "/catalog/{id}/arguments/{arg_id}", webCatalog)
				authed.Method(http.MethodDelete, "/catalog/{id}/arguments/{arg_id}", webCatalog)
			}

			// SIN-62962 — HTMX campaign dashboard (Fase 4). Same
			// envelope as WebCatalog: RequireAuth installs the
			// principal, RequireAction(ActionTenantCampaignManage)
			// gates every method. When Authorizer is nil (router
			// tests that don't exercise the authz seam) the gate
			// skips and the inner mux still runs with a Principal.
			if deps.WebCampaigns != nil {
				webCampaigns := http.Handler(deps.WebCampaigns)
				if deps.Authorizer != nil {
					webCampaigns = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantCampaignManage, nil)(webCampaigns),
					)
				} else {
					webCampaigns = middleware.RequireAuth(middleware.RequireAuthDeps{})(webCampaigns)
				}
				authed.Method(http.MethodGet, "/campaigns", webCampaigns)
				authed.Method(http.MethodGet, "/campaigns/new", webCampaigns)
				authed.Method(http.MethodPost, "/campaigns", webCampaigns)
				authed.Method(http.MethodGet, "/campaigns/{slug}", webCampaigns)
				authed.Method(http.MethodGet, "/campaigns/{slug}/clicks", webCampaigns)
			}

			// SIN-63084 — HTMX branding admin (Fase 5). Same envelope
			// as WebCatalog / WebAIPolicy: RequireAuth installs the
			// principal, RequireAction(ActionTenantBrandingManage)
			// gates every method. The page is read-then-write; chi
			// dispatches by verb so the GET form short-circuits CSRF
			// while the POSTs run the full csrfmw chain.
			if deps.WebBranding != nil {
				webBranding := http.Handler(deps.WebBranding)
				if deps.Authorizer != nil {
					webBranding = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantBrandingManage, nil)(webBranding),
					)
				} else {
					webBranding = middleware.RequireAuth(middleware.RequireAuthDeps{})(webBranding)
				}
				authed.Method(http.MethodGet, "/branding", webBranding)
				authed.Method(http.MethodPost, "/branding/logo", webBranding)
				authed.Method(http.MethodPost, "/branding/palette/override", webBranding)
				authed.Method(http.MethodPost, "/branding/palette/save", webBranding)
				authed.Method(http.MethodPost, "/branding/palette/revert", webBranding)
			}

			// SIN-63186 — LGPD data-subject admin surface (Fase 6 PR3).
			// Each verb is wrapped with the per-route Action constant
			// from ADR 0090 so a tenant-role user hitting either route
			// without RoleTenantGerente gets a 403 + audit_log_security
			// row tagged with the specific event_type (export vs
			// forget). Rate-limit (AC #7, 10/min/tenant via lgpd_admin
			// policy) wraps OUTSIDE RequireAction so a burst is
			// rejected before authz logs the deny — preventing
			// audit-log spam under the same conditions that trip the
			// limiter. When Authorizer is nil (router tests that don't
			// exercise the authz seam) the gate skips and the inner
			// handler still runs with a Principal.
			if deps.WebLGPD.Export != nil && deps.WebLGPD.Delete != nil {
				exportH := http.Handler(deps.WebLGPD.Export)
				deleteH := http.Handler(deps.WebLGPD.Delete)
				if deps.Authorizer != nil {
					exportH = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantLGPDExport, nil)(exportH),
					)
					deleteH = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantLGPDDelete, nil)(deleteH),
					)
				} else {
					exportH = middleware.RequireAuth(middleware.RequireAuthDeps{})(exportH)
					deleteH = middleware.RequireAuth(middleware.RequireAuthDeps{})(deleteH)
				}
				if deps.WebLGPD.RateLimit != nil {
					exportH = deps.WebLGPD.RateLimit(exportH)
					deleteH = deps.WebLGPD.RateLimit(deleteH)
				}
				authed.Method(http.MethodGet, "/admin/lgpd/export", exportH)
				authed.Method(http.MethodPost, "/admin/lgpd/delete", deleteH)
			}

			// SIN-63191 / Fase 6 PR4 — HTMX admin pages. All three
			// share the lgpd_admin rate limit and the
			// ActionTenantLGPDDelete gate (the destructive verb owns
			// the surface — an operator who can read the requests list
			// is the same one who can issue a deletion).
			if deps.WebLGPD.ContactPage != nil {
				h := http.Handler(deps.WebLGPD.ContactPage)
				if deps.Authorizer != nil {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantLGPDDelete, nil)(h),
					)
				} else {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(h)
				}
				if deps.WebLGPD.RateLimit != nil {
					h = deps.WebLGPD.RateLimit(h)
				}
				authed.Method(http.MethodGet, "/admin/contacts/{contactID}/lgpd", h)
			}
			if deps.WebLGPD.RequestsPage != nil {
				h := http.Handler(deps.WebLGPD.RequestsPage)
				if deps.Authorizer != nil {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantLGPDDelete, nil)(h),
					)
				} else {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(h)
				}
				if deps.WebLGPD.RateLimit != nil {
					h = deps.WebLGPD.RateLimit(h)
				}
				authed.Method(http.MethodGet, "/admin/lgpd/requests", h)
			}
			if deps.WebLGPD.DeleteForm != nil {
				h := http.Handler(deps.WebLGPD.DeleteForm)
				if deps.Authorizer != nil {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantLGPDDelete, nil)(h),
					)
				} else {
					h = middleware.RequireAuth(middleware.RequireAuthDeps{})(h)
				}
				if deps.WebLGPD.RateLimit != nil {
					h = deps.WebLGPD.RateLimit(h)
				}
				authed.Method(http.MethodPost, "/admin/lgpd/delete-form", h)
			}

			// SIN-63821 — operator inbox surface (parent SIN-63793).
			// Same envelope as the other web/* handlers: RequireAuth
			// installs the principal, RequireAction(ActionTenantInboxRead)
			// gates every method. Atendente is the minimum role; Common
			// is denied (CEO ACK on SIN-63808). When Authorizer is nil
			// (router tests that don't exercise the authz seam) the gate
			// skips and the inner mux still runs with a Principal.
			if deps.WebInbox != nil {
				webInbox := http.Handler(deps.WebInbox)
				if deps.Authorizer != nil {
					webInbox = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantInboxRead, nil)(webInbox),
					)
				} else {
					webInbox = middleware.RequireAuth(middleware.RequireAuthDeps{})(webInbox)
				}
				authed.Method(http.MethodGet, "/inbox", webInbox)
				authed.Method(http.MethodGet, "/inbox/conversations/{id}", webInbox)
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/messages", webInbox)
				authed.Method(http.MethodGet, "/inbox/conversations/{id}/messages/{msgID}/status", webInbox)
				// SIN-65419 — conversation-thread live-refresh poll route. Same
				// defect class as the assign / ai-assist / reset routes below:
				// the inner mux (web/inbox Routes) registers it conditionally on
				// the ListMessagesSince dep, but chi enumerates the subtree
				// route-by-route, so the GET must be listed here too or it 404s
				// before the handler. PR #395 added the inner-mux route, the
				// live-poll sentinel, and the use-case wiring but omitted this
				// chi mount — so the conversation view rendered the sentinel
				// while its every-3s GET .../messages/since hit chi's 404 and the
				// thread never live-refreshed (confirmed on staging: sentinel
				// present, route 404 with the chi default body). The inner-mux
				// handler tests passed because they bypass chi. Same RequireAuth +
				// RequireAction(ActionTenantInboxRead) read envelope as the view
				// and status polls. When ListMessagesSince is nil the inner mux
				// returns 404 for the GET, so listing it here is safe either way.
				authed.Method(http.MethodGet, "/inbox/conversations/{id}/messages/since", webInbox)
				// SIN-64979 — conversation-assignment write route. The inner
				// mux (web/inbox Routes) registers it conditionally on the
				// AssignConversation dep, but chi enumerates the subtree
				// route-by-route, so the POST must be listed here too or it
				// 404s before the handler (the inner-mux handler tests pass
				// without it — they bypass chi). Same RequireAuth +
				// RequireAction(ActionTenantInboxRead) envelope as the reads;
				// the route additionally inherits the authed group's
				// RequireCSRF gate. When the dep is nil the inner mux returns
				// 404 for the POST, so listing it here is safe either way.
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/assign", webInbox)
				// SIN-65004 — ai-assist write route. Same defect class as
				// the assign route above: the inner mux (web/inbox Routes)
				// registers it conditionally on AIAssist.Summarizer, but
				// chi enumerates the subtree route-by-route, so the POST
				// must be listed here too or it 404s before the handler
				// (the inner-mux handler tests pass without it — they
				// bypass chi). Same RequireAuth +
				// RequireAction(ActionTenantInboxRead) envelope as the
				// reads; the route additionally inherits the authed group's
				// RequireCSRF gate. When AIAssist.Summarizer is nil the
				// inner mux returns 404 for the POST, so listing it here is
				// safe either way (the feature stays gated).
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/ai-assist", webInbox)
				// SIN-65392 — fakellm training-conversation reset route.
				// Same defect class as the assign + ai-assist routes above:
				// the inner mux (web/inbox Routes) registers it conditionally
				// on the ResetConversation dep, but chi enumerates the subtree
				// route-by-route, so the POST must be listed here too or it
				// 404s before the handler. PR #391 added the inner-mux route,
				// the "Apagar mensagens" button, and the use-case wiring but
				// omitted this chi mount — so the button rendered while the
				// POST hit chi's 404 (confirmed on staging, SIN-65406). The
				// inner-mux handler tests passed because they bypass chi. Same
				// RequireAuth + RequireAction(ActionTenantInboxRead) envelope
				// as the reads plus the authed group's RequireCSRF gate. When
				// ResetConversation is nil the inner mux returns 404 for the
				// POST, so listing it here is safe either way.
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/reset", webInbox)
				// SIN-65473 — Transferir + Encerrar conversa write routes.
				// Same chi-enumeration discipline as the assign / ai-assist /
				// reset routes above: the inner mux (web/inbox Routes) registers
				// these conditionally (transfer on AssignConversation; close /
				// reopen on CloseConversation + ReopenConversation), but chi
				// enumerates the /inbox subtree route-by-route, so each POST must
				// be listed here or it 404s before the handler while the rendered
				// button posts into the void (the recurring defect on assign
				// SIN-64979, reset SIN-65406, live-poll SIN-65419). Same
				// RequireAuth + RequireAction(ActionTenantInboxRead) envelope as
				// the reads plus the authed group's RequireCSRF gate. When the
				// backing dep is nil the inner mux returns 404, so listing them
				// here is safe either way. Transfer reuses the assign handler —
				// it is the same reassignment operation surfaced from the
				// customer-actions panel.
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/transfer", webInbox)
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/close", webInbox)
				authed.Method(http.MethodPost, "/inbox/conversations/{id}/reopen", webInbox)
			}

			// SIN-65364 — LGPD consent accept/cancel endpoints. The inbox
			// AI-assist gate renders a consent modal (when the tenant runs
			// with ai_policy.consent_required=true) that POSTs to these two
			// routes; before this mount they 404'd and the operator was
			// stuck. Same envelope as the inbox surface: RequireAuth installs
			// the principal, RequireAction(ActionTenantInboxRead) gates both
			// methods (atendente minimum; Common denied), and the authed
			// group's RequireCSRF gate additionally protects the POSTs. The
			// accept handler resolves tenant from context + actor from the
			// session, never from the body, so authz never trusts a forgeable
			// form field (OWASP A01 / BOPLA). When Authorizer is nil (router
			// tests) the gate skips and the inner mux still runs with a
			// Principal. When the dep is nil both routes stay unmounted.
			if deps.WebAIPanel != nil {
				webAIPanel := http.Handler(deps.WebAIPanel)
				if deps.Authorizer != nil {
					webAIPanel = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantInboxRead, nil)(webAIPanel),
					)
				} else {
					webAIPanel = middleware.RequireAuth(middleware.RequireAuthDeps{})(webAIPanel)
				}
				authed.Method(http.MethodPost, "/aipanel/consent/accept", webAIPanel)
				authed.Method(http.MethodPost, "/aipanel/consent/cancel", webAIPanel)
			}

			// SIN-65008 — managerial dashboard / relatórios surface
			// (frontend half of SIN-64963; SIN-65007 read-model). Same
			// envelope as the other web/* handlers: RequireAuth installs
			// the principal, RequireAction(ActionTenantContactRead) gates
			// every method. That action is satisfied by atendente AND
			// gerente, so the seed atendente reaches the page (a
			// gerente-only gate would 403 the staging smoke). Both routes
			// are reads; the authed group's RequireCSRF short-circuits
			// safely on GET. When Authorizer is nil (router tests) the
			// gate skips and the inner mux still runs with a Principal.
			if deps.WebDashboard != nil {
				webDashboard := http.Handler(deps.WebDashboard)
				if deps.Authorizer != nil {
					webDashboard = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantContactRead, nil)(webDashboard),
					)
				} else {
					webDashboard = middleware.RequireAuth(middleware.RequireAuthDeps{})(webDashboard)
				}
				authed.Method(http.MethodGet, "/dashboard", webDashboard)
				authed.Method(http.MethodGet, "/dashboard/export.csv", webDashboard)
			}

			// SIN-63942 / UX-F5 — gerente wallet UI. Four routes share
			// the RequireAuth + RequireAction envelope: the action is
			// ActionTenantWalletViewLedger (gerente-only on the ADR-0090
			// matrix; atendente / common are denied at the gate). When
			// Authorizer is nil (router tests) the gate skips and the
			// inner mux still runs with a Principal in context.
			if deps.WebWallet != nil {
				webWallet := http.Handler(deps.WebWallet)
				if deps.Authorizer != nil {
					webWallet = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantWalletViewLedger, nil)(webWallet),
					)
				} else {
					webWallet = middleware.RequireAuth(middleware.RequireAuthDeps{})(webWallet)
				}
				authed.Method(http.MethodGet, "/wallet", webWallet)
				authed.Method(http.MethodGet, "/wallet/topup", webWallet)
				authed.Method(http.MethodGet, "/wallet/ledger", webWallet)
				authed.Method(http.MethodGet, "/wallet/ledger.csv", webWallet)
			}

			// SIN-66259 / Fase 4 — WhatsApp non-official session
			// provisioning surface. Same envelope as the wallet / branding
			// admin surfaces: RequireAuth → RequireAction. The action is
			// ActionTenantWASessionManage (gerente-only on the ADR 0090
			// matrix; atendente / common are denied at the gate). All five
			// patterns are enumerated on chi so the outer match and the
			// inner mux agree — the inner http.Handler is the stdlib
			// *http.ServeMux from web/wasession.Handler.Routes. When
			// Authorizer is nil (router tests) the gate is skipped and the
			// inner mux still runs behind RequireAuth.
			if deps.WebWASession != nil {
				webWASession := http.Handler(deps.WebWASession)
				if deps.Authorizer != nil {
					webWASession = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantWASessionManage, nil)(webWASession),
					)
				} else {
					webWASession = middleware.RequireAuth(middleware.RequireAuthDeps{})(webWASession)
				}
				authed.Method(http.MethodGet, "/settings/whatsapp-session", webWASession)
				authed.Method(http.MethodGet, "/settings/whatsapp-session/status", webWASession)
				authed.Method(http.MethodPost, "/settings/whatsapp-session/consent", webWASession)
				authed.Method(http.MethodPost, "/settings/whatsapp-session/connect", webWASession)
				authed.Method(http.MethodPost, "/settings/whatsapp-session/disconnect", webWASession)
			}

			// SIN-62963 — HTMX PIX-invoice surface (Fase 4). Reuses
			// the tenant-side billing action from SIN-62880; the
			// production matrix restricts ActionTenantBillingView to
			// RoleTenantGerente, matching the "admin do tenant" AC.
			// The mount is conditional so a deploy that has not yet
			// wired the PIX postgres adapter (C7) skips the routes.
			if deps.WebBillingInvoices != nil {
				webInvoices := http.Handler(deps.WebBillingInvoices)
				if deps.Authorizer != nil {
					webInvoices = middleware.RequireAuth(middleware.RequireAuthDeps{})(
						middleware.RequireAction(deps.Authorizer, iam.ActionTenantBillingView, nil)(webInvoices),
					)
				} else {
					webInvoices = middleware.RequireAuth(middleware.RequireAuthDeps{})(webInvoices)
				}
				authed.Method(http.MethodGet, "/billing/invoices", webInvoices)
				authed.Method(http.MethodGet, "/billing/invoices/{id}", webInvoices)
				authed.Method(http.MethodGet, "/billing/invoices/{id}/status", webInvoices)
				authed.Method(http.MethodGet, "/billing/dunning-banner", webInvoices)
			}

			// SIN-65264 — the /master/* operator surface was RELOCATED off
			// this tenant-host authed group onto the master-console host
			// (see the master-operator group below, mounted on r directly).
			// A tenant session can no longer reach it: it is served only on
			// MasterHost behind the master session + MFA + principal bridge.
			// SecEng C2 (SIN-65266) requires removing this mount — chi is
			// not host-aware, so leaving it here would double-register the
			// /master/* paths and panic.
		})
	})

	// /m/* master-console routes. Skipped when MasterDeps is zero so
	// existing tests and health-only mode are unaffected.
	if deps.Master.Login != nil {
		r.Route("/m", func(m chi.Router) {
			// CSRF-6 (SIN-65269) — the master auth/bootstrap POSTs
			// (/m/login, /m/logout, /m/2fa/verify, /m/2fa/enroll mint,
			// /m/2fa/recovery/regenerate) had NO CSRF control. Front the
			// whole /m/* group with the Option-B Origin/Referer gate so a
			// forged same-site POST from a tenant subdomain is rejected
			// (the master and tenant hosts share a registrable domain, so
			// SameSite=Strict does not isolate them). Safe methods
			// short-circuit inside the middleware, so GET /m/login and the
			// GET 2FA pages are unaffected. Empty MasterHost fails closed
			// (CSRF-3) — the operator console is unusable without
			// MASTER_CONSOLE_HOST set, which is the intended posture.
			m.Use(mastermfa.RequireMasterOriginCSRF(mastermfa.RequireMasterOriginCSRFConfig{
				MasterHost: deps.MasterHost,
				Logger:     deps.Logger,
				OnReject:   deps.MasterCSRFOnReject,
			}))
			// Bootstrap routes — no session required.
			m.Method(http.MethodGet, "/login", deps.Master.Login)
			m.Method(http.MethodPost, "/login", deps.Master.Login)
			// /m/logout is POST-only (SIN-65232): a GET-able logout lets any
			// cross-site <img>/link force-log-out a master operator (CSRF).
			// Matches the tenant POST /logout shape above. The handler stays
			// method-tolerant for reuse, but this is the only mounted verb.
			m.Method(http.MethodPost, "/logout", deps.Master.Logout)

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

					// GET start page (SIN-65264 Bug 2b): serve the
					// enroll-start HTML (which posts to the mint endpoint).
					// Falls back to Enroll if EnrollStart is not wired.
					enrollStart := http.Handler(deps.Master.Enroll)
					if deps.Master.EnrollStart != nil {
						enrollStart = deps.Master.EnrollStart
					}
					mfa.Method(http.MethodGet, "/2fa/enroll", enrollStart)
					mfa.Method(http.MethodPost, "/2fa/enroll", deps.Master.Enroll)
					mfa.Method(http.MethodPost, "/2fa/recovery/regenerate", deps.Master.Regenerate)
				})
			})
		})
	}

	// /master/* operator surface — master-session chain (SIN-65264 Leg 5b).
	// Mounted directly on r (NOT inside tenanted) so no tenant session or
	// TenantScope context is required. A tenant host receives 404 from
	// MasterHostOnly before any auth or session logic runs (SecEng C2).
	//
	// Chain: MasterHostOnly → RequireMasterOriginCSRF → RequireMasterAuth →
	// RequireMasterMFA → RequirePrincipalFromMaster → RequireAction → handler.
	// RequireAuth is intentionally absent (SecEng C3: it 401s on a missing
	// *tenant* session; the master principal is synthesised by the bridge).
	//
	// Gating: all five deps must be present. A missing Authorizer keeps the
	// surface dark (deny-by-default; no un-audited action gate).
	if deps.Master.Login != nil &&
		deps.MasterHost != "" &&
		deps.Master.RequireMasterAuth != nil &&
		deps.Master.RequireMasterMFA != nil &&
		deps.Master.RequirePrincipalFromMaster != nil &&
		deps.Authorizer != nil {
		r.Group(func(mo chi.Router) {
			mo.Use(mastermfa.MasterHostOnly(deps.MasterHost, deps.Logger, deps.MasterAccessDeniedAuditor))
			mo.Use(mastermfa.RequireMasterOriginCSRF(mastermfa.RequireMasterOriginCSRFConfig{
				MasterHost: deps.MasterHost,
				Logger:     deps.Logger,
				OnReject:   deps.MasterCSRFOnReject,
			}))
			mo.Use(deps.Master.RequireMasterAuth)
			mo.Use(deps.Master.RequireMasterMFA)
			mo.Use(deps.Master.RequirePrincipalFromMaster)

			// SIN-65368/SIN-65369: ImpersonationFromSession must wrap
			// EVERY /master/* route except End so the active envelope is
			// resolved onto the request context. Page handlers read it via
			// middleware.ActiveImpersonation(ctx) to render the red
			// impersonation banner ({{with .ActiveImpersonation}}); without
			// it the banner is silently suppressed on every operator page.
			// It is a router-level middleware here (not inline on a handful
			// of handlers) so any future /master/* route inherits it — the
			// design the ImpersonationRoutes godoc documents ("Mounted by
			// NewRouter as a wrapper on every /master/* route except End").
			//
			// End is the one deliberate exclusion: FromSession ends + 303
			// redirects an expired envelope (step 4) and swaps the request
			// into the impersonated tenant's context (step 7). POST
			// /master/impersonation/end must instead reach its handler in
			// the master's own context even when the envelope already
			// expired, so an operator can always exit a stale envelope (see
			// the ImpersonationRoutes godoc + impersonation_session.go §1.3).
			// It stays mounted directly on mo, below, outside moImp.
			moImp := mo.With()
			if deps.Impersonation.FromSession != nil {
				moImp = mo.With(deps.Impersonation.FromSession)
			}

			// MasterTenants surface (SIN-62882).
			if deps.MasterTenants.List != nil {
				moImp.Method(http.MethodGet, "/master/tenants",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterTenantRead, nil)(deps.MasterTenants.List))
			}
			if deps.MasterTenants.Create != nil {
				moImp.Method(http.MethodPost, "/master/tenants",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterTenantCreate, nil)(deps.MasterTenants.Create))
			}
			if deps.MasterTenants.AssignPlan != nil {
				moImp.Method(http.MethodPatch, "/master/tenants/{id}/plan",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterSubscriptionAssignPlan, nil)(deps.MasterTenants.AssignPlan))
			}
			if deps.MasterTenants.Detail != nil {
				moImp.Method(http.MethodGet, "/master/tenants/{id}",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterTenantRead, nil)(deps.MasterTenants.Detail))
			}
			// Grants surface (SIN-62884).
			if deps.MasterTenants.GrantsNew != nil {
				moImp.Method(http.MethodGet, "/master/tenants/{id}/grants/new",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantCourtesyFreeSubscriptionPeriod, nil)(deps.MasterTenants.GrantsNew))
			}
			if deps.MasterTenants.GrantsCreate != nil {
				moImp.Method(http.MethodPost, "/master/tenants/{id}/grants",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantCourtesyFreeSubscriptionPeriod, nil)(deps.MasterTenants.GrantsCreate))
			}
			if deps.MasterTenants.GrantsRevoke != nil {
				moImp.Method(http.MethodPost, "/master/grants/{id}/revoke",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantCourtesyRevoke, nil)(deps.MasterTenants.GrantsRevoke))
			}
			// Grant-requests surface (SIN-63605).
			if deps.MasterTenants.GrantRequestsCreate != nil {
				moImp.Method(http.MethodPost, "/master/tenants/{id}/grants/requests",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantRequestCreate, nil)(deps.MasterTenants.GrantRequestsCreate))
			}
			if deps.MasterTenants.GrantRequestsList != nil {
				moImp.Method(http.MethodGet, "/master/grants/requests",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantRequestApprove, nil)(deps.MasterTenants.GrantRequestsList))
			}
			if deps.MasterTenants.GrantRequestsShow != nil {
				moImp.Method(http.MethodGet, "/master/grants/requests/{id}",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantRequestApprove, nil)(deps.MasterTenants.GrantRequestsShow))
			}
			if deps.MasterTenants.GrantRequestsApprove != nil {
				moImp.Method(http.MethodPost, "/master/grants/requests/{id}/approve",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantRequestApprove, nil)(deps.MasterTenants.GrantRequestsApprove))
			}
			if deps.MasterTenants.GrantRequestsReject != nil {
				moImp.Method(http.MethodPost, "/master/grants/requests/{id}/reject",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterGrantRequestReject, nil)(deps.MasterTenants.GrantRequestsReject))
			}
			// Impersonation surface (SIN-63958). RequireRoleMaster instead
			// of RequireAction for End/Feed (see original mount comment).
			// Start + Feed ride moImp's FromSession wrapper; the previous
			// inline FromSession wrap is now redundant and was removed.
			if deps.Impersonation.Start != nil {
				moImp.Method(http.MethodPost, "/master/tenants/{id}/impersonate",
					middleware.RequireAction(deps.Authorizer, iam.ActionMasterTenantImpersonate, nil)(deps.Impersonation.Start))
			}
			// End is deliberately mounted on mo, NOT moImp: it must reach
			// its handler even with an expired envelope (FromSession would
			// 303-redirect it) and in the master's own tenant context.
			if deps.Impersonation.End != nil {
				mo.Method(http.MethodPost, "/master/impersonation/end",
					middleware.RequireRoleMaster()(deps.Impersonation.End))
			}
			if deps.Impersonation.Feed != nil {
				moImp.Method(http.MethodGet, "/master/impersonation/feed",
					middleware.RequireRoleMaster()(deps.Impersonation.Feed))
			}
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

// trustedRealIPMiddleware returns the SIN-62978 RealIP wrapper. Honours
// an explicit Deps.TrustedProxyMiddleware override when set; otherwise
// builds the production-safe NewTrustedRealIP(os.Getenv) which trusts
// loopback + RFC1918 by default and reads TRUSTED_PROXY_CIDRS as an
// override. The fallback uses os.Getenv directly so router tests that
// run inside httptest (loopback peer) still see the RealIP rewrite —
// the default trusted set covers 127.0.0.1 + ::1.
func (d Deps) trustedRealIPMiddleware() func(http.Handler) http.Handler {
	if d.TrustedProxyMiddleware != nil {
		return d.TrustedProxyMiddleware
	}
	return NewTrustedRealIP(os.Getenv)
}
