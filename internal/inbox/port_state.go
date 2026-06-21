package inbox

import (
	"context"

	"github.com/google/uuid"
)

// ConversationStateStore persists the conversation lifecycle column
// (conversation.state) that the read-model and the metrics open/closed
// split project on. The Conversation aggregate owns the legal transitions
// (Close / Reopen); this port only writes the resulting state so the list
// pane's state filter and the metrics layer reflect an Encerrar / Reabrir
// action. The Postgres adapter MUST run under WithTenant so the UPDATE
// cannot touch another tenant's row.
//
// It is a sibling of ConversationLeadStore (port_attendants.go): both keep
// a denormalised column on the conversation row coherent with a domain
// transition without re-reading the whole aggregate.
type ConversationStateStore interface {
	// SetConversationState sets conversation.state to state for
	// (tenantID, conversationID). Returns ErrNotFound when no conversation
	// matches the tenant scope (RLS-hidden rows from other tenants collapse
	// to the same sentinel, preserving the no-cross-tenant-existence
	// posture). The CHECK constraint on conversation.state (migration 0088)
	// is the backstop for an out-of-range value; callers pass only the two
	// ConversationState constants.
	SetConversationState(ctx context.Context, tenantID, conversationID uuid.UUID, state ConversationState) error
}
