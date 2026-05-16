package aipolicy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/aipolicy"
)

// AuditStore is the pgx-backed adapter for ai_policy_audit. It
// satisfies both domain.AuditLogger (write path used by the
// RecordingRepository decorator) and domain.AuditQuery (read path
// used by the admin views).
//
// Construct via NewAuditStore. RuntimePool MUST be the app_runtime
// pool so RLS gates SELECT/INSERT by app.tenant_id; MasterOpsPool is
// optional and routes the cross-tenant Purge through app_master_ops
// (the master_ops_audit ledger captures the sweep). When MasterOpsPool
// is nil, Purge returns ErrPurgeUnavailable so a misconfigured wire
// fails loudly rather than silently skipping the LGPD job.
type AuditStore struct {
	runtimePool   postgres.TxBeginner
	masterOpsPool postgres.TxBeginner
}

// ErrPurgeUnavailable is returned by Purge when the AuditStore was
// constructed without a master-ops pool. The LGPD job is the only
// caller; production wiring MUST supply both pools.
var ErrPurgeUnavailable = fmt.Errorf("aipolicy/postgres: master_ops pool not configured")

var (
	_ domain.AuditLogger = (*AuditStore)(nil)
	_ domain.AuditQuery  = (*AuditStore)(nil)
)

// NewAuditStore wraps runtimePool and returns a ready AuditStore. A
// nil runtimePool yields postgres.ErrNilPool. masterOpsPool is
// optional; pass nil when wiring the read/write surface for HTTP
// handlers and supply it from the LGPD worker so cross-tenant Purge
// stays out of the request path.
func NewAuditStore(runtimePool, masterOpsPool postgres.TxBeginner) (*AuditStore, error) {
	if runtimePool == nil {
		return nil, postgres.ErrNilPool
	}
	return &AuditStore{runtimePool: runtimePool, masterOpsPool: masterOpsPool}, nil
}

// Record persists one AuditEvent. WithTenant is invoked so RLS
// allows the INSERT; the underlying GRANT denies UPDATE, so a stray
// UPDATE attempt would be caught at the SQL layer.
//
// The function rejects malformed inputs (zero tenant, invalid scope
// type, blank scope id, blank field) with typed errors so the
// decorator can refuse the policy write before the audit hole
// appears.
func (s *AuditStore) Record(ctx context.Context, ev domain.AuditEvent) error {
	if ev.TenantID == uuid.Nil {
		return fmt.Errorf("aipolicy/postgres: Record: %w", domain.ErrInvalidTenant)
	}
	if !ev.ScopeType.IsValid() {
		return fmt.Errorf("aipolicy/postgres: Record: %w", domain.ErrInvalidScopeType)
	}
	if strings.TrimSpace(ev.ScopeID) == "" {
		return fmt.Errorf("aipolicy/postgres: Record: %w", domain.ErrInvalidScopeID)
	}
	if strings.TrimSpace(ev.Field) == "" {
		return fmt.Errorf("aipolicy/postgres: Record: field is required")
	}
	if !ev.Actor.IsValid() {
		return fmt.Errorf("aipolicy/postgres: Record: %w", domain.ErrMissingActor)
	}

	oldJSON, err := encodeJSONB(ev.OldValue)
	if err != nil {
		return fmt.Errorf("aipolicy/postgres: Record: encode old_value: %w", err)
	}
	newJSON, err := encodeJSONB(ev.NewValue)
	if err != nil {
		return fmt.Errorf("aipolicy/postgres: Record: encode new_value: %w", err)
	}
	occurredAt := ev.OccurredAt
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}

	return postgres.WithTenant(ctx, s.runtimePool, ev.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO ai_policy_audit
			  (tenant_id, scope_kind, scope_id, field,
			   old_value, new_value, actor_user_id, actor_master,
			   created_at)
			VALUES ($1, $2, $3, $4, $5::jsonb, $6::jsonb, $7, $8, $9)
		`,
			ev.TenantID,
			string(ev.ScopeType),
			ev.ScopeID,
			ev.Field,
			oldJSON,
			newJSON,
			ev.Actor.UserID,
			ev.Actor.Master,
			occurredAt,
		)
		if err != nil {
			return fmt.Errorf("aipolicy/postgres: Record: %w", err)
		}
		return nil
	})
}

// Page returns up to q.Limit rows ordered by (created_at DESC, id
// DESC) starting after q.Cursor. The next cursor is the (created_at,
// id) pair of the last row when more rows might exist, otherwise the
// zero cursor.
//
// q.TenantID is required; ScopeType + ScopeID are optional filters
// (both must be present to engage the scope index). Since / Until
// bound created_at; zero values mean "open-ended".
func (s *AuditStore) Page(ctx context.Context, q domain.AuditPageQuery) (domain.AuditPage, error) {
	zero := domain.AuditPage{}
	if q.TenantID == uuid.Nil {
		return zero, fmt.Errorf("aipolicy/postgres: Page: %w", domain.ErrInvalidTenant)
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	// Build the predicate string + args incrementally so the planner
	// sees a stable shape per query variant.
	where := []string{"true"}
	args := []any{}
	add := func(predicate string, arg any) {
		args = append(args, arg)
		where = append(where, strings.ReplaceAll(predicate, "?", fmt.Sprintf("$%d", len(args))))
	}
	if q.ScopeType.IsValid() && strings.TrimSpace(q.ScopeID) != "" {
		add("scope_kind = ?", string(q.ScopeType))
		add("scope_id   = ?", q.ScopeID)
	}
	if !q.Since.IsZero() {
		add("created_at >= ?", q.Since)
	}
	if !q.Until.IsZero() {
		add("created_at < ?", q.Until)
	}
	if !q.Cursor.IsZero() {
		// Keyset continuation: rows strictly before the cursor under
		// the (created_at DESC, id DESC) order.
		add("created_at < ?", q.Cursor.CreatedAt)
		// Reuse the same created_at predicate so equal timestamps
		// disambiguate by id.
		args = append(args, q.Cursor.CreatedAt, q.Cursor.ID)
		where[len(where)-1] = fmt.Sprintf(
			"(created_at < $%d OR (created_at = $%d AND id < $%d))",
			len(args)-2, len(args)-1, len(args),
		)
	}

	// Fetch limit+1 rows so we can tell whether a next page exists
	// without a separate COUNT.
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT id, tenant_id, scope_kind, scope_id, field,
		       old_value, new_value,
		       coalesce(actor_user_id, '00000000-0000-0000-0000-000000000000'::uuid),
		       actor_master, created_at
		  FROM ai_policy_audit
		 WHERE %s
		 ORDER BY created_at DESC, id DESC
		 LIMIT $%d
	`, strings.Join(where, " AND "), len(args))

	var page domain.AuditPage
	err := postgres.WithTenant(ctx, s.runtimePool, q.TenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, query, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				rec      domain.AuditRecord
				scopeStr string
				oldRaw   []byte
				newRaw   []byte
			)
			if err := rows.Scan(
				&rec.ID,
				&rec.TenantID,
				&scopeStr,
				&rec.ScopeID,
				&rec.Field,
				&oldRaw,
				&newRaw,
				&rec.ActorUserID,
				&rec.ActorMaster,
				&rec.CreatedAt,
			); err != nil {
				return err
			}
			rec.ScopeType = domain.ScopeType(scopeStr)
			rec.OldValue = decodeJSONB(oldRaw)
			rec.NewValue = decodeJSONB(newRaw)
			page.Events = append(page.Events, rec)
		}
		return rows.Err()
	})
	if err != nil {
		return zero, fmt.Errorf("aipolicy/postgres: Page: %w", err)
	}

	if len(page.Events) > limit {
		last := page.Events[limit-1]
		page.Events = page.Events[:limit]
		page.Next = domain.AuditCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

// Purge deletes ai_policy_audit rows older than olderThan for every
// tenant. The function is intended for the LGPD retention job
// (decisão #3, 12-month default — configurable per tenant via
// tenants.audit_data_retention_months). It routes through
// WithMasterOps because the sweep crosses tenants; the master_ops
// audit ledger captures the cross-tenant DELETE.
//
// Returns the number of rows deleted. actorID is the system user the
// LGPD job runs as.
func (s *AuditStore) Purge(ctx context.Context, actorID uuid.UUID, olderThan time.Time) (int64, error) {
	if olderThan.IsZero() {
		return 0, fmt.Errorf("aipolicy/postgres: Purge: olderThan is required")
	}
	if s.masterOpsPool == nil {
		return 0, ErrPurgeUnavailable
	}
	var deleted int64
	err := postgres.WithMasterOps(ctx, s.masterOpsPool, actorID, func(tx pgx.Tx) error {
		ct, err := tx.Exec(ctx, `
			DELETE FROM ai_policy_audit
			 WHERE created_at < $1
		`, olderThan)
		if err != nil {
			return err
		}
		deleted = ct.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("aipolicy/postgres: Purge: %w", err)
	}
	return deleted, nil
}

// encodeJSONB renders v as the raw bytes Postgres should store. nil
// stays as the SQL NULL surrogate `null` (the column default).
func encodeJSONB(v any) ([]byte, error) {
	if v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(v)
}

// decodeJSONB parses the raw jsonb bytes back into a Go value. A
// nil/empty payload decodes to nil so the reader can distinguish
// "no prior value" (FieldCreated) from "empty string".
func decodeJSONB(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		// Persisted rows are always written through encodeJSONB
		// above; an unparseable payload is a database-level
		// corruption, not a runtime branch. Return the raw string so
		// the admin view does not 500 on a single bad row.
		return string(b)
	}
	return v
}
