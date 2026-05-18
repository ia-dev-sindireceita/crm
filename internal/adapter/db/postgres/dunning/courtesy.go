package dunning

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	billingdunning "github.com/pericles-luz/crm/internal/billing/dunning"
)

// CourtesyOverrideStore implements billingdunning.CourtesyOverride
// against master_grant. It returns the live free_subscription_period
// reprieve (if any) so the dunning tick can ApplyOverride or pause
// escalation.
//
// Override semantics (ADR-0098 §D1 + ADR-0093):
//
//   - kind = 'free_subscription_period'
//   - revoked_at IS NULL
//   - payload-derived Until > now
//
// The grant payload is {"months": N, "plan_id": "..."} per ADR-0098.
// Until is computed as created_at + N months. consumed_at is ignored:
// the wallet domain marks the grant consumed when the ledger entry
// lands, but the dunning reprieve runs for the full N months
// regardless.
type CourtesyOverrideStore struct {
	master *pgxpool.Pool
}

// NewCourtesyOverrideStore constructs the store. masterPool MUST be
// the app_master_ops pool: the query is cross-tenant and the master
// role has SELECT on master_grant.
func NewCourtesyOverrideStore(masterPool *pgxpool.Pool) (*CourtesyOverrideStore, error) {
	if masterPool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &CourtesyOverrideStore{master: masterPool}, nil
}

var _ billingdunning.CourtesyOverride = (*CourtesyOverrideStore)(nil)

// ActiveFor returns the live reprieve for tenantID at now, or
// billingdunning.ErrNoActiveOverride. The most recent (latest
// created_at) grant wins so a master who issues two overlapping grants
// gets predictable semantics.
func (s *CourtesyOverrideStore) ActiveFor(ctx context.Context, tenantID uuid.UUID, now time.Time) (billingdunning.Override, error) {
	if tenantID == uuid.Nil {
		return billingdunning.Override{}, billingdunning.ErrZeroTenant
	}
	rows, err := s.master.Query(ctx, `
		SELECT created_at, payload, reason
		  FROM master_grant
		 WHERE tenant_id = $1
		   AND kind = 'free_subscription_period'
		   AND revoked_at IS NULL
		 ORDER BY created_at DESC
		 LIMIT 16`, tenantID)
	if err != nil {
		return billingdunning.Override{}, fmt.Errorf("dunning/postgres: courtesy lookup: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			createdAt time.Time
			payload   []byte
			reason    string
		)
		if err := rows.Scan(&createdAt, &payload, &reason); err != nil {
			return billingdunning.Override{}, fmt.Errorf("dunning/postgres: scan grant: %w", err)
		}
		months, ok := DecodeMonths(payload)
		if !ok {
			continue
		}
		until := createdAt.AddDate(0, months, 0)
		if until.After(now) {
			return billingdunning.Override{Until: until, Reason: reason}, nil
		}
	}
	if err := rows.Err(); err != nil {
		return billingdunning.Override{}, fmt.Errorf("dunning/postgres: courtesy lookup: %w", err)
	}
	return billingdunning.Override{}, billingdunning.ErrNoActiveOverride
}

// DecodeMonths extracts the integer "months" field from the grant
// payload. The ADR-0098 schema is {"months": N, "plan_id": "..."}; we
// accept any positive integer (JSON numbers decode to float64). A
// missing or non-positive value yields (0, false) so the caller skips
// the row.
//
// Exported so the parent postgres_test package can table-drive the
// payload decoder without an integration test — see
// dunning_unit_test.go (SIN-62965). Subpackage *_test.go files under
// internal/adapter/db/postgres/ are forbidden by SIN-62750 to avoid
// the shared CI cluster's ALTER ROLE race.
func DecodeMonths(payload []byte) (int, bool) {
	if len(payload) == 0 {
		return 0, false
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return 0, false
	}
	raw, ok := obj["months"]
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		if v <= 0 || v > 120 {
			return 0, false
		}
		return int(v), true
	case int:
		if v <= 0 || v > 120 {
			return 0, false
		}
		return v, true
	}
	return 0, false
}
