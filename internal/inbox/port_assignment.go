package inbox

import (
	"context"

	"github.com/google/uuid"
)

// AssignmentRepository is the storage port for the append-only
// `assignment_history` ledger (F2-03, migration 0092). The Conversation
// aggregate is the domain owner of leadership transitions; this port
// lets the inbox use cases persist them and read them back without
// importing pgx.
//
// The Postgres adapter lives in internal/adapter/db/postgres/inbox and
// MUST run all three methods under WithTenant so the RLS policies on
// assignment_history (tenant_isolation_*) apply.
//
// All three methods are scoped by tenant_id; "current leader" is
// derived from LatestAssignment (`ORDER BY assigned_at DESC LIMIT 1`).
// AppendHistory is the only write seam — the schema is append-only and
// there is no UPDATE / DELETE path from the domain.
type AssignmentRepository interface {
	// AppendHistory inserts a new row in assignment_history for the
	// given (tenantID, conversationID, userID) tuple with the supplied
	// reason. Returns ErrInvalidLeadReason if reason fails Valid().
	// The returned Assignment carries the assigned_at chosen by the
	// adapter (typically `now() AT TIME ZONE 'UTC'`) so the caller
	// can update the in-memory Conversation.history slice.
	AppendHistory(
		ctx context.Context,
		tenantID, conversationID, userID uuid.UUID,
		reason LeadReason,
	) (*Assignment, error)

	// LatestAssignment returns the most recent assignment_history row
	// for (tenantID, conversationID). Returns ErrNotFound when the
	// conversation has no recorded leader yet.
	LatestAssignment(
		ctx context.Context,
		tenantID, conversationID uuid.UUID,
	) (*Assignment, error)

	// ListHistory returns the full assignment_history projection for
	// (tenantID, conversationID), ordered oldest-first so the caller
	// can pass it directly to Conversation.SetHistory. Returns an
	// empty slice (nil error) when the conversation has no history.
	ListHistory(
		ctx context.Context,
		tenantID, conversationID uuid.UUID,
	) ([]*Assignment, error)
}
