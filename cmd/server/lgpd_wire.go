package main

// SIN-63186 wiring — LGPD data-subject admin surface (Fase 6 PR3).
//
// buildLGPDStack assembles the GET /admin/lgpd/export +
// POST /admin/lgpd/delete handlers plus the lgpd_admin rate-limit
// policy (10/min/tenant — AC #7). The handler is wired here so the
// /admin/lgpd endpoints become reachable through the chi router and
// the per-route RequireAction gate (ActionTenantLGPDExport /
// ActionTenantLGPDDelete) actually runs in the production middleware
// chain.
//
// Returns a stack with nil routes and a no-op cleanup when any
// required input is missing (pool, redis, master-ops DSN). cmd/server
// then boots without the LGPD endpoints rather than panicking — the
// chi router skips both routes when LGPDRoutes.Export / Delete are
// nil.

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	pglgpd "github.com/pericles-luz/crm/internal/adapter/db/postgres/lgpd"
	"github.com/pericles-luz/crm/internal/adapter/httpapi"
	httpratelimit "github.com/pericles-luz/crm/internal/adapter/httpapi/ratelimit"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	domainratelimit "github.com/pericles-luz/crm/internal/iam/ratelimit"
	domain "github.com/pericles-luz/crm/internal/lgpd"
	"github.com/pericles-luz/crm/internal/tenancy"
	weblgpd "github.com/pericles-luz/crm/internal/web/lgpd"
)

const (
	// envLGPDFiscalRetentionYears tunes the fiscal-retention window the
	// handler stamps onto every new deletion-request row. AC #2 default
	// is 5 years (Brazilian fiscal baseline); operators dial it per
	// jurisdiction without a redeploy. Unset / non-numeric falls back
	// to lgpd.DefaultFiscalRetentionYears.
	envLGPDFiscalRetentionYears = "LGPD_FISCAL_RETENTION_YEARS"

	// envLGPDAdminRatePerMin tunes the lgpd_admin per-tenant rate
	// limit on /admin/lgpd/{export,delete}. AC #7 default is 10/min;
	// kept low because export is heavy and an attacker who has stolen
	// gerente credentials should not be able to siphon every tenant's
	// data in a single burst. Operators can dial it down further (DR
	// drill) without redeploying.
	envLGPDAdminRatePerMin = "LGPD_ADMIN_RATE_PER_MIN"

	// defaultLGPDAdminRatePerMin is the AC #7 cap.
	defaultLGPDAdminRatePerMin = 10

	// lgpdAdminPolicyName is the iam/ratelimit.Policy name used by the
	// per-tenant bucket. Distinct from the auth-side policy names so
	// the Redis key prefix never collides; named to match the doc.go
	// in internal/web/lgpd.
	lgpdAdminPolicyName = "lgpd_admin"

	// lgpdAdminRateRedisPrefix is the Redis key namespace for the
	// per-tenant LGPD admin rate limiter. Lives under its own root so
	// a flush of the auth-side limiter does not nuke the LGPD cap.
	lgpdAdminRateRedisPrefix = "lgpd_admin:rl:"
)

// lgpdStack bundles the router-level routes payload plus a cleanup
// hook for any pool the wire layer opens beyond the shared IAM pool.
// Cleanup is non-nil even when Routes is empty so the caller can
// always defer it without a nil-check.
type lgpdStack struct {
	Routes  httpapi.LGPDRoutes
	Cleanup func()
}

// noopLGPDStack returns a stack with no mounted routes and a no-op
// cleanup. Used whenever a required input is missing so cmd/server's
// defer chain stays uniform.
func noopLGPDStack() lgpdStack {
	return lgpdStack{Cleanup: func() {}}
}

// buildLGPDStack assembles the SIN-63186 admin handlers + rate-limit
// middleware. pool is the IAM runtime pgxpool (reused so we don't open
// a second connection set), rdb is the shared goredis client backing
// the auth-side limiter, and getenv sources the master-ops DSN + the
// AC #7 rate knob.
//
// Returns noopLGPDStack() on any failure or missing input — the chi
// router then omits /admin/lgpd/{export,delete} cleanly. The handler
// constructor panics on nil collaborators (defensive), so every
// failure path here returns early before reaching weblgpd.New.
func buildLGPDStack(ctx context.Context, pool *pgxpool.Pool, rdb *goredis.Client, getenv func(string) string) lgpdStack {
	if pool == nil || rdb == nil {
		return noopLGPDStack()
	}
	masterDSN := getenv(envMasterOpsDSN)
	if masterDSN == "" {
		log.Printf("crm: web/lgpd handler disabled (%s unset)", envMasterOpsDSN)
		return noopLGPDStack()
	}

	masterPool, err := pgxpool.New(ctx, masterDSN)
	if err != nil {
		log.Printf("crm: web/lgpd handler disabled — master pg connect: %v", err)
		return noopLGPDStack()
	}

	store, err := pglgpd.New(pool, masterPool)
	if err != nil {
		masterPool.Close()
		log.Printf("crm: web/lgpd handler disabled — store: %v", err)
		return noopLGPDStack()
	}

	splitLogger, err := postgresadapter.NewSplitAuditLogger(pool)
	if err != nil {
		masterPool.Close()
		log.Printf("crm: web/lgpd handler disabled — audit logger: %v", err)
		return noopLGPDStack()
	}

	policy, err := domain.NewRetentionPolicy(readLGPDFiscalRetentionYears(getenv))
	if err != nil {
		masterPool.Close()
		log.Printf("crm: web/lgpd handler disabled — retention policy: %v", err)
		return noopLGPDStack()
	}

	handler, err := weblgpd.New(weblgpd.Deps{
		Export:    store,
		Deletions: store,
		Audit:     splitLogger,
		Policy:    policy,
		Now:       func() time.Time { return time.Now().UTC() },
		Logger:    slog.Default(),
	})
	if err != nil {
		masterPool.Close()
		log.Printf("crm: web/lgpd handler disabled — handler: %v", err)
		return noopLGPDStack()
	}

	rate := readLGPDAdminRatePerMin(getenv)
	mw, err := buildLGPDRateLimitMiddleware(rdb, rate, slog.Default())
	if err != nil {
		masterPool.Close()
		log.Printf("crm: web/lgpd handler disabled — rate limit: %v", err)
		return noopLGPDStack()
	}

	log.Printf("crm: web/lgpd /admin/lgpd/{export,delete} mounted (rate=%d/min/tenant, retention=%dy)", rate, policy.FiscalYears)
	return lgpdStack{
		Routes: httpapi.LGPDRoutes{
			Export:    http.HandlerFunc(handler.Export),
			Delete:    http.HandlerFunc(handler.Delete),
			RateLimit: mw,
		},
		Cleanup: func() { masterPool.Close() },
	}
}

// buildLGPDRateLimitMiddleware assembles the lgpd_admin per-tenant
// throttle in front of the export + delete handlers. Lives as a
// separate function so cmd/server tests can substitute the
// policy/limiter without dragging in goredis. The single bucket keys
// off the resolved tenant id from context (the tenant is on the
// request context at this point because middleware.TenantScope ran
// upstream in the chi tenanted group).
func buildLGPDRateLimitMiddleware(rdb *goredis.Client, ratePerMin int, logger *slog.Logger) (func(http.Handler) http.Handler, error) {
	policy, err := domainratelimit.NewPolicy(
		lgpdAdminPolicyName,
		[]domainratelimit.Bucket{
			{Name: "tenant", Window: time.Minute, Max: ratePerMin},
		},
		domainratelimit.Lockout{},
	)
	if err != nil {
		return nil, fmt.Errorf("web/lgpd: build rate-limit policy: %w", err)
	}
	limiter := rlredis.New(rdb, lgpdAdminRateRedisPrefix)
	mw, err := httpratelimit.New(httpratelimit.Config{
		Policy:  policy,
		Limiter: limiter,
		Buckets: []httpratelimit.Bucket{
			{Name: "tenant", Extractor: lgpdTenantKeyExtractor},
		},
		Logger: logger,
	})
	if err != nil {
		return nil, fmt.Errorf("web/lgpd: build rate-limit middleware: %w", err)
	}
	return mw, nil
}

// lgpdTenantKeyExtractor reads the resolved tenant id from the request
// context (placed there by middleware.TenantScope). Empty key when no
// tenant is on context — the limiter middleware then skips this bucket
// rather than collapsing all tenants into a single global bucket. The
// fail-open is acceptable here because the /admin/lgpd routes are
// already gated by RequireAuth + RequireAction; the rate limit is a
// secondary defence against credentialled-attacker exfiltration.
func lgpdTenantKeyExtractor(r *http.Request) string {
	if r == nil {
		return ""
	}
	t, err := tenancy.FromContext(r.Context())
	if err != nil || t == nil {
		return ""
	}
	return t.ID.String()
}

// readLGPDFiscalRetentionYears parses LGPD_FISCAL_RETENTION_YEARS;
// unset / non-positive falls back to lgpd.DefaultFiscalRetentionYears
// (5y Brazilian baseline). Capped at 100 so a typo cannot wedge the
// retention window past anyone's lifetime.
func readLGPDFiscalRetentionYears(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envLGPDFiscalRetentionYears))
	if raw == "" {
		return domain.DefaultFiscalRetentionYears
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return domain.DefaultFiscalRetentionYears
	}
	if v > 100 {
		v = 100
	}
	return v
}

// readLGPDAdminRatePerMin parses LGPD_ADMIN_RATE_PER_MIN; unset /
// non-positive falls back to defaultLGPDAdminRatePerMin (10).
func readLGPDAdminRatePerMin(getenv func(string) string) int {
	raw := strings.TrimSpace(getenv(envLGPDAdminRatePerMin))
	if raw == "" {
		return defaultLGPDAdminRatePerMin
	}
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return defaultLGPDAdminRatePerMin
	}
	if v > 10_000 {
		v = 10_000
	}
	return v
}
