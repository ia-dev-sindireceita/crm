package funnel

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/funnel"
)

// Compile-time assertion — Store also satisfies BoardReader.
var _ domain.BoardReader = (*Store)(nil)

// Board materialises the full F2-12 board for tenantID in a single
// round trip: it joins funnel_stage with each conversation's *latest*
// transition (DISTINCT ON (conversation_id) ORDER BY transitioned_at
// DESC) and pulls the conversation + contact denormalised columns the
// UI needs. Stages without conversations still appear (the LEFT JOIN
// yields one row with NULL identifiers). Closed conversations are
// excluded so the operator's board reflects active work only.
//
// The query takes O(stages + conversations-in-funnel) plan rows. RLS
// filters everything else; the WHERE on s.tenant_id is for the planner.
func (s *Store) Board(ctx context.Context, tenantID uuid.UUID) (domain.Board, error) {
	if tenantID == uuid.Nil {
		return domain.Board{}, fmt.Errorf("funnel/postgres: Board: tenant id is nil")
	}
	stageOrder := make([]uuid.UUID, 0, 8)
	stageByID := map[uuid.UUID]*domain.BoardColumn{}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			WITH latest AS (
				SELECT DISTINCT ON (conversation_id)
				       conversation_id, to_stage_id
				  FROM funnel_transition
				 ORDER BY conversation_id, transitioned_at DESC
			)
			SELECT s.id, s.tenant_id, s.key, s.label, s.position, s.is_default,
			       c.id, c.channel, c.last_message_at,
			       ct.id, ct.display_name
			  FROM funnel_stage s
			  LEFT JOIN latest l ON l.to_stage_id = s.id
			  LEFT JOIN conversation c
			         ON c.id = l.conversation_id
			        AND c.state = 'open'
			  LEFT JOIN contact ct ON ct.id = c.contact_id
			 ORDER BY s.position ASC,
			          c.last_message_at DESC NULLS LAST,
			          c.id
		`)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var (
				st            domain.Stage
				convID        *uuid.UUID
				channel       *string
				lastMessageAt *time.Time
				contactID     *uuid.UUID
				displayName   *string
			)
			if err := rows.Scan(
				&st.ID, &st.TenantID, &st.Key, &st.Label, &st.Position, &st.IsDefault,
				&convID, &channel, &lastMessageAt, &contactID, &displayName,
			); err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			col, ok := stageByID[st.ID]
			if !ok {
				stageByID[st.ID] = &domain.BoardColumn{Stage: st}
				col = stageByID[st.ID]
				stageOrder = append(stageOrder, st.ID)
			}
			if convID == nil {
				continue
			}
			card := domain.ConversationCard{
				ConversationID: *convID,
			}
			if contactID != nil {
				card.ContactID = *contactID
			}
			if displayName != nil {
				card.DisplayName = *displayName
			}
			if channel != nil {
				card.Channel = *channel
			}
			if lastMessageAt != nil {
				card.LastMessageAt = *lastMessageAt
			}
			col.Cards = append(col.Cards, card)
		}
		return rows.Err()
	})
	if err != nil {
		return domain.Board{}, fmt.Errorf("funnel/postgres: Board: %w", err)
	}
	board := domain.Board{Columns: make([]domain.BoardColumn, 0, len(stageOrder))}
	for _, id := range stageOrder {
		board.Columns = append(board.Columns, *stageByID[id])
	}
	return board, nil
}

// ListForConversation returns every funnel_transition row for the
// conversation, ordered oldest-first so the history modal can render
// the ledger chronologically.
func (s *Store) ListForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) ([]*domain.Transition, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("funnel/postgres: ListForConversation: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return nil, nil
	}
	var out []*domain.Transition
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, tenant_id, conversation_id, from_stage_id, to_stage_id,
			       transitioned_by_user_id, transitioned_at, reason
			  FROM funnel_transition
			 WHERE conversation_id = $1
			 ORDER BY transitioned_at ASC
		`, conversationID)
		if err != nil {
			return fmt.Errorf("query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			tr, err := scanTransition(rows)
			if err != nil {
				return fmt.Errorf("scan: %w", err)
			}
			out = append(out, tr)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("funnel/postgres: ListForConversation: %w", err)
	}
	return out, nil
}
