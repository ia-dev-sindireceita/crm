package inbox

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/pericles-luz/crm/internal/adapter/db/postgres"
	domain "github.com/pericles-luz/crm/internal/inbox"
)

// inboxRoles are the tenant roles eligible to lead a conversation. It
// mirrors the iam.ActionTenantInboxRead matrix (tenant_atendente /
// tenant_gerente). Kept as a SQL-side literal list so the role gate is a
// single indexed predicate; the canonical Go names live in internal/iam.
const (
	roleTenantAtendente = "tenant_atendente"
	roleTenantGerente   = "tenant_gerente"
)

// ListAssignable returns the tenant's assignable attendants — users whose
// role is tenant_atendente or tenant_gerente — ordered by display label.
// The query runs under WithTenant so RLS restricts the users table to the
// tenant scope; the role predicate narrows further. DisplayName is derived
// from the email via domain.UserLabelFromEmail (no display-name column on
// users), matching the inbox list pane's atendente label so the dropdown
// and the badge read identically.
func (s *Store) ListAssignable(ctx context.Context, tenantID uuid.UUID) ([]domain.AssignableAttendant, error) {
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("inbox/postgres: ListAssignable: tenant id is nil")
	}
	var out []domain.AssignableAttendant
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, email::text
			  FROM users
			 WHERE role IN ($1, $2)
			 ORDER BY email ASC
		`, roleTenantAtendente, roleTenantGerente)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var (
				id    uuid.UUID
				email string
			)
			if err := rows.Scan(&id, &email); err != nil {
				return err
			}
			out = append(out, domain.AssignableAttendant{
				UserID:      id,
				DisplayName: domain.UserLabelFromEmail(email),
			})
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("inbox/postgres: ListAssignable: %w", err)
	}
	return out, nil
}

// IsAssignable reports whether userID is an assignable attendant under
// tenantID. It runs under WithTenant, so a user from another tenant is
// RLS-hidden and collapses to false — the deny-by-default tenant-isolation
// guarantee the AssignConversation use case relies on. A user that exists
// under the tenant but whose role is not an inbox role also returns false.
func (s *Store) IsAssignable(ctx context.Context, tenantID, userID uuid.UUID) (bool, error) {
	if tenantID == uuid.Nil {
		return false, fmt.Errorf("inbox/postgres: IsAssignable: tenant id is nil")
	}
	if userID == uuid.Nil {
		return false, nil
	}
	var ok bool
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM users
				 WHERE id = $1 AND role IN ($2, $3)
			)
		`, userID, roleTenantAtendente, roleTenantGerente).Scan(&ok)
	})
	if err != nil {
		return false, fmt.Errorf("inbox/postgres: IsAssignable: %w", err)
	}
	return ok, nil
}

// SetConversationLead updates the denormalised conversation.assigned_user_id
// cache so the inbox list read-model reflects the latest manual
// (re)assignment. It runs under WithTenant; the UPDATE's RLS USING clause
// scopes the row to the tenant, and a zero rows-affected result means no
// conversation matched the tenant scope (unknown id or RLS-hidden) — mapped
// to domain.ErrNotFound, mirroring GetConversation's no-cross-tenant-
// existence posture. The append-only assignment_history ledger remains the
// audit source of truth; this method only keeps the cached lead coherent.
func (s *Store) SetConversationLead(ctx context.Context, tenantID, conversationID, userID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SetConversationLead: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SetConversationLead: conversation id is nil")
	}
	if userID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SetConversationLead: user id is nil")
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE conversation
			   SET assigned_user_id = $1
			 WHERE id = $2
		`, userID, conversationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("inbox/postgres: SetConversationLead: %w", err)
	}
	return nil
}

// ClearConversationLead nulls the denormalised
// conversation.assigned_user_id so the inbox list read-model
// (ListConversationSummaries) and the "fila / não atribuídas" queue see
// the conversation as Unassigned again. It is the release half of
// SetConversationLead: SetConversationLead names a user, ClearConversationLead
// removes whoever currently leads. Two callers use it — the
// "Transferir conversa" action (return the thread to the queue, SIN-65471
// AC#4) and the fakellm reset (drop the assignment when the training thread
// is wiped, SIN-65471 AC#1).
//
// The append-only assignment_history ledger is the audit source of truth and
// is intentionally left untouched: it has no nil-user "unassigned" row shape
// (AppendHistory rejects uuid.Nil), so a release is recorded only on the
// cached lead column the read-model projects on. Idempotent: clearing a
// conversation that is already unassigned affects one row and returns nil.
//
// It runs under WithTenant; the UPDATE's RLS USING clause scopes the row to
// the tenant, and a zero rows-affected result means no conversation matched
// the tenant scope (unknown id or RLS-hidden) — mapped to domain.ErrNotFound,
// mirroring SetConversationLead's no-cross-tenant-existence posture.
func (s *Store) ClearConversationLead(ctx context.Context, tenantID, conversationID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: ClearConversationLead: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: ClearConversationLead: conversation id is nil")
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE conversation
			   SET assigned_user_id = NULL
			 WHERE id = $1
		`, conversationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("inbox/postgres: ClearConversationLead: %w", err)
	}
	return nil
}

// SetConversationState updates conversation.state for (tenantID,
// conversationID). It backs the "Encerrar conversa" action (SIN-65471 AC#4):
// the CloseConversation use case loads the conversation, applies the domain
// transition (Conversation.Close), and persists the new state through this
// method. The DB CHECK constraint (migration 0088) rejects any value outside
// {open, closed}, so an invalid state surfaces as an adapter error rather
// than corrupting the row.
//
// It runs under WithTenant; a zero rows-affected result means no conversation
// matched the tenant scope (unknown id or RLS-hidden) — mapped to
// domain.ErrNotFound, the same no-cross-tenant-existence posture as the other
// conversation mutators. Idempotent: writing the state a conversation already
// holds affects one row and returns nil.
func (s *Store) SetConversationState(ctx context.Context, tenantID, conversationID uuid.UUID, state domain.ConversationState) error {
	if tenantID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SetConversationState: tenant id is nil")
	}
	if conversationID == uuid.Nil {
		return fmt.Errorf("inbox/postgres: SetConversationState: conversation id is nil")
	}
	err := postgres.WithTenant(ctx, s.pool, tenantID, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			UPDATE conversation
			   SET state = $1
			 WHERE id = $2
		`, string(state), conversationID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return domain.ErrNotFound
		}
		return nil
	})
	if errors.Is(err, domain.ErrNotFound) {
		return err
	}
	if err != nil {
		return fmt.Errorf("inbox/postgres: SetConversationState: %w", err)
	}
	return nil
}
