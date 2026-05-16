package wallet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/wallet"
)

var _ wallet.MasterGrantRepository = (*MasterGrantStore)(nil)

// MasterGrantStore is the pgx-backed adapter for wallet.MasterGrantRepository.
// All writes run under WithMasterOps because app_runtime has only SELECT
// access to master_grant (migration 0097).
type MasterGrantStore struct {
	pool    postgresadapter.TxBeginner
	actorID uuid.UUID
}

// NewMasterGrantStore constructs a MasterGrantStore over the master_ops pool.
// actorID is the master user performing the operations; it is passed to
// WithMasterOps so the audit trigger records attributable authorship.
func NewMasterGrantStore(masterOps *pgxpool.Pool, actorID uuid.UUID) (*MasterGrantStore, error) {
	if masterOps == nil {
		return nil, postgresadapter.ErrNilPool
	}
	if actorID == uuid.Nil {
		return nil, postgresadapter.ErrZeroActor
	}
	return &MasterGrantStore{pool: masterOps, actorID: actorID}, nil
}

// Create inserts a new master_grant row. The grant's ExternalID (ULID)
// must already be populated (NewMasterGrant does this).
func (s *MasterGrantStore) Create(ctx context.Context, g *wallet.MasterGrant) error {
	payloadJSON, err := json.Marshal(g.Payload())
	if err != nil {
		return fmt.Errorf("wallet/postgres: marshal master_grant payload: %w", err)
	}
	return postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO master_grant
			  (id, external_id, tenant_id, kind, payload, reason,
			   created_by_user_id, created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			g.ID(), g.ExternalID(), g.TenantID(), string(g.Kind()),
			payloadJSON, g.Reason(), g.CreatedByUserID(), g.CreatedAt(),
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return wallet.ErrIdempotencyConflict
			}
			return fmt.Errorf("wallet/postgres: insert master_grant: %w", err)
		}
		return nil
	})
}

// GetByID returns the MasterGrant with the given internal UUID.
func (s *MasterGrantStore) GetByID(ctx context.Context, id uuid.UUID) (*wallet.MasterGrant, error) {
	var g *wallet.MasterGrant
	err := postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		got, err := scanMasterGrant(tx.QueryRow(ctx, selectMasterGrantByID, id))
		if err != nil {
			return err
		}
		g = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return g, nil
}

// ListByTenant returns all grants for tenantID ordered by created_at DESC.
func (s *MasterGrantStore) ListByTenant(ctx context.Context, tenantID uuid.UUID) ([]*wallet.MasterGrant, error) {
	var out []*wallet.MasterGrant
	err := postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, selectMasterGrantsByTenant, tenantID)
		if err != nil {
			return fmt.Errorf("wallet/postgres: query master_grants by tenant: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			g, err := scanMasterGrantRow(rows)
			if err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if out == nil {
		out = []*wallet.MasterGrant{}
	}
	return out, nil
}

// Revoke persists the revocation triple for the grant, subject to the
// consumed/revoked guard (ADR-0098 §D4).
func (s *MasterGrantStore) Revoke(ctx context.Context, id, revokedByUserID uuid.UUID, revokeReason string, now time.Time) error {
	return postgresadapter.WithMasterOps(ctx, s.pool, s.actorID, func(tx pgx.Tx) error {
		// Read current state to surface domain-level errors before the update.
		g, err := scanMasterGrant(tx.QueryRow(ctx, selectMasterGrantByID, id))
		if err != nil {
			return err
		}
		if err := g.Revoke(revokedByUserID, revokeReason, now); err != nil {
			return err
		}
		ct, err := tx.Exec(ctx, `
			UPDATE master_grant
			   SET revoked_at = $1, revoked_by_user_id = $2, revoke_reason = $3
			 WHERE id = $4
			   AND revoked_at IS NULL
			   AND consumed_at IS NULL`,
			now, revokedByUserID, revokeReason, id,
		)
		if err != nil {
			return fmt.Errorf("wallet/postgres: update master_grant revoke: %w", err)
		}
		if ct.RowsAffected() == 0 {
			// Row was consumed or revoked between our read and the update.
			return wallet.ErrGrantAlreadyConsumed
		}
		return nil
	})
}

// --- queries ---------------------------------------------------------

const selectMasterGrantByID = `
	SELECT id, external_id, tenant_id, kind, payload, reason,
	       created_by_user_id, created_at,
	       consumed_at, consumed_ref,
	       revoked_at, revoked_by_user_id, revoke_reason
	  FROM master_grant
	 WHERE id = $1
`

const selectMasterGrantsByTenant = `
	SELECT id, external_id, tenant_id, kind, payload, reason,
	       created_by_user_id, created_at,
	       consumed_at, consumed_ref,
	       revoked_at, revoked_by_user_id, revoke_reason
	  FROM master_grant
	 WHERE tenant_id = $1
	 ORDER BY created_at DESC
`

func scanMasterGrant(row pgx.Row) (*wallet.MasterGrant, error) {
	return decodeMasterGrant(row.Scan)
}

func scanMasterGrantRow(rows pgx.Rows) (*wallet.MasterGrant, error) {
	return decodeMasterGrant(rows.Scan)
}

func decodeMasterGrant(scan func(...any) error) (*wallet.MasterGrant, error) {
	var (
		id, tenantID, createdByUserID uuid.UUID
		externalID, kind, reason      string
		payloadRaw                    []byte
		createdAt                     time.Time
		consumedAt                    *time.Time
		consumedRef                   *string
		revokedAt                     *time.Time
		revokedByUserID               *uuid.UUID
		revokeReason                  *string
	)
	if err := scan(
		&id, &externalID, &tenantID, &kind, &payloadRaw, &reason,
		&createdByUserID, &createdAt,
		&consumedAt, &consumedRef,
		&revokedAt, &revokedByUserID, &revokeReason,
	); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, wallet.ErrNotFound
		}
		return nil, fmt.Errorf("wallet/postgres: scan master_grant: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadRaw, &payload); err != nil {
		return nil, fmt.Errorf("wallet/postgres: unmarshal master_grant payload: %w", err)
	}
	consumedRefStr := ""
	if consumedRef != nil {
		consumedRefStr = *consumedRef
	}
	revokeReasonStr := ""
	if revokeReason != nil {
		revokeReasonStr = *revokeReason
	}
	return wallet.HydrateMasterGrant(
		id, externalID, tenantID, createdByUserID,
		wallet.MasterGrantKind(kind), payload, reason, createdAt,
		consumedAt, consumedRefStr,
		revokedAt, revokedByUserID, revokeReasonStr,
	), nil
}
