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
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// EnvDSN names the env var that holds the runtime DSN. cmd/server reads it
// (see PR3 wire-up) and passes the value to NewFromEnv / New.
const EnvDSN = "DATABASE_URL"

// EnvPingRetryBudget names the optional env var overriding the boot-time ping
// retry budget. The value is parsed as a Go time.Duration (e.g. "30s",
// "1ms"); when unset, empty, or unparseable, New falls back to
// defaultPingRetryBudget so production keeps its 60s self-heal window. Boot
// tests that point DATABASE_URL at an unreachable host set it tiny so each
// pool fails fast instead of spending the full budget per pool.
const EnvPingRetryBudget = "DB_PING_RETRY_BUDGET"

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
	budget := resolvePingRetryBudget(os.Getenv(EnvPingRetryBudget))
	if err := pingWithRetry(ctx, pool, budget, defaultPingInitialBackoff, defaultPingMaxBackoff); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}
	return pool, nil
}

// resolvePingRetryBudget parses raw (a time.Duration string from
// EnvPingRetryBudget) and returns it when valid and strictly positive;
// otherwise it returns defaultPingRetryBudget. Keeping the parse pure (no
// os.Getenv inside) lets it be unit-tested without mutating the process
// environment.
func resolvePingRetryBudget(raw string) time.Duration {
	if raw == "" {
		return defaultPingRetryBudget
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return defaultPingRetryBudget
	}
	return d
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
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			// Budget gone. Surface the last ping error if at least one
			// attempt was made (the top-of-loop guard can win the race with
			// the mid-loop exit below when a backoff sleep pushes time past
			// the deadline). Fall back to DeadlineExceeded only when the
			// budget was so small no ping could be attempted at all.
			if lastErr != nil {
				return lastErr
			}
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
		lastErr = err
		// The caller's ctx (not the per-attempt budget) being done means
		// stop now and surface its error, not the attempt's timeout.
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		// Retry on ANY ping failure up to the budget (literal AC1): a host
		// that is still coming up surfaces transient errors, and a host that
		// hangs at the TCP layer is bounded by the per-attempt timeout above,
		// so the loop still exits within budget either way.
		//
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

// EnvEnforceRLSRole names the env var that turns the runtime RLS-role boot
// guard from a WARNING into a hard boot failure. When its value is "1",
// EnforceRuntimeRLSRoleFromEnv returns ErrRuntimeRoleBypassesRLS (so the
// process never finishes booting) if the runtime DB role is SUPERUSER or
// BYPASSRLS. compose.stg.yml and compose.yml (prod) set it; dev `make up`
// leaves it unset so connecting as the bootstrap superuser only WARNs.
const EnvEnforceRLSRole = "DB_ENFORCE_RLS_ROLE"

// ErrRuntimeRoleBypassesRLS is returned by the runtime RLS-role guard when
// the connected role bypasses Row-Level Security (SUPERUSER or BYPASSRLS)
// and enforcement is on (DB_ENFORCE_RLS_ROLE=1). Callers can errors.Is on
// it to surface a deterministic boot-failure hint. RLS is the DB half of
// the tenant-isolation defense (RLS + WithTenant, ADR 0071); a runtime role
// that bypasses it silently disables cross-tenant protection for every
// query, so the only safe response in stg/prod is to refuse to boot.
var ErrRuntimeRoleBypassesRLS = errors.New("postgres: runtime DB role bypasses RLS")

// rlsRoleQuerier is the tiny seam assertRuntimeRLSRole needs. *pgxpool.Pool
// already satisfies it; extracting it lets the guard's branches be unit
// tested with a fake row that returns canned (rolsuper, rolbypassrls)
// tuples, with no real database (table-driven, SIN-65590 AC3).
type rlsRoleQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// assertRuntimeRLSRole reads the connected role's RLS-bypass attributes
// from pg_roles and applies the SIN-65590 boot policy:
//
//   - role is NOBYPASSRLS and not SUPERUSER  → nil (correct; the runtime
//     pool's tenant RLS is enforced).
//   - role is SUPERUSER or BYPASSRLS         → ALWAYS emit a structured
//     WARNING; return ErrRuntimeRoleBypassesRLS only when enforce is true.
//
// It reads ONLY current_user / pg_roles attributes — never the DSN, a
// password, or any secret (security bar). A query/scan error is treated
// conservatively: it fails (wrapped) when enforce is true, otherwise it
// WARNs and returns nil so a transient catalog hiccup never bricks a dev
// boot. logf is injected (log.Printf in production) so tests can capture it.
func assertRuntimeRLSRole(ctx context.Context, q rlsRoleQuerier, enforce bool, logf func(string, ...any)) error {
	const query = `SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`
	var rolname string
	var super, bypass bool
	if err := q.QueryRow(ctx, query).Scan(&rolname, &super, &bypass); err != nil {
		if enforce {
			return fmt.Errorf("postgres: runtime RLS-role guard: read pg_roles: %w", err)
		}
		logf("level=WARN component=postgres event=rls_role_check_failed enforce=false err=%q msg=%q",
			err.Error(),
			"could not verify the runtime DB role enforces RLS; continuing (set DB_ENFORCE_RLS_ROLE=1 to fail-fast)")
		return nil
	}
	if !super && !bypass {
		return nil
	}
	logf("level=WARN component=postgres event=runtime_role_bypasses_rls role=%q rolsuper=%t rolbypassrls=%t enforce=%t msg=%q",
		rolname, super, bypass, enforce,
		"runtime DB role bypasses Row-Level Security; tenant isolation is NOT enforced for this pool. The runtime DSN MUST connect as a NOBYPASSRLS role (e.g. app_runtime). See deploy/compose/.env.example.")
	if enforce {
		return fmt.Errorf("%w: role %q (rolsuper=%t rolbypassrls=%t); point DATABASE_URL at a NOBYPASSRLS role (app_runtime)",
			ErrRuntimeRoleBypassesRLS, rolname, super, bypass)
	}
	return nil
}

// EnforceRuntimeRLSRoleFromEnv is the boot-time defense-in-depth guard for
// the runtime pool (SIN-65590). cmd/server calls it once early in boot. It:
//
//   - no-ops (nil) when DATABASE_URL is unset, so dev/local without a DB and
//     the fail-soft feature wires are unaffected;
//   - opens a short-lived pool on the RUNTIME DSN (DATABASE_URL) only — the
//     app_master_ops / audit pools (MASTER_OPS_DATABASE_URL) are BYPASSRLS by
//     design and are NEVER inspected here;
//   - on a connectivity error, WARNs and returns nil (connectivity is the
//     feature wires' / New's ping-retry concern, not this guard's — keep the
//     existing fail-soft boot contract);
//   - otherwise delegates to assertRuntimeRLSRole, which WARNs always and
//     hard-fails (ErrRuntimeRoleBypassesRLS) only when DB_ENFORCE_RLS_ROLE=1.
//
// It closes the pool before returning; the real runtime pools are opened
// independently by each feature wire.
func EnforceRuntimeRLSRoleFromEnv(ctx context.Context, getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	dsn := getenv(EnvDSN)
	if dsn == "" {
		return nil
	}
	enforce := getenv(EnvEnforceRLSRole) == "1"
	pool, err := New(ctx, dsn)
	if err != nil {
		log.Printf("level=WARN component=postgres event=rls_role_guard_skipped enforce=%t msg=%q err=%v",
			enforce,
			"could not open the runtime pool to verify it enforces RLS; skipping the role guard (connectivity is handled elsewhere)",
			err)
		return nil
	}
	defer pool.Close()
	return assertRuntimeRLSRole(ctx, pool, enforce, log.Printf)
}

// EnvAuditDSN names the env var holding the DSN for the dedicated audit
// pool (SIN-66332). It MUST connect as role app_audit — LOGIN, BYPASSRLS,
// INSERT-only on the split audit tables (migration 0078). The writer
// (SplitAuditLogger) supplies tenant_id explicitly and legitimately
// persists NULL-tenant master/impersonation rows, so it cannot depend on
// the per-tenant app.tenant_id GUC the runtime pool's RLS keys on.
const EnvAuditDSN = "AUDIT_DATABASE_URL"

// EnvAppEnv mirrors the cmd/server APP_ENV knob (staging|production|...).
// Audit is a non-repudiation control, so when AUDIT_DATABASE_URL is unset
// in a non-dev environment the audit-role guard fails the boot rather than
// silently degrading the security ledger.
const EnvAppEnv = "APP_ENV"

// ErrAuditRoleEnforcesRLS is the inverse of ErrRuntimeRoleBypassesRLS: the
// audit pool MUST bypass RLS so audit INSERTs succeed regardless of
// app.tenant_id. A NOBYPASSRLS audit role re-introduces the 42501 the
// dedicated role exists to close (the 2FA-enroll-500 root cause). Hard-fails
// only when DB_ENFORCE_RLS_ROLE=1; otherwise WARNs (dev parity).
var ErrAuditRoleEnforcesRLS = errors.New("postgres: audit DB role does not bypass RLS")

// ErrAuditDSNRequired is returned by the audit-role guard when
// AUDIT_DATABASE_URL is unset in a non-dev environment (APP_ENV in
// {staging, production}). Audit is a non-repudiation control; booting
// stg/prod without the dedicated app_audit pool would route audit writes
// through the NOBYPASSRLS runtime pool and drop (or 500 on) every write.
var ErrAuditDSNRequired = errors.New("postgres: AUDIT_DATABASE_URL is required in staging/production")

// NewAuditFromEnv builds the dedicated app_audit pool from AUDIT_DATABASE_URL.
// Returns (nil, ErrEmptyDSN) when unset so the caller can fall back to the
// runtime pool in dev (where the bootstrap role is SUPERUSER/BYPASSRLS and
// RLS is off, so audit INSERTs still succeed). In stg/prod the unset case is
// rejected at boot by EnforceAuditRLSRoleFromEnv before this is reached.
// Callers MUST Close the returned pool on shutdown.
func NewAuditFromEnv(ctx context.Context, getenv func(string) string) (*pgxpool.Pool, error) {
	if getenv == nil {
		return nil, ErrEmptyDSN
	}
	return New(ctx, getenv(EnvAuditDSN))
}

// assertAuditRLSRole is the inverse of assertRuntimeRLSRole: the audit role
// is correct precisely when it bypasses RLS (SUPERUSER or BYPASSRLS). A
// NOBYPASSRLS audit role ALWAYS WARNs and hard-fails only when enforce is
// true. Reads only current_user / pg_roles attributes — never the DSN or any
// secret. A query/scan error is conservative: fail (wrapped) when enforce,
// else WARN and return nil so a transient catalog hiccup never bricks a boot.
func assertAuditRLSRole(ctx context.Context, q rlsRoleQuerier, enforce bool, logf func(string, ...any)) error {
	const query = `SELECT rolname, rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`
	var rolname string
	var super, bypass bool
	if err := q.QueryRow(ctx, query).Scan(&rolname, &super, &bypass); err != nil {
		if enforce {
			return fmt.Errorf("postgres: audit RLS-role guard: read pg_roles: %w", err)
		}
		logf("level=WARN component=postgres event=audit_role_check_failed enforce=false err=%q msg=%q",
			err.Error(),
			"could not verify the audit DB role bypasses RLS; continuing (set DB_ENFORCE_RLS_ROLE=1 to fail-fast)")
		return nil
	}
	if super || bypass {
		return nil
	}
	logf("level=WARN component=postgres event=audit_role_enforces_rls role=%q rolsuper=%t rolbypassrls=%t enforce=%t msg=%q",
		rolname, super, bypass, enforce,
		"audit DB role does NOT bypass Row-Level Security; audit INSERTs will fail (SQLSTATE 42501) for NULL-tenant or out-of-WithTenant writes. AUDIT_DATABASE_URL MUST connect as a BYPASSRLS role (app_audit). See deploy/compose/.env.example.")
	if enforce {
		return fmt.Errorf("%w: role %q (rolsuper=%t rolbypassrls=%t); point AUDIT_DATABASE_URL at a BYPASSRLS role (app_audit)",
			ErrAuditRoleEnforcesRLS, rolname, super, bypass)
	}
	return nil
}

// EnforceAuditRLSRoleFromEnv is the boot-time defense-in-depth guard for the
// dedicated audit pool (SIN-66332), the inverse of the runtime guard:
//
//   - AUDIT_DATABASE_URL unset + DATABASE_URL set + APP_ENV in
//     {staging, production} → returns ErrAuditDSNRequired (fail-hard: a prod
//     deployment that talks to a DB but has no dedicated audit pool would
//     route audit writes through the NOBYPASSRLS runtime pool and 500/drop
//     them — audit is a non-repudiation control). Gated on DATABASE_URL the
//     same way the runtime/TLS guards are: with no DB there are no audit
//     writes to route, so the requirement is moot;
//   - AUDIT_DATABASE_URL unset + dev/other APP_ENV (or no DATABASE_URL) → nil
//     (fail-soft; in dev the runtime pool is SUPERUSER/BYPASSRLS and absorbs
//     audit writes);
//   - AUDIT_DATABASE_URL set → opens a short-lived pool and delegates to
//     assertAuditRLSRole, which WARNs always and hard-fails (ErrAuditRoleEnforcesRLS)
//     only when DB_ENFORCE_RLS_ROLE=1;
//   - connectivity error on a set DSN → WARNs and returns nil (connectivity is
//     New's ping-retry concern, mirroring EnforceRuntimeRLSRoleFromEnv).
//
// It closes its probe pool before returning; the real audit pool is opened
// separately by NewAuditFromEnv in cmd/server.
func EnforceAuditRLSRoleFromEnv(ctx context.Context, getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	dsn := getenv(EnvAuditDSN)
	if dsn == "" {
		// Required only when the app actually connects to a DB in a non-dev
		// environment; with no DATABASE_URL there are no audit writes to
		// route (mirrors the runtime/TLS guards, which also no-op without a
		// DSN), so narrow boot-gate paths are unaffected.
		if getenv(EnvDSN) != "" {
			switch getenv(EnvAppEnv) {
			case "staging", "production":
				return ErrAuditDSNRequired
			}
		}
		return nil
	}
	enforce := getenv(EnvEnforceRLSRole) == "1"
	pool, err := New(ctx, dsn)
	if err != nil {
		log.Printf("level=WARN component=postgres event=audit_role_guard_skipped enforce=%t msg=%q err=%v",
			enforce,
			"could not open the audit pool to verify it bypasses RLS; skipping the role guard (connectivity is handled elsewhere)",
			err)
		return nil
	}
	defer pool.Close()
	return assertAuditRLSRole(ctx, pool, enforce, log.Printf)
}
