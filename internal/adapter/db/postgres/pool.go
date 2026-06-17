// Package postgres factory for the application's pgx pool.
//
// New is the only place in the codebase allowed to construct a
// *pgxpool.Pool for application use; the testpg harness has its own
// constructor for integration tests. Pool tuning lives here so call sites
// don't need to know the values, and the notenant analyzer (SIN-62232 /
// ADR 0071) blocks any direct .Exec/.Query against the pool from
// non-adapter code — every tenant-scoped query goes through WithTenant.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the env var that holds the runtime DSN. cmd/server reads it
// (see PR3 wire-up) and passes the value to NewFromEnv / New.
const EnvDSN = "DATABASE_URL"

// Fase 0 defaults. Tuned for a single-replica app talking to one Postgres;
// PR9 revisits when the production Dockerfile and staging soak land.
const (
	defaultMaxConns          int32         = 10
	defaultMinConns          int32         = 2
	defaultMaxConnIdleTime   time.Duration = 5 * time.Minute
	defaultMaxConnLifetime   time.Duration = 30 * time.Minute
	defaultHealthCheckPeriod time.Duration = 30 * time.Second
)

// Boot-time ping retry budget. On a host reboot or Docker daemon restart,
// app and postgres come up together and the app may boot while Postgres is
// still starting (SQLSTATE 57P03 / connection refused). A single Ping would
// permanently disable every surface for the process lifetime; instead we
// retry with exponential backoff so the pool self-heals once the DB accepts
// connections. depends_on: service_healthy does NOT cover daemon/host
// restarts, so the recovery must live in the code (SIN-65041 / SIN-65016).
//
// The budget is a package-level default so it can be tuned later. Only the
// Ping step retries; empty/malformed DSN still fail fast (see New).
const (
	defaultPingRetryBudget    time.Duration = 60 * time.Second
	defaultPingInitialBackoff time.Duration = 500 * time.Millisecond
	defaultPingMaxBackoff     time.Duration = 5 * time.Second
)

// ErrEmptyDSN is returned when the DSN string is empty. Callers can use
// errors.Is to surface a startup-time hint (e.g. "set DATABASE_URL").
var ErrEmptyDSN = errors.New("postgres: dsn is empty")

// New parses the DSN, applies the Fase 0 pool defaults, opens the pool, and
// pings to fail fast on bad credentials or unreachable hosts. Callers MUST
// Close the returned pool on shutdown.
//
// The DSN MUST point at the app_runtime role in production. app_runtime is
// NOBYPASSRLS, so SELECTs that don't go through WithTenant return zero rows
// (defense in depth: RLS at the DB plus WithTenant in the app — ADR 0071).
func New(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, ErrEmptyDSN
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse dsn: %w", err)
	}
	cfg.MaxConns = defaultMaxConns
	cfg.MinConns = defaultMinConns
	cfg.MaxConnIdleTime = defaultMaxConnIdleTime
	cfg.MaxConnLifetime = defaultMaxConnLifetime
	cfg.HealthCheckPeriod = defaultHealthCheckPeriod

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}
	if err := pingWithRetry(ctx, pool, defaultPingRetryBudget, defaultPingInitialBackoff, defaultPingMaxBackoff); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// Pinger is the tiny seam pingWithRetry needs. *pgxpool.Pool already
// satisfies it; extracting it lets the backoff policy be unit-tested with a
// fake that fails N times then succeeds, without a real database.
type Pinger interface {
	Ping(context.Context) error
}

// pingWithRetry pings p, retrying on failure with exponential backoff
// (initialBackoff, doubling, capped at maxBackoff) until the ping succeeds,
// the budget is exhausted, or ctx is done.
//
// It returns nil on the first successful ping; ctx.Err() if ctx is
// cancelled/expired before a ping succeeds (returning promptly, not after
// the full budget); or the last ping error once the budget is spent. The
// budget bounds total wall-clock even when ctx has no deadline. It never
// busy-spins: each wait is a time.Timer selected against ctx.Done(), so
// there is no goroutine leak.
//
// Each Ping attempt gets its own bounded deadline (min(maxBackoff*2,
// remaining budget)). Without this cap a single p.Ping(ctx) can block
// forever when the caller ctx has no deadline (production main, cmd/server
// tests) and the DB host hangs at the TCP layer (slow DNS / no RST): the
// budget check below sits *after* the Ping, so it is never reached. The
// per-attempt timeout guarantees pingWithRetry exits within budget.
func pingWithRetry(ctx context.Context, p Pinger, budget, initialBackoff, maxBackoff time.Duration) error {
	deadline := time.Now().Add(budget)
	backoff := initialBackoff
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Budget gone before this attempt could start.
			return context.DeadlineExceeded
		}
		perAttempt := maxBackoff * 2
		if perAttempt > remaining {
			perAttempt = remaining
		}
		pingCtx, pingCancel := context.WithTimeout(ctx, perAttempt)
		err := p.Ping(pingCtx)
		pingCancel()
		if err == nil {
			return nil
		}
		// The caller's ctx (not the per-attempt budget) being done means
		// stop now and surface its error, not the attempt's timeout.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// Per-attempt deadline fired but the caller ctx is still live. A
		// reboot produces *fast* kernel errors (ECONNREFUSED while Postgres
		// starts, then 57P03 while it initialises) — those retry below and
		// the pool self-heals. A per-attempt timeout firing instead means the
		// host is hanging (slow DNS / no TCP RST), which is not a "coming up"
		// condition: fail fast without spending the remaining budget.
		if errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		// Surface the real ping error (not a ctx/timer artifact) once the
		// budget is spent or the next backoff would overrun it.
		if now := time.Now(); !now.Before(deadline) || !now.Add(backoff).Before(deadline) {
			return err
		}
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < maxBackoff {
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// NewFromEnv is the convenience wrapper used by cmd/server. It reads
// DATABASE_URL via the supplied getenv (typically os.Getenv) and forwards
// to New. Returning ErrEmptyDSN here lets the caller log a deterministic
// "DATABASE_URL is not set" message without sniffing the wrap chain.
func NewFromEnv(ctx context.Context, getenv func(string) string) (*pgxpool.Pool, error) {
	if getenv == nil {
		return nil, ErrEmptyDSN
	}
	return New(ctx, getenv(EnvDSN))
}
