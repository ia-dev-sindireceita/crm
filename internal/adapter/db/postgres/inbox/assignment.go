package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/inbox"
)

// Compile-time assertion — Store also satisfies AssignmentRepository.
var _ domain.AssignmentRepository = (*Store)(nil)

// AppendHistory inserts a new row in assignment_history. The schema is
// append-only (no UnassignedAt column); "current leader" is derived
// from LatestAssignment. Returns ErrInvalidLeadReason for an unknown
// reason so the caller does not have to ferry SQLSTATE 23514 around.
func (s *Store) AppendHistory(
	ctx context.Context,
	tenantID, conversationID, userID uuid.UUID,
	reason domain.LeadReason,
) (*domain.Assignment, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: AppendHistory: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: AppendHistory: conversation id is nil")
	}
	if userID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: AppendHistory: user id is nil")
	}
	if !reason.Valid() {
		return nil, domain.ErrInvalidLeadReason
	}
	a, err := domain.NewLeaderAssignment(tenantID, conversationID, userID, reason)
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: AppendHistory: %w", err)
	}
	a.AssignedAt = s.nowUTC()
	err = postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO assignment_history (id, tenant_id, conversation_id, user_id, assigned_at, reason)
			VALUES ($1, $2, $3, $4, $5, $6)
		`, a.ID, a.TenantID, a.ConversationID, a.UserID, a.AssignedAt, string(a.Reason))
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: AppendHistory: %w", err)
	}
	return a, nil
}

// LatestAssignment returns the most recent assignment_history row for
// (tenantID, conversationID) — served by the
// (tenant_id, conversation_id, assigned_at DESC) composite index. Returns
// ErrNotFound when no row exists.
func (s *Store) LatestAssignment(
	ctx context.Context,
	tenantID, conversationID uuid.UUID,
) (*domain.Assignment, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: LatestAssignment: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: LatestAssignment: conversation id is nil")
	}
	var out *domain.Assignment
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		row := tx.QueryRow(ctx, `
			SELECT id, user_id, assigned_at, reason
			  FROM assignment_history
			 WHERE tenant_id = $1 AND conversation_id = $2
			 ORDER BY assigned_at DESC
			 LIMIT 1
		`, tenantID, conversationID)
		var (
			id     uuid.UUID
			userID uuid.UUID
			at     time.Time
			reason string
		)
		if err := row.Scan(&id, &userID, &at, &reason); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return domain.ErrNotFound
			}
			return err
		}
		out = domain.HydrateLeaderAssignment(id, tenantID, conversationID, userID, at.UTC(), domain.LeadReason(reason))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: LatestAssignment: %w", err)
	}
	return out, nil
}

// ListHistory returns the full assignment_history projection for
// (tenantID, conversationID), oldest-first so the caller can pass the
// slice directly to Conversation.SetHistory. The composite index covers
// the DESC pattern; we reverse the ordering here so the domain receives
// rows in chronological order.
func (s *Store) ListHistory(
	ctx context.Context,
	tenantID, conversationID uuid.UUID,
) ([]*domain.Assignment, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListHistory: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListHistory: conversation id is nil")
	}
	var out []*domain.Assignment
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, user_id, assigned_at, reason
			  FROM assignment_history
			 WHERE tenant_id = $1 AND conversation_id = $2
			 ORDER BY assigned_at ASC
		`, tenantID, conversationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id     uuid.UUID
				userID uuid.UUID
				at     time.Time
				reason string
			)
			if err := rows.Scan(&id, &userID, &at, &reason); err != nil {
				return err
			}
			out = append(out, domain.HydrateLeaderAssignment(id, tenantID, conversationID, userID, at.UTC(), domain.LeadReason(reason)))
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListHistory: %w", err)
	}
	return out, nil
}
