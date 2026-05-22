package main

// SIN-62259 + SIN-62313 wiring — public-side custom-domain management UI.
//
// The handler is wired only when CUSTOM_DOMAIN_UI_ENABLED=1 AND
// DATABASE_URL + CUSTOM_DOMAIN_CSRF_SECRET are configured. When any of
// those are unset the handler is omitted; the existing /health route
// remains the only public surface (preserving cmd/server tests that
// run without external deps).
//
// DNS validation (SIN-62313): the management use-case is constructed
// with the SIN-62242 validator (`internal/customdomain/validation`)
// backed by the production miekg adapter (`adapters/dnsresolver/miekg`).
// CUSTOMDOMAIN_DNS_SERVER selects the recursive resolver — production
// MUST point this at the Unbound sidecar configured in
// `infra/caddy/unbound.conf` (typically `unbound:5353` inside the
// compose network) so DNS rebinding into private/loopback ranges is
// rejected at both the validator and the recursive cache (ADR 0079 §2).
// CUSTOMDOMAIN_DNSSEC=1 (default on) enables DNSSEC observability so
// the `verified_with_dnssec` badge in the UI reflects whether the
// resolver returned the AD bit on the answer.
//
// Gating on missing DNS — fail-fast at startup. If the feature flag is
// on but the resolver cannot be constructed (currently the only failure
// mode is a misconfigured server address surfaced when LookupIP is first
// called), the wire-up still installs the handler with a working
// validator — the resolver itself does not perform a startup probe to
// avoid blocking boot on transient DNS outages. The `Verificar agora`
// button is therefore always rendered when the feature flag is on; if
// the resolver is unreachable at request time the UI surfaces the
// resolver error inline as a row tooltip ("dns_resolution_failed"),
// which is the same UX the user sees when their own DNS publishing is
// broken. The alternative — disabling the button when the resolver is
// nil — is unreachable because the resolver is now always constructed
// when the flag is on; we keep the management.Config{DNS: nil} branch
// for tests that intentionally simulate the unwired state.
//
// The enrollment-quota / circuit-breaker gate is wired to Redis-backed
// adapters (SIN-62334 F53). When CUSTOM_DOMAIN_UI_ENABLED=1 and
// REDIS_URL is unset, the wire-up returns a hard error — boot must
// fail rather than serve customer traffic with the per-tenant quota
// disabled, because the rate-limiter on /internal/tls/ask (3/min/host)
// would otherwise be the only brake against LE quota exhaustion at
// scale. The dev/CI path (flag off) still boots without Redis.
//
// Tenant identity is sourced from a request header (`X-Tenant-ID`)
// while session/cookie auth is owned by a separate ticket. The header
// path is gated by the same feature flag and is logged on every
// request — production deploys MUST replace it with real auth before
// flipping the flag.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"

	miekgresolver "github.com/pericles-luz/crm/adapters/dnsresolver/miekg"
	pgpool "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	customdomainhttp "github.com/pericles-luz/crm/internal/adapter/transport/http/customdomain"
	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker"
	"github.com/pericles-luz/crm/internal/customdomain/circuitbreaker/redisstate"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment"
	"github.com/pericles-luz/crm/internal/customdomain/enrollment/rediswindow"
	"github.com/pericles-luz/crm/internal/customdomain/management"
	"github.com/pericles-luz/crm/internal/customdomain/validation"
	"github.com/pericles-luz/crm/internal/iam/dnsresolver"
	"github.com/pericles-luz/crm/internal/slugreservation"
)

const (
	envCustomDomainUI       = "CUSTOM_DOMAIN_UI_ENABLED"
	envCustomDomainCSRF     = "CUSTOM_DOMAIN_CSRF_SECRET"
	envCustomDomainPrimary  = "PRIMARY_DOMAIN"
	envCustomDomainTenantHd = "X-Tenant-ID"
	envCustomDomainDNSSrv   = "CUSTOMDOMAIN_DNS_SERVER"
	envCustomDomainDNSSEC   = "CUSTOMDOMAIN_DNSSEC"
	envCustomDomainRedisURL = "REDIS_URL"
	enrollmentRedisPrefix   = "customdomain:enrollment"
	breakerRedisPrefix      = "customdomain:lebreaker"
)

// ErrEnrollmentRedisRequired is returned when CUSTOM_DOMAIN_UI_ENABLED=1
// is set but REDIS_URL is missing (or the dial fails). The caller
// (cmd/server main) MUST treat this as a hard boot failure: shipping
// the binary with the placeholders gone but the Redis adapter not
// configured would silently disable the per-tenant enrollment quota
// and the LE circuit breaker, leaving only the 3/min/host rate-limit
// on /internal/tls/ask as a brake against LE quota exhaustion at
// scale (SIN-62334 F53 / ADR 0079 §"Production gates").
var ErrEnrollmentRedisRequired = errors.New("customdomain: CUSTOM_DOMAIN_UI_ENABLED=1 requires REDIS_URL")

// customDomainDial is the test seam: the production wiring opens a real
// pgxpool; tests inject a fake pool that satisfies pgstore.PgxRowsConn.
type customDomainDial func(ctx context.Context, dsn string) (customDomainPool, error)

// customDomainResolverFactory is the test seam for the SIN-62242
// resolver. Production returns a *miekg.Resolver bound to the configured
// recursive server; tests inject a deterministic in-memory fake.
type customDomainResolverFactory func(getenv func(string) string) dnsresolver.Resolver

// customDomainRedis is the narrow Redis surface buildEnrollmentGate
// needs. *goredis.Client satisfies it; tests inject an in-memory fake
// matching the same Eval semantics as the unit-test fakes for
// rediswindow / redisstate.
type customDomainRedis interface {
	Eval(ctx context.Context, script string, keys []string, args ...any) *goredis.Cmd
	Close() error
}

// customDomainRedisDial is the test seam for opening the Redis client.
// Production parses REDIS_URL and pings; tests inject a stub that
// returns a fake client without touching the network.
type customDomainRedisDial func(ctx context.Context, redisURL string) (customDomainRedis, error)

// defaultCustomDomainRedisDial is the production Redis dialer. Same
// shape as cmd/server's defaultDial — ParseURL + NewClient + Ping. A
// failed Ping is treated as "Redis not configured" and surfaces as
// ErrEnrollmentRedisRequired upstream so boot fails closed.
func defaultCustomDomainRedisDial(ctx context.Context, redisURL string) (customDomainRedis, error) {
	opt, err := goredis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("redis url: %w", err)
	}
	rdb := goredis.NewClient(opt)
	if err := rdb.Ping(ctx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("redis ping: %w", err)
	}
	return rdb, nil
}

// defaultCustomDomainResolverFactory returns a miekg-backed resolver
// pointed at CUSTOMDOMAIN_DNS_SERVER when set, falling back to the
// stdlib resolver address. Production deploys MUST set the env var to
// the Unbound sidecar (typically `unbound:5353`) — see ADR 0079 §2.
func defaultCustomDomainResolverFactory(getenv func(string) string) dnsresolver.Resolver {
	server := getenv(envCustomDomainDNSSrv)
	dnssec := true
	if v := getenv(envCustomDomainDNSSEC); v != "" {
		if parsed, err := strconv.ParseBool(v); err == nil {
			dnssec = parsed
		}
	}
	return miekgresolver.NewResolver(miekgresolver.Config{
		Server:       server,
		EnableDNSSEC: dnssec,
	})
}

// customDomainPool is the pgxpool-shaped surface buildCustomDomainHandler
// needs. *pgxpool.Pool satisfies it; tests pass an in-memory implementation
// from custom_domain_store_test.go.
type customDomainPool interface {
	pgstore.PgxRowsConn
	Close()
}

// defaultCustomDomainDial is the production dialer. *pgxpool.Pool
// implements every method on customDomainPool (QueryRow, Exec, Query,
// Close), so the wrapper is just the type assertion.
func defaultCustomDomainDial(ctx context.Context, dsn string) (customDomainPool, error) {
	pool, err := pgpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return pool, nil
}

// EnrollmentRedisRequired enforces the SIN-62334 F53 hard-error gate.
// Call from cmd/server BEFORE buildCustomDomainHandler so a
// misconfigured production deploy fails boot rather than serving
// traffic with the per-tenant quota and LE breaker disabled.
//
// Returns ErrEnrollmentRedisRequired when CUSTOM_DOMAIN_UI_ENABLED=1
// AND REDIS_URL is unset; nil otherwise. The flag-off path bypasses
// the check so dev/CI environments without Redis still boot the
// public listener.
func EnrollmentRedisRequired(getenv func(string) string) error {
	if getenv(envCustomDomainUI) != "1" {
		return nil
	}
	if getenv(envCustomDomainRedisURL) == "" {
		return ErrEnrollmentRedisRequired
	}
	return nil
}

// buildCustomDomainHandler returns the registered http.Handler for the
// SIN-62259 routes plus a cleanup func. Returns (nil, no-op) when the
// feature is disabled or required deps cannot be reached, so the
// public listener stays fully functional for the existing /health
// route.
//
// SIN-62334 F53: when REDIS_URL is set the gate is wired against the
// real Redis-backed adapters (rediswindow + redisstate +
// pgstore.EnrollmentCountStore). When REDIS_URL is unset the gate
// falls back to in-memory placeholders — this path is unreachable in
// production because cmd/server calls EnrollmentRedisRequired first
// and exits non-zero on its error. Tests that drive the wire-up
// without a Redis stub still hit the placeholder path.
func buildCustomDomainHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
	return buildCustomDomainHandlerWithRedis(ctx, getenv, defaultCustomDomainDial, defaultCustomDomainResolverFactory, defaultCustomDomainRedisDial)
}

// buildCustomDomainHandlerWith is the test-friendly variant for the
// pgxpool dial seam. Production uses the resolver factory + Redis dial
// baked into buildCustomDomainHandler; tests that need to inject a
// fake DNS resolver or Redis client should call
// buildCustomDomainHandlerWithDeps instead. The signature is preserved
// for backwards compatibility with the SIN-62259 test suite, with the
// production Redis dial wired in.
func buildCustomDomainHandlerWith(ctx context.Context, getenv func(string) string, dial customDomainDial) (http.Handler, func()) {
	return buildCustomDomainHandlerWithRedis(ctx, getenv, dial, defaultCustomDomainResolverFactory, defaultCustomDomainRedisDial)
}

// buildCustomDomainHandlerWithDeps is the test seam for the SIN-62313
// wire-up. The dial / resolverFactory arguments cover the
// network-touching paths inherited from before SIN-62334; passing
// stubs short-circuits pgxpool.New and miekg.NewResolver so handler
// tests can drive the wiring deterministically without Redis. Tests
// that ALSO want to stub Redis dialing should call
// buildCustomDomainHandlerWithRedis.
func buildCustomDomainHandlerWithDeps(ctx context.Context, getenv func(string) string, dial customDomainDial, resolverFactory customDomainResolverFactory) (http.Handler, func()) {
	return buildCustomDomainHandlerWithRedis(ctx, getenv, dial, resolverFactory, defaultCustomDomainRedisDial)
}

// buildCustomDomainHandlerWithRedis is the SIN-62334 test seam: same as
// buildCustomDomainHandlerWithDeps plus a redisDial seam so unit tests
// can drive the wire-up against an in-memory fake Redis client.
func buildCustomDomainHandlerWithRedis(ctx context.Context, getenv func(string) string, dial customDomainDial, resolverFactory customDomainResolverFactory, redisDial customDomainRedisDial) (http.Handler, func()) {
	noop := func() {}
	if v := getenv(envCustomDomainUI); v != "1" {
		return nil, noop
	}
	dsn := getenv(pgpool.EnvDSN)
	if dsn == "" {
		log.Printf("crm: custom-domain UI disabled (DATABASE_URL unset)")
		return nil, noop
	}
	secret := []byte(getenv(envCustomDomainCSRF))
	if len(secret) < 32 {
		log.Printf("crm: custom-domain UI disabled (CUSTOM_DOMAIN_CSRF_SECRET must be ≥32 bytes)")
		return nil, noop
	}
	primary := getenv(envCustomDomainPrimary)
	if primary == "" {
		primary = "exemplo.com"
	}

	pool, err := dial(ctx, dsn)
	if err != nil {
		log.Printf("crm: custom-domain UI disabled — pg connect: %v", err)
		return nil, noop
	}

	// SIN-62334 F53: dial Redis when REDIS_URL is set; fall back to
	// in-memory placeholders otherwise. The flag-on + REDIS_URL-unset
	// path is unreachable from cmd/server.runWith because main calls
	// EnrollmentRedisRequired first; this branch exists for the test
	// suite that drives buildCustomDomainHandler without Redis.
	var rdb customDomainRedis
	if redisURL := getenv(envCustomDomainRedisURL); redisURL != "" {
		rdb, err = redisDial(ctx, redisURL)
		if err != nil {
			pool.Close()
			log.Printf("crm: custom-domain UI disabled — redis dial: %v", err)
			return nil, noop
		}
	}

	store := pgstore.NewCustomDomainStore(pool)
	gate, gateCleanup := buildEnrollmentGate(pool, rdb)
	slugSvc, _ := slugreservation.NewService(
		pgstore.NewSlugReservationStore(pool),
		pgstore.NewSlugRedirectStore(pool),
		nopAudit{},
		nopSlack{},
		nil,
	)
	// SIN-62313: wire the SIN-62242 validator + miekg adapter so the
	// `Verificar agora` path executes the real DNS-only ownership
	// validation (TXT proof + IP allowlist + DNSSEC observability).
	//
	// SIN-62333: also wire the dns_resolution_log writer so every
	// Validate / ValidateHostOnly call lands one row keyed on
	// (tenant_id, host, decision, reason). Without this, a blocked
	// SSRF attempt is observable only in slog and IR cannot
	// reconstruct the timeline (OWASP A09).
	dnsLogStore := pgstore.NewDNSResolutionLogStore(pool)
	validator := validation.New(
		resolverFactory(getenv),
		validationAudit{logger: slog.Default()},
		validation.SystemClock{},
		validation.WithWriter(dnsLogStore),
	)
	uc, err := management.New(management.Config{
		Store:     store,
		Gate:      gate,
		Validator: hostValidatorAdapter{v: validator},
		DNS:       dnsCheckerAdapter{v: validator},
		Slug:      slugReleaseAdapter{svc: slugSvc},
		Audit:     managementAudit{logger: slog.Default()},
		Now:       time.Now,
	})
	if err != nil {
		pool.Close()
		gateCleanup()
		log.Printf("crm: custom-domain UI disabled — management.New: %v", err)
		return nil, noop
	}
	// SIN-63133 — wire the SIN-63124 per-(tenant, IP) rate-limiter into
	// the handler so POST /api/customdomains/{id}/verify is actually
	// gated in production. Defaults: 10 req/min, burst 5, 10m idle TTL,
	// 5m janitor cadence (IdleTTL/2). The same managementAudit instance
	// passed to management.New is reused so denied:rate_limited events
	// land on the same slog pipeline as enrollment/verify audits.
	verifyLimiter := customdomainhttp.NewMemoryVerifyRateLimiter(customdomainhttp.VerifyRateLimitConfig{})
	mgmtAudit := managementAudit{logger: slog.Default()}
	handler, err := customdomainhttp.New(customdomainhttp.Config{
		UseCase:       uc,
		CSRF:          customdomainhttp.CSRFConfig{Secret: secret, Secure: getenv("HTTP_TLS_TERMINATED") == "1"},
		PrimaryDomain: primary,
		RateLimiter:   verifyLimiter,
		Audit:         mgmtAudit,
	})
	if err != nil {
		pool.Close()
		gateCleanup()
		log.Printf("crm: custom-domain UI disabled — handler: %v", err)
		return nil, noop
	}
	// SIN-63133 — janitor goroutine bound to the boot context so SIGTERM
	// (`signal.NotifyContext` in main) cancels it without leaking. Sweep
	// runs on the configured cadence (default IdleTTL/2 = 5m). Exit on
	// ctx.Done is graceful; the limiter remains queryable after the
	// janitor exits, but the process is shutting down anyway.
	go func() {
		if err := verifyLimiter.RunJanitor(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("crm: custom-domain verify rate-limit janitor: %v", err)
		}
	}()
	log.Printf("crm: custom-domain verify rate-limit janitor sweeping every %s", verifyLimiter.SweepInterval())
	cleanup := func() {
		pool.Close()
		gateCleanup()
	}
	return wrapWithDevTenantHeader(registerCustomDomainRoutes(handler), os.Stderr), cleanup
}

// registerCustomDomainRoutes returns a *http.ServeMux carrying the
// SIN-62259 routes. The /static/ FileServer used to be mounted here
// too, but that gated `/static/css/*` and the bundled HTMX vendor
// tree on CUSTOM_DOMAIN_UI_ENABLED=1 — every tenant template
// references those assets, so staging silently 404'd them for weeks
// when the flag wasn't set (SIN-63303). The FileServer is now
// mounted unconditionally on the public mux in cmd/server/main.go
// runWith; do NOT reintroduce the duplicate `mux.Handle` here —
// stdlib ServeMux panics on duplicate patterns.
func registerCustomDomainRoutes(h *customdomainhttp.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	h.Register(mux)
	return mux
}

// wrapWithDevTenantHeader is the dev-only tenant-context middleware. It
// reads `X-Tenant-ID` from the request, parses it as a UUID, and if
// valid stores it on the request context. Production deploys MUST
// replace this with real session/cookie auth before flipping
// CUSTOM_DOMAIN_UI_ENABLED.
func wrapWithDevTenantHeader(next http.Handler, _ *os.File) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get(envCustomDomainTenantHd)
		if raw == "" {
			next.ServeHTTP(w, r)
			return
		}
		tenantID, err := uuid.Parse(raw)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := customdomainhttp.WithTenantID(r.Context(), tenantID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// buildEnrollmentGate wires *enrollment.UseCase against the production
// Redis-backed adapters when rdb != nil (SIN-62334 F53):
//
//   - CountStore     -> pgstore.NewEnrollmentCountStore (Postgres
//     COUNT(*) on the same partial unique index the UI reads from).
//   - WindowCounter  -> rediswindow.New (sorted-set sliding window
//     5/h, 20/d, 50/mo).
//   - Breaker        -> circuitbreaker.New backed by redisstate (5
//     failures / 1h => 24h freeze, persisted across restarts).
//
// When rdb is nil (test path; production hard-errors before reaching
// this via EnrollmentRedisRequired), the gate falls back to in-memory
// placeholders so tests that drive the wire-up without Redis still
// work. Returning a cleanup func keeps the Redis client lifecycle
// aligned with the rest of the wire-up.
func buildEnrollmentGate(pool pgstore.PgxConn, rdb customDomainRedis) (management.EnrollmentGate, func()) {
	if rdb == nil {
		gate := enrollment.New(
			zeroCount{},
			passWindowCounter{},
			zeroBreaker{},
			nil,
			time.Now,
			enrollment.DefaultQuota(),
		)
		return enrollmentGateAdapter{gate: gate}, func() {}
	}
	store := pgstore.NewEnrollmentCountStore(pool)
	winCounter := rediswindow.New(rdb, enrollmentRedisPrefix)
	breakerState := redisstate.New(rdb, breakerRedisPrefix)
	breaker := circuitbreaker.New(breakerState, nil, time.Now, circuitbreaker.DefaultConfig())
	gate := enrollment.New(
		store,
		winCounter,
		breakerAdapter{uc: breaker},
		nil,
		time.Now,
		enrollment.DefaultQuota(),
	)
	cleanup := func() {
		_ = rdb.Close()
	}
	return enrollmentGateAdapter{gate: gate}, cleanup
}

// breakerAdapter projects circuitbreaker.UseCase into enrollment.Breaker.
// The use-case carries the policy (5/1h trip → 24h freeze); the gate
// only reads IsOpen from it.
type breakerAdapter struct{ uc *circuitbreaker.UseCase }

func (b breakerAdapter) IsOpen(ctx context.Context, tenantID uuid.UUID, now time.Time) (bool, error) {
	return b.uc.IsOpen(ctx, tenantID, now)
}

// enrollmentGateAdapter projects enrollment.Result into management.EnrollmentDecision.
type enrollmentGateAdapter struct{ gate *enrollment.UseCase }

func (a enrollmentGateAdapter) Allow(ctx context.Context, tenantID uuid.UUID) management.EnrollmentDecision {
	res := a.gate.Allow(ctx, tenantID)
	switch res.Decision {
	case enrollment.DecisionAllowed:
		return management.EnrollmentDecision{Allowed: true}
	case enrollment.DecisionDeniedHardCap:
		return management.EnrollmentDecision{Reason: management.ReasonRateLimited}
	case enrollment.DecisionDeniedHourlyQuota, enrollment.DecisionDeniedDailyQuota, enrollment.DecisionDeniedMonthlyQuota:
		return management.EnrollmentDecision{Reason: management.ReasonRateLimited, RetryAfter: res.ResetAfter}
	case enrollment.DecisionDeniedCircuitBreaker:
		return management.EnrollmentDecision{Reason: management.ReasonInternal}
	default:
		err := res.Err
		if err == nil {
			err = errors.New("enrollment: unknown decision")
		}
		return management.EnrollmentDecision{Err: fmt.Errorf("gate: %w", err)}
	}
}

// slugReleaseAdapter wraps slugreservation.Service so it satisfies
// management.SlugReleaser without leaking the Reservation type. Custom
// domains are FQDNs, but slugreservation operates on single-label slugs
// (regex `[a-z0-9](?:[a-z0-9-]*[a-z0-9])?`). The adapter extracts the
// first label so the 12-month lock is tracked at the natural slug level
// — `shop.example.com` reserves `shop` while the FQDN is removed. A
// per-host reservation table is its own ticket.
type slugReleaseAdapter struct{ svc *slugreservation.Service }

func (s slugReleaseAdapter) ReleaseSlug(ctx context.Context, host string, byTenantID uuid.UUID) error {
	if s.svc == nil {
		return nil
	}
	slug := firstLabel(host)
	if _, err := s.svc.ReleaseSlug(ctx, slug, byTenantID); err != nil {
		return err
	}
	return nil
}

// firstLabel returns the leftmost dot-separated component of host, or
// the whole string if there is no dot.
func firstLabel(host string) string {
	for i := 0; i < len(host); i++ {
		if host[i] == '.' {
			return host[:i]
		}
	}
	return host
}

// managementAudit emits one slog event per management decision.
type managementAudit struct{ logger *slog.Logger }

func (a managementAudit) LogManagement(ctx context.Context, ev management.AuditEvent) {
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.management",
		slog.String("tenant_id", ev.TenantID.String()),
		slog.String("domain_id", ev.DomainID.String()),
		slog.String("host", ev.Host),
		slog.String("action", ev.Action),
		slog.String("outcome", ev.Outcome),
		slog.String("reason", ev.Reason.String()),
	)
}

// validationAudit forwards SIN-62242 validation events to slog so the
// SSRF / token-mismatch / DNSSEC observability shows up in the same
// log pipeline as the management events.
type validationAudit struct{ logger *slog.Logger }

func (a validationAudit) Record(ctx context.Context, ev validation.AuditEvent) {
	attrs := []slog.Attr{
		slog.String("event", ev.Event),
		slog.String("host", ev.Host),
		slog.Time("at", ev.At),
	}
	for k, v := range ev.Detail {
		attrs = append(attrs, slog.String("detail."+k, v))
	}
	a.logger.LogAttrs(ctx, slog.LevelInfo, "customdomain.validation", attrs...)
}

// hostValidatorAdapter projects validation.Validator's host-only check
// into management.HostValidator. The Enroll path calls this before
// issuing a verification token, so the validator skips the TXT proof
// (the user has not published it yet) and only enforces the FQDN /
// IP-allowlist contract from ADR 0079 §1. Sentinel error mapping keeps
// the management.classifyValidationError -> Reason chain intact:
// validation.ErrPrivateIP is wrapped in management.ErrPrivateIP so
// errors.Is reaches the right Reason.
type hostValidatorAdapter struct{ v *validation.Validator }

func (h hostValidatorAdapter) Validate(ctx context.Context, host string) error {
	if err := h.v.ValidateHostOnly(propagateTenantID(ctx), host); err != nil {
		switch {
		case errors.Is(err, validation.ErrPrivateIP):
			return fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		case errors.Is(err, validation.ErrEmptyHost):
			return fmt.Errorf("%w: %w", management.ErrInvalidHost, err)
		default:
			return err
		}
	}
	return nil
}

// dnsCheckerAdapter projects validation.Validator into management.DNSChecker.
// The Verify path calls this with the stored verification token; the
// validator runs the full DNS-only check (TXT proof + IP allowlist +
// DNSSEC observability) and returns the result the management
// use-case needs to flip `verified_with_dnssec`.
//
// Error mapping mirrors hostValidatorAdapter: validation sentinels are
// wrapped in management equivalents so errors.Is on the boundary
// classifies them correctly. ErrTokenMismatch is the most important
// case — without the wrap, classifyValidationError falls through to
// ReasonDNSResolutionFailed and the UI shows the wrong PT-BR copy.
//
// LogID is currently nil. SIN-62333 wires the dns_resolution_log
// writer (validation.Writer / pgstore.DNSResolutionLogStore) so a row
// IS persisted on every Validate call now, but plumbing the inserted
// row id back through DNSCheckResult.LogID is a separate ticket. The
// DNSSEC flag still flows through so the badge renders correctly.
type dnsCheckerAdapter struct{ v *validation.Validator }

func (d dnsCheckerAdapter) Check(ctx context.Context, host, expectedToken string) (management.DNSCheckResult, error) {
	res, err := d.v.Validate(propagateTenantID(ctx), host, expectedToken)
	if err != nil {
		switch {
		case errors.Is(err, validation.ErrTokenMismatch):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrTokenMismatch, err)
		case errors.Is(err, validation.ErrPrivateIP):
			return management.DNSCheckResult{}, fmt.Errorf("%w: %w", management.ErrPrivateIP, err)
		default:
			return management.DNSCheckResult{}, err
		}
	}
	return management.DNSCheckResult{WithDNSSEC: res.VerifiedWithDNSSEC}, nil
}

// zeroCount, passWindowCounter, zeroBreaker are dev-only placeholders
// retained for the test path that drives buildCustomDomainHandler
// without REDIS_URL. Production never reaches them — cmd/server's
// runWith calls EnrollmentRedisRequired before this function and exits
// non-zero on its error (SIN-62334 F53).
type zeroCount struct{}

func (zeroCount) ActiveCount(context.Context, uuid.UUID) (int, error) { return 0, nil }

type passWindowCounter struct{}

func (passWindowCounter) CountAndRecord(_ context.Context, _ uuid.UUID, _ enrollment.Window, _ time.Time) (int, error) {
	return 0, nil
}

type zeroBreaker struct{}

func (zeroBreaker) IsOpen(context.Context, uuid.UUID, time.Time) (bool, error) { return false, nil }

// propagateTenantID translates the tenant id stored on the request
// context by `wrapWithDevTenantHeader` (or the future production auth
// middleware) into the validation package's own context key. The
// validation use-case then writes that id into every dns_resolution_log
// row it persists. uuid.Nil is left alone so anonymous calls land as
// SQL NULL.
func propagateTenantID(ctx context.Context) context.Context {
	tenantID := customdomainhttp.TenantIDFromContext(ctx)
	if tenantID == uuid.Nil {
		return ctx
	}
	return validation.WithTenantID(ctx, tenantID)
}

// nopAudit / nopSlack satisfy slugreservation's required ports without
// emitting anything; the management UI never invokes the master-override
// path (that lives on a separate admin surface).
type nopAudit struct{}

func (nopAudit) LogMasterOverride(context.Context, slugreservation.MasterOverrideEvent) error {
	return nil
}

type nopSlack struct{}

func (nopSlack) NotifyAlert(context.Context, string) error { return nil }

// Compile-time guards.
var (
	_ enrollment.CountStore             = zeroCount{}
	_ enrollment.WindowCounter          = passWindowCounter{}
	_ enrollment.Breaker                = zeroBreaker{}
	_ enrollment.Breaker                = breakerAdapter{}
	_ slugreservation.MasterAuditLogger = nopAudit{}
	_ slugreservation.SlackNotifier     = nopSlack{}
	_ management.EnrollmentGate         = enrollmentGateAdapter{}
	_ management.SlugReleaser           = slugReleaseAdapter{}
	_ management.AuditLogger            = managementAudit{}
	_ management.HostValidator          = hostValidatorAdapter{}
	_ management.DNSChecker             = dnsCheckerAdapter{}
	_ validation.Auditor                = validationAudit{}
	_ circuitbreaker.State              = (*circuitbreaker.InMemoryState)(nil) // package import sentinel
)
