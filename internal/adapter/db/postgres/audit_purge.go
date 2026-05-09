package postgres

// SIN-62424 / ADR 0004 §4: postgres-backed implementation of
// purge.Store. The sweep runs as app_master_ops because that is the
// role granted DELETE on audit_log_data in migration 0012, and the
// master_ops_audit_trigger fires per row so the cross-tenant deletion
// leaves an inspectable trail in master_ops_audit.
//
// audit_log_security is NEVER touched by this code path: the WHERE
// clause names audit_log_data only, and the trigger on
// audit_log_security records would never fire under app_master_ops
// because the delete statement does not target it. Adding a JOIN or
// USING clause that names audit_log_security would be a regression of
// AC #3 in SIN-62252 and is gated by the `_NeverTouchesSecurityLog`
// integration test.

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/audit/purge"
)

// LGPDPurgeActorID is the system actor UUID written into
// master_ops_audit rows when the LGPD purge job runs. It is a
// deterministic sentinel: master_ops_audit.actor_user_id has no FK
// constraint, so the row records "the purge job did this" with a
// stable identifier that operators can grep for. Mirrors the
// bootstrapMasterActorID pattern in cmd/server/wire.go (which uses
// ...000000000001 for login bootstrap; this picks a distinct value so
// purge events are unambiguous in the audit ledger).
const LGPDPurgeActorID = "00000000-0000-0000-0000-0000000c0de1"

// AuditPurgeStore is the postgres implementation of purge.Store.
//
// IMPORTANT: pool MUST be the dedicated app_master_ops pool. Wiring
// app_runtime here would fail closed because RLS blocks cross-tenant
// DELETEs and app_runtime has no DELETE grant on audit_log_data; the
// purge job is intentionally a master-ops operation so the
// master_ops_audit_trigger captures every row removed.
type AuditPurgeStore struct {
	pool    TxBeginner
	actorID uuid.UUID
}

// NewAuditPurgeStore wires AuditPurgeStore around a pool. ErrNilPool
// fires eagerly so cmd/server fails at boot rather than on the first
// scheduled sweep. ErrZeroActor mirrors WithMasterOps' precondition
// and prevents an accidentally-zero actor sneaking past
// master_ops_audit_trigger.
func NewAuditPurgeStore(pool TxBeginner, actorID uuid.UUID) (*AuditPurgeStore, error) {
	if pool == nil {
		return nil, ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, ErrZeroActor
	}
	return &AuditPurgeStore{pool: pool, actorID: actorID}, nil
}

// purgeSQL deletes every audit_log_data row whose tenant retention
// has elapsed. The USING-tenants clause joins each row to its tenant
// once so a single statement covers the cross-tenant sweep; the
// per-tenant retention column drives the cutoff. RETURNING tenant_id
// gives the caller a count of distinct tenants swept without a
// follow-up query.
//
// `make_interval(months => ...)` is calendar-aware, so a 12-month
// retention against a row dated 2025-04-30 evaluates against
// 2026-04-30 (not 360 days), matching how legal retention dates are
// reasoned about.
const purgeSQL = `
DELETE FROM audit_log_data d
USING tenants t
WHERE d.tenant_id = t.id
  AND d.occurred_at < ($1::timestamptz - make_interval(months => t.audit_data_retention_months))
RETURNING d.tenant_id
`

// PurgeExpired runs the LGPD retention sweep against audit_log_data
// and returns a Result describing how many rows were removed across
// how many tenants. The DELETE is wrapped in WithMasterOps so the
// master_ops_audit_trigger can record every row in master_ops_audit
// against the LGPD purge actor; without WithMasterOps, the trigger
// raises and aborts the transaction (see migration 0002).
//
// The whole sweep runs in one transaction. If any row fails the
// trigger preconditions the sweep aborts and no rows are deleted.
func (s *AuditPurgeStore) PurgeExpired(ctx context.Context, now time.Time) (purge.Result, error) {
	if s == nil || s.pool == nil {
		return purge.Result{}, ErrNilPool
	}

	var result purge.Result
	err := WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, purgeSQL, now)
		if err != nil {
			return fmt.Errorf("postgres: purge audit_log_data: %w", err)
		}
		defer rows.Close()

		tenants := make(map[uuid.UUID]struct{})
		var deleted int64
		for rows.Next() {
			var tenantID uuid.UUID
			if err := rows.Scan(&tenantID); err != nil {
				return fmt.Errorf("postgres: scan purge tenant: %w", err)
			}
			tenants[tenantID] = struct{}{}
			deleted++
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("postgres: iterate purge: %w", err)
		}

		result.DeletedRows = deleted
		result.TenantsSwept = len(tenants)
		return nil
	})
	if err != nil {
		return purge.Result{}, err
	}
	return result, nil
}
