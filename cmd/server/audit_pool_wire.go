package main

// SIN-66332 — wiring for the dedicated app_audit pool used by every
// SplitAuditLogger. The pool connects as role app_audit (LOGIN, BYPASSRLS,
// INSERT-only on the split audit tables) so audit writes succeed regardless
// of the per-tenant app.tenant_id GUC the runtime pool's RLS keys on. This
// is the fix for the 2FA-enroll 500 (RLS 42501): the bare audit INSERT runs
// outside any WithTenant scope, so on the NOBYPASSRLS runtime pool the
// tenant_isolation_insert policy (migration 0083) compared tenant_id against
// a NULL GUC and rejected the row.
//
// buildAuditPool returns nil when AUDIT_DATABASE_URL is unset (dev); callers
// then fall back to the runtime pool via auditExecutorOr. In stg/prod the
// unset case never reaches here — EnforceAuditRLSRoleFromEnv fails the boot.

import (
	"context"
	"errors"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
)

// buildAuditPool opens the dedicated app_audit pool from AUDIT_DATABASE_URL.
// Returns nil (and logs) when the DSN is unset or the pool cannot be opened,
// so cmd/server stays fail-soft in dev: a nil pool routes audit writes back
// through the runtime pool, which in dev is SUPERUSER/BYPASSRLS. The caller
// owns the returned pool and MUST Close it on shutdown.
func buildAuditPool(ctx context.Context, getenv func(string) string) *pgxpool.Pool {
	pool, err := postgresadapter.NewAuditFromEnv(ctx, getenv)
	if err != nil {
		if errors.Is(err, postgresadapter.ErrEmptyDSN) {
			log.Printf("crm: audit pool disabled (%s unset) — audit writes fall back to the runtime pool", postgresadapter.EnvAuditDSN)
		} else {
			log.Printf("crm: audit pool disabled — %v", err)
		}
		return nil
	}
	log.Printf("crm: dedicated app_audit pool opened (%s)", postgresadapter.EnvAuditDSN)
	return pool
}

// auditExecutorOr returns the dedicated audit pool when it is non-nil, else
// the supplied fallback (the runtime pool). It avoids the typed-nil trap: a
// nil *pgxpool.Pool must not be returned as a non-nil AuditExecutor
// interface, so the nil check is on the concrete pointer.
func auditExecutorOr(auditPool *pgxpool.Pool, fallback postgresadapter.AuditExecutor) postgresadapter.AuditExecutor {
	if auditPool != nil {
		return auditPool
	}
	return fallback
}
