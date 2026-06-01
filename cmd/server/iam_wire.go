package main

// SIN-62527 / SIN-62217 + SIN-62348 wiring — chi router (IAM routes).
//
// buildIAMHandler assembles the production dependency chain
// (Postgres + Redis + IAM) and returns the chi router as an http.Handler.
// The returned handler serves /login, /logout, /hello-tenant, and /m/*.
//
// MasterDeps is intentionally left empty (zero value) in this batch;
// /m/* routes are enabled when the mastermfa batch (SIN-62526 / batch 17)
// lands and provides RequireMasterAuth + RequireMasterMFA.
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
var iamRoutes = []string{
	"/login",
	"/logout",
	"/hello-tenant",
	"/contacts/",
	"/funnel",
	"/funnel/",
	"/settings/privacy",
	"/settings/privacy/dpa.md",
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

	// WebInbox is the SIN-63821 operator inbox HTMX UI handler.
	// Built by inbox_wire.go with stub use cases in W1 so the
	// routes render the empty-inbox shell while the real channel
	// adapter + WalletDebitor land in W2/W4/W5. Nil keeps every
	// /inbox/* route unmounted; cmd/server tests that don't
	// exercise the surface keep their pre-PR behaviour.
	WebInbox http.Handler

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
	_ = notifier // available for future alert wiring

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
		stack := buildUserMFAStack(ctx, pool, iamAdapter{
			tenants:  tenants,
			users:    users,
			sessions: sessions,
			logger:   logger,
			limiter:  limiter,
			policies: policies,
			pool:     pool,
		}, logoutAudit, getenv)
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

	h := httpapi.NewRouter(httpapi.Deps{
		IAM: iamAdapter{
			tenants:  tenants,
			users:    users,
			sessions: sessions,
			logger:   logger,
			limiter:  limiter,
			policies: policies,
			pool:     pool,
		},
		TenantResolver: tenants,
		Logger:         logger,
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
		// Master MFA deps deferred to batch 17 (SIN-62526).
		WebContacts:         opts.WebContacts,
		WebFunnel:           opts.WebFunnel,
		WebPrivacy:          opts.WebPrivacy,
		WebAIPolicy:         opts.WebAIPolicy,
		WebCatalog:          opts.WebCatalog,
		WebCampaigns:        opts.WebCampaigns,
		WebFunnelRules:      opts.WebFunnelRules,
		WebCampaignPublic:   webCampaignPublic,
		WebBranding:         opts.WebBranding,
		WebLGPD:             lgpdRoutes,
		WebPublicPrivacy:    opts.WebPublicPrivacy,
		WebConsent:          opts.WebConsent,
		WebInbox:            opts.WebInbox,
		Theme:               opts.Theme,
		Metrics:             opts.Metrics,
		UserMFA:             userMFARoutes,
		CustomDomainEnabled: opts.CustomDomainEnabled,
		Impersonation:       impersonationRoutes,
		MasterTenants:       masterTenantsRoutes,
	})

	fullCleanup := func() {
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
