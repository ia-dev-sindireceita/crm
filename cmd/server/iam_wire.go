package main

// SIN-62527 / SIN-62217 + SIN-62348 wiring — chi router (IAM routes).
//
// buildIAMHandler assembles the production dependency chain
// (Postgres + Redis + IAM) and returns the chi router as an http.Handler.
// The returned handler serves /login, /logout, /hello-tenant, and /m/*.
//
// SIN-65223 (Child B) wires deps.Master: buildMasterMFAStack assembles
// the master adapters into mastermfa ports and buildMasterDeps turns them
// into the /m/* handlers + RequireMasterAuth/RequireMasterMFA middlewares.
// The stack is the noop fallback (deps.Master stays zero, /m/* unmounted)
// when the master seed key / actor id is unset, so health-only boots are
// unaffected.
//
// Returns (nil, no-op) when DATABASE_URL or REDIS_URL is unset so
// cmd/server boots cleanly in health-only / custom-domain-only mode.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	goredis "github.com/redis/go-redis/v9"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	"github.com/pericles-luz/crm/internal/adapter/httpapi/middleware"
	slackadapter "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/obs"
	"github.com/pericles-luz/crm/internal/tenancy"
	"github.com/pericles-luz/crm/internal/version"
)

const envSlackWebhook = "SLACK_WEBHOOK_URL"

// envMasterConsoleHost names the operator-console hostname env var
// (SIN-65076). When set, its value flows into httpapi.Deps.MasterHost,
// which adds the host to the CSRF Origin/Referer allowlist (router.go
// csrfAllowedHosts) so the /m/* master console is reachable on a
// dedicated host. Unset is the safe default: the allowlist falls back
// to the resolved tenant host alone ("no master host configured").
const envMasterConsoleHost = "MASTER_CONSOLE_HOST"

// masterConsoleHost reads MASTER_CONSOLE_HOST and returns it for
// httpapi.Deps.MasterHost. An empty value is a graceful degradation,
// not an error: the CSRF allowlist falls back to the tenant host alone
// (router.go:191 "no master host configured"). We log the disabled
// state at boot — same nil-safe wire-log pattern as the other optional
// surfaces in buildIAMHandler — so an operator can tell from the logs
// whether the console host was provisioned. DNS/TLS for the host is a
// separate operator action (see docs/deploy/staging.md).
func masterConsoleHost(getenv func(string) string) string {
	host := strings.TrimSpace(getenv(envMasterConsoleHost))
	if host == "" {
		log.Printf("crm: master console host unset (%s) — CSRF allowlist uses tenant host only", envMasterConsoleHost)
	}
	return host
}

// iamRoutes lists the path patterns the chi router handles on the public mux.
// Registering them explicitly keeps the stdlib mux in control of dispatch
// order — custom-domain catch-all at "/" still fires last.
//
// SIN-62855 added the /contacts/ subtree (web/contacts HTMX UI) — chi
// handles it inside the authed group when Deps.WebContacts is wired.
// The trailing slash makes it a stdlib subtree pattern that catches
// /contacts/{id} and /contacts/identity/split.
//
// SIN-62862 adds the funnel HTMX UI:
//   - "/funnel" matches the exact board path GET /funnel.
//   - "/funnel/" matches the three sub-routes (transitions, conversations/{id}/history,
//     modal/close); Go's mux prefers the longer pattern, so the exact
//     "/funnel" still wins for GET /funnel.
//
// SIN-62354 adds the privacy / DPA disclosure UI (Fase 3, decisão #8):
//   - "/settings/privacy" matches the page itself.
//   - "/settings/privacy/dpa.md" matches the DPA download. The longer
//     pattern wins by Go 1.22 mux specificity, so the exact page route
//     still hits the page handler.
//
// SIN-64973 adds the AI-policy admin UI (SIN-62906). The handler was
// built (cmd/server/ai_policy_wire.go) and wired into the chi router
// via iamHandlerOpts.WebAIPolicy, but the dispatch prefix was never
// added here — so the public stdlib mux never delegated
// "/settings/ai-policy*" to chi and the custom-domain catch-all at "/"
// swallowed it, returning a clean 404 (not the authed 302) in staging.
// Same wireup gap memory reference_crm_router_nil_dep_silent_skip warns
// about, at the public-mux dispatch layer.
//   - "/settings/ai-policy" matches the list page (GET) + create (POST).
//   - "/settings/ai-policy/" matches the subtree (new, preview,
//     {scope}/{id}/edit, PATCH/DELETE {scope}/{id}). The exact pattern
//     still wins for GET/POST /settings/ai-policy by mux specificity.
var iamRoutes = []string{
	"/login",
	"/logout",
	"/hello-tenant",
	// SIN-65575 — a rota exata GET /contacts (router.go contactsRead, add.
	// em SIN-64977) precisa do padrão exato no stdlib mux; sem ela o mux
	// redireciona /contacts → /contacts/ (sem handler de índice) → 404.
	"/contacts",
	"/contacts/",
	"/funnel",
	"/funnel/",
	"/settings/privacy",
	"/settings/privacy/dpa.md",
	"/settings/ai-policy",
	"/settings/ai-policy/",
	"/catalog",
	"/catalog/",
	"/campaigns",
	"/campaigns/",
	// SIN-62959 — public campaign redirect endpoint (GET /c/{slug}).
	// The stdlib mux dispatches "/c/" (subtree) to the chi router,
	// which then re-matches the Go 1.22 method+pattern "GET /c/{slug}"
	// inside the tenanted group. The custom-domain catch-all at "/"
	// still loses to the more-specific "/c/" prefix on every request.
	"/c/",
	// SIN-63186 — LGPD admin surface (Fase 6 PR3). The stdlib mux
	// dispatches "/admin/lgpd/" (subtree) to the chi router, which then
	// re-matches "GET /admin/lgpd/export" and "POST /admin/lgpd/delete"
	// inside the authed tenanted group. Without this prefix the
	// custom-domain catch-all at "/" would shadow both routes and the
	// SIN-63186 RequireAction gate would never run.
	"/admin/lgpd/",
	// SIN-63361 — user-side TOTP enrolment + verify (Fase 6 / ADR
	// 0102). The stdlib mux dispatches "/admin/2fa/" (subtree) to
	// the chi router, which then re-matches the three routes
	// (setup/verify/regenerate) inside the tenanted group. Without
	// this prefix the custom-domain catch-all at "/" would shadow
	// every enrolment URL and the seeded totp_required_at flag would
	// be unreachable — exactly the SIN-63359 Lens 1 failure.
	"/admin/2fa/",
	// SIN-63191 — Fase 6 PR4. /admin/contacts/{id}/lgpd is shadowed by
	// the existing /contacts/ prefix only when the chi router is wired,
	// so we register a dedicated subtree under /admin/contacts/ for the
	// LGPD page.
	"/admin/contacts/",
	// SIN-63191 — public LGPD pages. /privacy is the disclosure page
	// (unauthenticated by design); /consent/ carries the cookie banner
	// + decision POST. Both subtrees must hit the chi router so the
	// custom-domain catch-all at "/" does not shadow them.
	"/privacy",
	"/consent/",
	// SIN-63821 — operator inbox surface (parent SIN-63793). The
	// "/inbox" exact pattern matches GET /inbox; the "/inbox/"
	// subtree pattern catches the three nested routes
	// (conversations/{id}, conversations/{id}/messages, conversations/
	// {id}/messages/{msgID}/status). Without registering both on the
	// stdlib mux the custom-domain catch-all at "/" would shadow them.
	"/inbox",
	"/inbox/",
	// SIN-65364 — LGPD consent accept/cancel endpoints behind the inbox
	// AI-assist gate. The "/aipanel/" subtree dispatches POST
	// /aipanel/consent/accept and /aipanel/consent/cancel to the chi
	// router; without it the custom-domain catch-all at "/" shadows both
	// and confirming the consent modal 404s (the wireup gap this issue
	// fixes — same defect class as the SIN-63821 inbox mount above).
	"/aipanel/",
	// SIN-64975 — HTMX branding admin surface (Fase 5, SIN-63084).
	// The handler was registered inside the chi authed/tenanted group
	// (router.go) but never added here, so the stdlib mux dispatched
	// every /branding* request to the custom-domain catch-all at "/"
	// instead of chi — staging returned 404 for an otherwise
	// fully-wired, non-nil handler. The "/branding" exact pattern
	// matches GET /branding; the "/branding/" subtree catches the four
	// POSTs (logo, palette/override, palette/save, palette/revert).
	// Identical defect + fix to the SIN-63821 inbox mount above.
	"/branding",
	"/branding/",
	// SIN-65576 (fix SIN-65583) — SIN-65008 managerial dashboard /
	// relatórios surface. The handler is registered inside the chi
	// authed/tenanted group (router.go: GET /dashboard + GET
	// /dashboard/export.csv, guarded by deps.WebDashboard != nil) and is
	// non-nil at runtime, so the /hello-tenant tile renders — but the
	// prefixes were never added here, so the stdlib mux dispatched every
	// /dashboard* request to the custom-domain catch-all at "/" instead
	// of chi and staging returned a raw 404. The "/dashboard" exact
	// pattern matches GET /dashboard; the "/dashboard/" subtree catches
	// GET /dashboard/export.csv. Identical defect + fix to the SIN-64975
	// branding and SIN-64973 ai-policy mounts above.
	"/dashboard",
	"/dashboard/",
	"/m/",
	// SIN-63957 — master tenants + grants surface (Fase 2.5 C9/C10 +
	// SIN-63605 + SIN-63958 impersonation). The "/master/" subtree
	// pattern catches every /master/tenants/* and /master/grants/*
	// path mounted by httpapi.NewRouter via Deps.MasterTenants +
	// Deps.Impersonation. Without this entry the custom-domain
	// catch-all at "/" shadows the entire master subtree and every
	// /master/* request returns 404 even with deps slots populated —
	// the same wireup gap memory reference_crm_router_nil_dep_silent_
	// skip warns about, applied at the public-mux dispatch layer.
	"/master/",
	"/metrics",
}

// iamHandlerOpts bundles the optional dependencies the chi router needs
// to mount feature-specific routes inside its authed group. Each field is
// nil-safe so cmd/server can degrade individual subsystems independently
// (DB unavailable, env var missing, etc.) without losing the rest of the
// public surface.
type iamHandlerOpts struct {
	// WebContacts is the SIN-62855 HTMX identity-split mux. Nil keeps
	// the /contacts/{contactID} + /contacts/identity/split routes
	// unmounted; the chi router emits 404 for those paths.
	WebContacts http.Handler

	// WebFunnel is the SIN-62862 HTMX funnel board mux. Nil keeps the
	// four /funnel* routes unmounted (chi emits 404) so cmd/server boots
	// cleanly when DATABASE_URL is unset.
	WebFunnel http.Handler

	// WebPrivacy is the SIN-62354 HTMX privacy / DPA disclosure mux.
	// Nil keeps the two /settings/privacy* routes unmounted; the wire
	// in privacy_wire.go takes no DB dependency so this is non-nil
	// whenever the privacy_wire factory succeeded.
	WebPrivacy http.Handler

	// WebAIPolicy is the SIN-62906 HTMX AI policy admin UI mux. Nil
	// keeps the /settings/ai-policy* routes unmounted; the wire in
	// ai_policy_wire.go owns its own pgxpool and returns nil when the
	// DB / aipolicy store cannot be built.
	WebAIPolicy http.Handler

	// WebCatalog is the SIN-62907 HTMX catalog admin UI mux. Nil keeps
	// the /catalog* routes unmounted; the wire in catalog_wire.go
	// owns its runtime + master_ops pgxpools and returns nil when
	// either DSN is missing or the connection fails.
	WebCatalog http.Handler

	// WebCampaigns is the SIN-62962 HTMX campaign dashboard mux.
	// Nil keeps the /campaigns* routes unmounted; the wire in
	// campaigns_wire.go owns its own pgxpool and returns nil when
	// DATABASE_URL is missing.
	WebCampaigns http.Handler

	// WebFunnelRules is the SIN-62961 HTMX funnel-rules editor mux.
	// Nil keeps the /funnel/rules* routes unmounted; the wire in
	// funnelrules_wire.go owns its own pgxpool and returns nil when
	// DATABASE_URL is missing.
	WebFunnelRules http.Handler

	// WebCampaignPublic is the SIN-62959 GET /c/{slug} handler
	// pre-wrapped with its per-IP rate limit by
	// campaigns_public_wire.go. Nil keeps the route unmounted (e.g.
	// when DATABASE_URL / REDIS_URL is unset). Mounted inside the
	// tenanted group but outside the authed sub-group — the redirect
	// is unauthenticated by design (AC #1).
	WebCampaignPublic http.Handler

	// WebChat is the SIN-64972 webchat widget handler (ADR-0021),
	// pre-assembled with the ReceiveInbound stack + per-tenant origin
	// allowlist / signature / rate limiter by webchat_wire.go. Built
	// inside buildIAMHandler so it reuses the IAM pool. A non-nil value
	// supplied by the caller wins over the wire-built handler so tests
	// can inject a stub. Nil keeps /widget/v1/* unmounted.
	WebChat http.Handler

	// WebBranding is the SIN-63084 HTMX branding admin mux. Nil keeps
	// the /branding* routes unmounted; the wire in branding_ui_wire.go
	// has no DB dependency and only returns nil on a programmer error
	// in webbranding.New.
	WebBranding http.Handler

	// WebLGPD carries the SIN-63186 admin handlers + lgpd_admin rate
	// limit produced by buildLGPDStack. Built inside buildIAMHandler
	// (like WebCampaignPublic) so it reuses the IAM pool + Redis; the
	// caller-supplied opts value, when its Export / Delete slots are
	// already set, wins so tests can inject stubs without standing up
	// the postgres + redis stack.
	WebLGPD httpapi.LGPDRoutes

	// WebPublicPrivacy is the SIN-63191 public LGPD disclosure page
	// handler. Nil keeps GET /privacy unmounted. Built by the wire
	// layer in privacy_public_wire.go on top of the postgres tenant
	// resolver; the slot here lets cmd/server unit tests inject a
	// stub without standing up the DB.
	WebPublicPrivacy http.Handler

	// WebConsent is the SIN-63191 cookie consent banner handler. Nil
	// keeps GET /consent/cookies-banner and POST /consent/cookies
	// unmounted. Built by the wire layer in consent_wire.go.
	WebConsent http.Handler

	// WebBillingInvoices is the SIN-62963 HTMX PIX-invoice surface mux.
	// Nil keeps the /billing/invoices* + /billing/dunning-banner routes
	// unmounted; the wire in billing_invoices_wire.go owns its runtime
	// + master_ops pgxpools and returns nil when either DSN is missing
	// or a connection fails. SIN-64974 added this slot — the surface was
	// shipped but never plumbed, leaving the routes 404 in staging.
	WebBillingInvoices http.Handler

	// WebInbox is the SIN-63821 operator inbox HTMX UI handler.
	// Built by inbox_wire.go with stub use cases in W1 so the
	// routes render the empty-inbox shell while the real channel
	// adapter + WalletDebitor land in W2/W4/W5. Nil keeps every
	// /inbox/* route unmounted; cmd/server tests that don't
	// exercise the surface keep their pre-PR behaviour.
	WebInbox http.Handler

	// WebAIPanel is the SIN-65364 LGPD consent accept/cancel handler
	// (internal/web/aipanel) the inbox AI-assist gate's modal POSTs to.
	// Nil keeps the /aipanel/* routes unmounted; built by
	// aipanel_wire.go with fail-soft semantics when DATABASE_URL is unset.
	WebAIPanel http.Handler

	// WebDashboard is the SIN-65008 managerial dashboard HTMX UI handler
	// built by dashboard_wire.go on top of the SIN-65007 metrics read-
	// model use case. Nil keeps the /dashboard + /dashboard/export.csv
	// routes unmounted (chi emits 404); cmd/server tests that don't
	// exercise the surface keep their pre-PR behaviour.
	WebDashboard http.Handler

	// WebWallet is the SIN-63942 / UX-F5 gerente wallet UI handler
	// built by walletui_wire.go on top of the SIN-63954 read-side
	// ports + postgres adapter. Nil keeps every /wallet* route
	// unmounted (chi emits 404); cmd/server tests that don't
	// exercise the surface keep their pre-PR behaviour.
	WebWallet http.Handler

	// Theme is the SIN-63085 per-tenant theme middleware, built by
	// branding_ui_wire.go on top of the same PaletteStore that backs
	// the WebBranding handler. Mounted by httpapi.NewRouter inside
	// the tenanted group so every authenticated render sees the
	// resolved palette via branding.ThemeStyleFromContext. Nil keeps
	// the chain unchanged — pages fall back to DefaultThemeStyle and
	// the AC #4 cache-invalidation seam is a no-op.
	Theme *middleware.ThemeMiddleware

	// WebUserMFA carries the SIN-63361 user-side TOTP handlers
	// (LoginPost + Setup + Verify + Regenerate) built by
	// buildUserMFAStack. The caller-supplied opts value wins when
	// LoginPost is non-nil — the same pattern as opts.WebLGPD — so
	// cmd/server unit tests can inject stub handlers without
	// standing up the seed cipher / postgres stack. When all slots
	// are nil the legacy password-only POST /login handler stays
	// mounted and the /admin/2fa routes remain unmounted.
	WebUserMFA httpapi.UserMFARoutes

	// Metrics is the process-wide obs.Metrics constructed at boot.
	// SIN-63105 threads it through httpapi.Deps.Metrics so the
	// /metrics scrape endpoint is mounted and the per-route
	// HTTPMetrics middleware records http_requests_total /
	// http_request_duration_seconds. The same pointer also backs
	// the SIN-63085 theme middleware's ObserveThemeCacheLookup hook
	// (wired via buildBrandingStack), so tenant_theme_cache_hits_total
	// is observable on the same scrape. Nil keeps the router behaving
	// as it did pre-SIN-63105 — /metrics returns 404 and the counters
	// stay silent.
	Metrics *obs.Metrics

	// CustomDomainEnabled mirrors the SIN-62259 boot-time decision in
	// buildCustomDomainHandler. The actual `/tenant/custom-domains*`
	// route subtree lives on the public mux (mounted in main.go via
	// mux.Handle("/", cdHandler)), but the SIN-63940 /hello-tenant
	// landing surfaces the link too; this flag is the only signal the
	// IAM router needs to flip the card from disabled to live. main.go
	// derives the value from `getenv("CUSTOM_DOMAIN_UI_ENABLED") == "1"`
	// because buildIAMHandler runs BEFORE buildCustomDomainHandler in
	// the boot sequence and cannot inspect the resulting handler.
	CustomDomainEnabled bool

	// Impersonation carries the SIN-63958 session-bound impersonation
	// bundle built by buildImpersonationStack. The caller-supplied opts
	// value wins when Start is non-nil so cmd/server unit tests can
	// inject stubs. When all slots are nil (default) the router skips
	// the impersonation routes cleanly.
	Impersonation httpapi.ImpersonationRoutes

	// MasterTenants carries the SIN-63957 master tenants + grants +
	// grant-requests bundle built by buildMasterTenantsStack. The
	// caller-supplied opts value wins when List is non-nil so
	// cmd/server unit tests can inject stubs (matches the WebLGPD /
	// UserMFA / Impersonation pattern). When all slots are nil the
	// router skips every /master/tenants/* and /master/grants* route
	// cleanly per reference_crm_router_nil_dep_silent_skip.
	MasterTenants httpapi.MasterTenantsRoutes
}

// buildIAMHandler assembles the IAM deps and returns the chi handler plus a
// cleanup function. Returns (nil, no-op) when DATABASE_URL or REDIS_URL is
// unset. The caller MUST defer the cleanup to release pool + Redis.
//
// opts carries optional handlers mounted inside the authed group (see
// iamHandlerOpts). Pass the zero value when no extras are wired — the
// router still serves /login, /logout, /hello-tenant unchanged.
func buildIAMHandler(ctx context.Context, getenv func(string) string, opts iamHandlerOpts) (http.Handler, func()) {
	noop := func() {}
	dsn := getenv(postgresadapter.EnvDSN)
	redisURL := getenv(envRedisURL)
	if dsn == "" || redisURL == "" {
		log.Printf("crm: IAM handler disabled (DATABASE_URL/REDIS_URL unset)")
		return nil, noop
	}

	pool, err := postgresadapter.NewFromEnv(ctx, getenv)
	if err != nil {
		log.Printf("crm: IAM handler disabled — pg connect: %v", err)
		return nil, noop
	}

	rdb, cleanup, err := openIAMRedis(ctx, redisURL)
	if err != nil {
		pool.Close()
		log.Printf("crm: IAM handler disabled — redis: %v", err)
		return nil, noop
	}

	policies, err := ratelimit.DefaultPolicies()
	if err != nil {
		pool.Close()
		cleanup()
		log.Printf("crm: IAM handler disabled — rate-limit policies: %v", err)
		return nil, noop
	}
	limiter := rlredis.New(rdb, "auth:rl:")
	notifier := slackadapter.New(getenv(envSlackWebhook))

	tenants, err := postgresadapter.NewTenantResolver(pool)
	if err != nil {
		pool.Close()
		cleanup()
		log.Printf("crm: IAM handler disabled — tenant resolver: %v", err)
		return nil, noop
	}

	users := postgresadapter.NewUserCredentialReader(pool)
	sessions := postgresadapter.NewSessionStore(pool)
	logger := slog.Default()

	// Single iamAdapter shared across the user-MFA stack, the router's
	// IAM slot, and the master login fn (SIN-65223). iamSvc.Login is the
	// per-request tenant-scoped iam.Service.Login the master /m/login
	// handler delegates the credential check to — its signature matches
	// mastermfa.MasterLoginFunc verbatim, so it assigns straight into
	// buildMasterMFAStack's masterLogin parameter.
	iamSvc := iamAdapter{
		tenants:  tenants,
		users:    users,
		sessions: sessions,
		logger:   logger,
		limiter:  limiter,
		policies: policies,
		pool:     pool,
	}

	// SIN-62765 — wrap the RBAC inner authorizer with the audit
	// decorator so every recorded Decision lands in audit_log_security
	// + the authz_* Prometheus counters. Failure to build the wrapper
	// is fatal at boot: F10 is a security-bar finding and silently
	// running without audit coverage is worse than refusing to serve.
	audited, err := newAuditedAuthorizer(pool, prometheus.DefaultRegisterer, getenv, logger)
	if err != nil {
		pool.Close()
		cleanup()
		log.Printf("crm: IAM handler disabled — authz audit wrap: %v", err)
		return nil, noop
	}

	// SIN-63214 — split audit logger for router-mounted handlers that
	// need to emit a SecurityEvent* row (currently the tenant POST
	// /logout, added in PR #234 / SIN-63188). Same pool + same writer
	// shape the audited authorizer already uses; instantiated separately
	// because the audited wrap keeps its own splitLogger encapsulated.
	// A constructor failure is fatal at boot for the same reason the
	// authz audit wrap is — silently running without the logout audit
	// row would degrade the security ledger.
	logoutAudit, err := postgresadapter.NewSplitAuditLogger(pool)
	if err != nil {
		pool.Close()
		cleanup()
		log.Printf("crm: IAM handler disabled — logout audit logger: %v", err)
		return nil, noop
	}

	// SIN-62959 — public campaign redirect endpoint. Built here so it
	// reuses the IAM pool + Redis (no second pgxpool / goredis client
	// opened). A failure to build the handler is non-fatal — the
	// router simply omits GET /c/{slug} and the rest of IAM keeps
	// serving. opts.WebCampaignPublic, when set by the caller, wins
	// over the wire-built handler (tests rely on this).
	webCampaignPublic := opts.WebCampaignPublic
	if webCampaignPublic == nil {
		if h, err := buildWebCampaignHandler(pool, rdb, getenv); err != nil {
			log.Printf("crm: campaigns/public handler disabled — %v", err)
		} else {
			webCampaignPublic = h
		}
	}

	// SIN-64972 — public webchat widget surface (ADR-0021). Built here
	// so it reuses the IAM pool (no second pgxpool). A build failure is
	// non-fatal — the router simply omits /widget/v1/* and the rest of
	// IAM keeps serving. opts.WebChat, when set by the caller, wins over
	// the wire-built handler (tests rely on this).
	webChat := opts.WebChat
	if webChat == nil {
		if h, err := buildWebchatHandler(pool, getenv); err != nil {
			log.Printf("crm: webchat widget handler disabled — %v", err)
		} else {
			webChat = h
		}
	}

	// SIN-63186 — LGPD admin handlers + lgpd_admin rate limit. Built
	// here so it reuses the IAM pool + Redis; opens a second pgxpool
	// against MASTER_OPS_DATABASE_URL for the store constructor (the
	// web handlers themselves only hit the runtime pool, but the store
	// constructor takes both pools so the same store can back the
	// retention worker without a second adapter). opts.WebLGPD, when
	// the caller has wired Export / Delete, wins over the wire-built
	// stack so tests can inject stubs.
	lgpdRoutes := opts.WebLGPD
	lgpdCleanup := func() {}
	if lgpdRoutes.Export == nil || lgpdRoutes.Delete == nil {
		stack := buildLGPDStack(ctx, pool, rdb, getenv)
		lgpdRoutes = stack.Routes
		lgpdCleanup = stack.Cleanup
	}

	// SIN-63361 — user-side TOTP. Built here so it reuses the IAM
	// pool + the iamAdapter that already implements the
	// LoginAuthenticator slice the usermfa wrapper needs. Same wire
	// rule as WebLGPD: the caller-supplied opts.WebUserMFA wins
	// when LoginPost is non-nil so cmd/server unit tests can inject
	// stubs without standing up the seed cipher. Failure inside
	// buildUserMFAStack returns the noop stack — POST /login then
	// falls back to the password-only handler.LoginPost and the
	// /admin/2fa routes stay unmounted.
	userMFARoutes := opts.WebUserMFA
	userMFACleanup := func() {}
	if userMFARoutes.LoginPost == nil {
		stack := buildUserMFAStack(ctx, pool, iamSvc, logoutAudit, getenv)
		userMFARoutes = stack.Routes
		userMFACleanup = stack.Cleanup
	}

	// SIN-63958 — session-bound impersonation envelope. Built here so it
	// reuses the IAM pool + tenant resolver. opts.Impersonation, when Start
	// is non-nil, wins over the wire-built stack (same pattern as
	// opts.WebLGPD) so unit tests can inject stubs.
	impersonationRoutes := opts.Impersonation
	impersonationCleanup := func() {}
	if impersonationRoutes.Start == nil {
		stack := buildImpersonationStack(ctx, pool, tenants, getenv)
		impersonationRoutes = stack.Routes
		impersonationCleanup = stack.Cleanup
	}

	// SIN-63957 — master /master/tenants + /master/grants surface.
	// Built here so it reuses the IAM runtime pool + the same
	// SplitAuditLogger backing logoutAudit (master.grant.issued events
	// land on the same audit chain). opts.MasterTenants wins when List
	// is non-nil so cmd/server unit tests can inject stubs without
	// standing up the master_ops DB.
	masterTenantsRoutes := opts.MasterTenants
	masterTenantsCleanup := func() {}
	if masterTenantsRoutes.List == nil {
		stack := buildMasterTenantsStack(ctx, pool, logoutAudit, getenv, logger, tenants)
		masterTenantsRoutes = stack.Routes
		masterTenantsCleanup = stack.Cleanup
	}

	// SIN-65254 — master-operator login. The master surface is tenant-less:
	// the seeded operator (master@crm.local) is is_master=true /
	// tenant_id=NULL, so the previous wire (iamSvc.Login, tenant-scoped)
	// could never resolve it and /m/login returned 401 for every operator.
	// buildMasterLogin resolves the GLOBAL operator by
	// (email, is_master=true, tenant_id IS NULL) over the app_master_ops
	// pool (MASTER_OPS_DATABASE_URL) with the m_login lockout + Slack alert
	// posture (ADR 0074 §6). It ALSO returns that master-ops pool: the rest
	// of the master /m/* stack (sessions, directory, seed repo) runs under
	// WithMasterOps, which the tenant-scoped IAM runtime pool (app_runtime)
	// cannot satisfy — so the whole stack must be built on the master-ops
	// pool, not `pool`. Nil login fn / nil pool → buildMasterMFAStack noop.
	masterLoginFn, masterOpsPool, masterLoginCleanup := buildMasterLogin(ctx, limiter, policies, notifier, logger, getenv)

	// SIN-65223 (Child B) — master /m/* console deps. Child A
	// (buildMasterMFAStack) assembles the concrete adapters into the
	// mastermfa ports; buildMasterDeps turns that stack into the five
	// handlers + two middlewares httpapi.MasterDeps needs. The logout
	// handler is handed logoutAudit — the SAME audit.SplitLogger the tenant
	// POST /logout uses — so a master logout appends a SecurityEventLogout
	// (audience="master") row to the security ledger (closes SIN-63216
	// AC #1). A missing master seed key / actor id / DSN (or any nil input)
	// yields the noop stack → buildMasterDeps returns the zero MasterDeps
	// and router.go leaves /m/* unmounted, the same fail-soft contract as
	// the surfaces above.
	masterMFA := buildMasterMFAStack(ctx, masterOpsPool, masterLoginFn, logoutAudit, getenv)
	masterDeps, masterDeniedAuditor := buildMasterDeps(masterMFA, logoutAudit, logger, masterConsoleHost(getenv))

	// SIN-65277 — master CSRF rejection counter, registered on the shared
	// Prometheus registry the /metrics endpoint scrapes. The CounterVec is
	// discarded here (the gate only needs the OnReject closure); it stays
	// reachable through the registry for scraping.
	_, masterCSRFOnReject := newMasterCSRFRejectMetric(prometheus.DefaultRegisterer)

	routerDeps := httpapi.Deps{
		IAM:            iamSvc,
		TenantResolver: tenants,
		// SIN-63963 / UX-F4 — the TenantResolver also implements
		// tenancy.BrandingReader (LoadBranding), so the same adapter feeds
		// the pre-auth /login white-label surface.
		LoginBranding: tenants,
		Logger:        logger,
		// SIN-65076 — operator-console hostname. Empty when
		// MASTER_CONSOLE_HOST is unset (graceful: CSRF allowlist falls
		// back to the tenant host alone). Previously never assigned, so
		// the master console host was unreachable in every deploy.
		MasterHost: masterConsoleHost(getenv),
		// SIN-63146 — surface the build SHA on /health so cd-stg can
		// detect a stale `docker compose pull`. The value comes from
		// the -ldflags="-X .../internal/version.commitSHA=…" injected
		// by Dockerfile + cd-stg.yml; without an ldflag it returns
		// "unknown".
		CommitSHA:   version.CommitSHA(),
		Policies:    policies,
		RateLimiter: limiter,
		Authorizer:  audited,
		AuditLogger: logoutAudit,
		// SessionToucher is nil — Activity middleware deferred to batch
		// that lands the session role/last_activity DB columns (0077).
		// SIN-65223 — master /m/* deps. buildMasterDeps returns the zero
		// MasterDeps when the master MFA stack is the noop (DB-less boot,
		// MASTERMFA_SEED_KEY / MASTER_OPS_ACTOR_ID unset), so router.go
		// leaves the /m/* group unmounted in that case (deps.Master.Login
		// nil → 404), exactly as before this child landed.
		Master:                    masterDeps,
		MasterAccessDeniedAuditor: masterDeniedAuditor,
		// SIN-65277 — count RequireMasterOriginCSRF rejections by reason on
		// the shared registry so the existing /metrics scrape exposes
		// master_csrf_rejected_total{reason} alongside authz_decisions_total
		// et al. Previously the gate's OnReject hook was nil, so CSRF
		// rejections were logged but never counted.
		MasterCSRFOnReject:  masterCSRFOnReject,
		WebContacts:         opts.WebContacts,
		WebFunnel:           opts.WebFunnel,
		WebPrivacy:          opts.WebPrivacy,
		WebAIPolicy:         opts.WebAIPolicy,
		WebCatalog:          opts.WebCatalog,
		WebCampaigns:        opts.WebCampaigns,
		WebFunnelRules:      opts.WebFunnelRules,
		WebCampaignPublic:   webCampaignPublic,
		WebChat:             webChat,
		WebBranding:         opts.WebBranding,
		WebLGPD:             lgpdRoutes,
		WebPublicPrivacy:    opts.WebPublicPrivacy,
		WebConsent:          opts.WebConsent,
		WebBillingInvoices:  opts.WebBillingInvoices,
		WebInbox:            opts.WebInbox,
		WebAIPanel:          opts.WebAIPanel,
		WebDashboard:        opts.WebDashboard,
		WebWallet:           opts.WebWallet,
		Theme:               opts.Theme,
		Metrics:             opts.Metrics,
		UserMFA:             userMFARoutes,
		CustomDomainEnabled: opts.CustomDomainEnabled,
		Impersonation:       impersonationRoutes,
		MasterTenants:       masterTenantsRoutes,
	}

	// SIN-64985 — publish the web-surface mounted/not map (booleans only)
	// for /health BEFORE the listener accepts connections, derived from
	// the same Deps that gate the route mounts so the diagnostic cannot
	// drift from reality. Read by healthHandler via surfacesForHealth.
	surfaces := routerDeps.WebSurfaces()
	surfacesForHealth.Store(&surfaces)

	h := httpapi.NewRouter(routerDeps)

	fullCleanup := func() {
		masterLoginCleanup()
		masterTenantsCleanup()
		impersonationCleanup()
		userMFACleanup()
		lgpdCleanup()
		pool.Close()
		cleanup()
	}
	return h, fullCleanup
}

// openIAMRedis parses redisURL, pings, and returns the client + a close func.
func openIAMRedis(ctx context.Context, redisURL string) (*goredis.Client, func(), error) {
	if redisURL == "" {
		return nil, func() {}, errors.New("REDIS_URL is empty")
	}
	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, func() {}, fmt.Errorf("parse REDIS_URL: %w", err)
	}
	client := goredis.NewClient(opts)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, func() {}, fmt.Errorf("redis ping: %w", err)
	}
	return client, func() { _ = client.Close() }, nil
}

// iamAdapter bridges the postgres adapters to httpapi.IAMService.
// Login builds a per-request tenant-scoped iam.Service so each request
// gets a TenantLockouts adapter scoped to that tenant's lockout rows.
type iamAdapter struct {
	tenants  *postgresadapter.TenantResolver
	users    *postgresadapter.UserCredentialReader
	sessions *postgresadapter.SessionStore
	logger   *slog.Logger
	limiter  ratelimit.RateLimiter
	policies map[string]ratelimit.Policy
	pool     *pgxpool.Pool
}

func (a iamAdapter) Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent, route string) (iam.Session, error) {
	tenant, err := tenancy.FromContext(ctx)
	if err != nil {
		return iam.Session{}, fmt.Errorf("cmd/server: tenant from context: %w", err)
	}
	lockouts, err := postgresadapter.NewTenantLockouts(a.pool, tenant.ID)
	if err != nil {
		return iam.Session{}, fmt.Errorf("cmd/server: tenant lockouts: %w", err)
	}
	svc := &iam.Service{
		Tenants:     iamTenantResolver{inner: a.tenants},
		Users:       a.users,
		Sessions:    a.sessions,
		Logger:      a.logger,
		Lockouts:    lockouts,
		Limiter:     a.limiter,
		LoginPolicy: a.policies["login"],
	}
	return svc.Login(ctx, host, email, password, ipAddr, userAgent, route)
}

// LoadBranding lets iamAdapter double as a tenancy.BrandingReader so the
// usermfa wire layer can pick it up via an optional interface assertion
// (SIN-63963 / UX-F4) without growing buildUserMFAStack's signature. It
// delegates to the shared TenantResolver, which owns the single-row
// branding lookup.
func (a iamAdapter) LoadBranding(ctx context.Context, tenantID uuid.UUID) (tenancy.TenantBranding, error) {
	return a.tenants.LoadBranding(ctx, tenantID)
}

func (a iamAdapter) Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	svc := &iam.Service{
		Tenants:  iamTenantResolver{inner: a.tenants},
		Users:    a.users,
		Sessions: a.sessions,
		Logger:   a.logger,
	}
	return svc.Logout(ctx, tenantID, sessionID)
}

func (a iamAdapter) ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (iam.Session, error) {
	svc := &iam.Service{
		Tenants:  iamTenantResolver{inner: a.tenants},
		Users:    a.users,
		Sessions: a.sessions,
		Logger:   a.logger,
	}
	return svc.ValidateSession(ctx, tenantID, sessionID)
}

// iamTenantResolver bridges tenancy.Resolver to iam.TenantResolver.
type iamTenantResolver struct {
	inner *postgresadapter.TenantResolver
}

func (r iamTenantResolver) ResolveByHost(ctx context.Context, host string) (uuid.UUID, error) {
	t, err := r.inner.ResolveByHost(ctx, host)
	if err != nil {
		if errors.Is(err, tenancy.ErrTenantNotFound) {
			return uuid.Nil, iam.ErrTenantNotFound
		}
		return uuid.Nil, err
	}
	return t.ID, nil
}
