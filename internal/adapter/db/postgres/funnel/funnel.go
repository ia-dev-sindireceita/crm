// Package funnel is the pgx-backed adapter for the funnel.StageRepository
// and funnel.TransitionRepository ports (migration 0093:
// funnel_stage + funnel_transition).
//
// The package lives under internal/adapter/db/postgres/ so the
// forbidimport / notenant analyzers allow it to import pgx and call
// pgxpool methods directly. Every tenant-scoped call routes through
// the sibling postgres.WithTenant helper so the RLS GUC app.tenant_id
// is set before reading or writing.
//
// SIN-62792 (Fase 2 F2-08, child of SIN-62194).
package funnel

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/funnel"
)

// Compile-time assertions: Store satisfies both funnel ports. If a port
// grows or shrinks the build fails here before any caller notices.
var (
	_ domain.StageRepository      = (*Store)(nil)
	_ domain.TransitionRepository = (*Store)(nil)
)

// Store is the pgx-backed adapter for the funnel ports. Construct via
// New(pool); the pool MUST be the app_runtime pool so the RLS policies
// on funnel_stage / funnel_transition apply.
type Store struct {
	pool postgres.TxBeginner
}

// New wraps pool and returns a ready-to-use Store. nil pool yields
// postgres.ErrNilPool.
func New(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, postgres.ErrNilPool
	}
	return &Store{pool: pool}, nil
}

// FindByKey returns the funnel_stage row with (tenant_id, key). RLS
// hides rows from other tenants, so the result collapses to
// ErrNotFound for those just like a missing key.
func (s *Store) FindByKey(ctx context.Context, tenantID uuid.UUID, key string) (*domain.Stage, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("funnel/postgres: FindByKey: tenant id is nil")
	}
	if key == "" {
		return nil, domain.ErrNotFound
	}
	var stage *domain.Stage
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, key, label, position, is_default
			  FROM funnel_stage
			 WHERE key = $1
		`, key)
		st := &domain.Stage{}
		if err := row.Scan(
			&st.ID,
			&st.TenantID,
			&st.Key,
			&st.Label,
			&st.Position,
			&st.IsDefault,
		); err != nil {
			return err
		}
		stage = st
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("funnel/postgres: FindByKey: %w", err)
	}
	return stage, nil
}

// LatestForConversation returns the most-recent transition for the
// conversation, ordered by transitioned_at DESC (the (tenant_id,
// conversation_id, transitioned_at DESC) index satisfies this in one
// btree fetch). Returns ErrNotFound when the conversation has no
// transitions yet.
func (s *Store) LatestForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*domain.Transition, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("funnel/postgres: LatestForConversation: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, domain.ErrNotFound
	}
	var tr *domain.Transition
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, tenant_id, conversation_id, from_stage_id, to_stage_id,
			       transitioned_by_user_id, transitioned_at, reason
			  FROM funnel_transition
			 WHERE conversation_id = $1
			 ORDER BY transitioned_at DESC
			 LIMIT 1
		`, conversationID)
		t, err := scanTransition(row)
		if err != nil {
			return err
		}
		tr = t
		return nil
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("funnel/postgres: LatestForConversation: %w", err)
	}
	return tr, nil
}

// Create inserts a new transition row. The caller MUST set ID,
// TransitionedAt, and all FK fields; the adapter does not invent any.
func (s *Store) Create(ctx context.Context, t *domain.Transition) error {
	if t == nil {
		return fmt.Errorf("funnel/postgres: Create: nil transition")
	}
	if t.TenantID == uuid.Nil {
		return fmt.Errorf("funnel/postgres: Create: tenant id is nil")
	}
	if t.ID == uuid.Nil {
		return fmt.Errorf("funnel/postgres: Create: transition id is nil")
	}
	if t.ConversationID == uuid.Nil {
		return fmt.Errorf("funnel/postgres: Create: conversation id is nil")
	}
	if t.ToStageID == uuid.Nil {
		return fmt.Errorf("funnel/postgres: Create: to_stage_id is nil")
	}
	if t.TransitionedByUserID == uuid.Nil {
		return fmt.Errorf("funnel/postgres: Create: transitioned_by_user_id is nil")
	}
	if t.TransitionedAt.IsZero() {
		return fmt.Errorf("funnel/postgres: Create: transitioned_at is zero")
	}
	return postgres.WithTenant(ctx, s.pool, t.TenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO funnel_transition
			  (id, tenant_id, conversation_id, from_stage_id, to_stage_id,
			   transitioned_by_user_id, transitioned_at, reason)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`,
			t.ID,
			t.TenantID,
			t.ConversationID,
			fromStageArg(t.FromStageID),
			t.ToStageID,
			t.TransitionedByUserID,
			t.TransitionedAt,
			nullableReason(t.Reason),
		)
		if err != nil {
			return fmt.Errorf("funnel/postgres: Create: %w", err)
		}
		return nil
	})
}

// fromStageArg converts the optional FromStageID into a value the pgx
// driver maps to NULL when the pointer is nil; doing it via an
// interface{} keeps the query parameter list flat.
func fromStageArg(p *uuid.UUID) any {
	if p == nil {
		return nil
	}
	return *p
}

// nullableReason maps an empty reason to SQL NULL; the column is
// nullable and we prefer NULL over a row of blanks for downstream
// audit clarity.
func nullableReason(reason string) any {
	if reason == "" {
		return nil
	}
	return reason
}

// scanTransition decodes a row from funnel_transition into the domain
// struct. from_stage_id and reason are nullable, so we scan into
// pointer targets and copy into the result only when present.
func scanTransition(row pgx.Row) (*domain.Transition, error) {
	var (
		t      domain.Transition
		fromID *uuid.UUID
		reason *string
	)
	if err := row.Scan(
		&t.ID,
		&t.TenantID,
		&t.ConversationID,
		&fromID,
		&t.ToStageID,
		&t.TransitionedByUserID,
		&t.TransitionedAt,
		&reason,
	); err != nil {
		return nil, err
	}
	t.FromStageID = fromID
	if reason != nil {
		t.Reason = *reason
	}
	return &t, nil
}
