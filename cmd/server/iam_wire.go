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
	slackadapter "github.com/pericles-luz/crm/internal/adapter/notify/slack"
	rlredis "github.com/pericles-luz/crm/internal/adapter/ratelimit/redis"
	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/ratelimit"
	"github.com/pericles-luz/crm/internal/tenancy"
)

const envSlackWebhook = "SLACK_WEBHOOK_URL"

// iamRoutes lists the path patterns the chi router handles on the public mux.
// Registering them explicitly keeps the stdlib mux in control of dispatch
// order — custom-domain catch-all at "/" still fires last.
var iamRoutes = []string{
	"/login",
	"/logout",
	"/hello-tenant",
	"/m/",
	"/metrics",
}

// buildIAMHandler assembles the IAM deps and returns the chi handler plus a
// cleanup function. Returns (nil, no-op) when DATABASE_URL or REDIS_URL is
// unset. The caller MUST defer the cleanup to release pool + Redis.
func buildIAMHandler(ctx context.Context, getenv func(string) string) (http.Handler, func()) {
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
		Policies:       policies,
		RateLimiter:    limiter,
		Authorizer:     audited,
		// SessionToucher is nil — Activity middleware deferred to batch
		// that lands the session role/last_activity DB columns (0077).
		// Master MFA deps deferred to batch 17 (SIN-62526).
	})

	fullCleanup := func() {
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
