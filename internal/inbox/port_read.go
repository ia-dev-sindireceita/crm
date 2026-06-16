package inbox

import (
	"context"

	"github.com/google/uuid"
)

// ConversationReadModel is the read-side (CQRS) port backing the inbox
// list pane (GET /inbox, SIN-64967). It is deliberately separate from
// Repository: the write-side aggregate port returns *Conversation, while
// the read side returns the flat ConversationListItem projection enriched
// with the last-message preview. Keeping the two ports apart lets the
// list view grow new facets (snippet, filters, future indicators) without
// widening the aggregate port every Repository implementation must
// satisfy.
type ConversationReadModel interface {
	// ListConversationSummaries returns up to `limit` conversation
	// projections under the tenant scope, newest-last-message-first,
	// narrowed by filter. The adapter computes the last-message snippet
	// and direction in a single query (no N+1). limit must be > 0; the
	// adapter clamps to a sane upper bound. RLS hides other tenants'
	// rows — they are simply absent from the result set.
	ListConversationSummaries(ctx context.Context, tenantID uuid.UUID, filter ConversationFilter, limit int) ([]ConversationListItem, error)
}

// UserDirectory resolves a set of tenant user IDs to display labels for
// the inbox list's assigned-atendente column (SIN-64967). It is a read
// port: the Postgres adapter implements it by reading the users table
// under the tenant scope. Implementations MUST be tenant-scoped — a label
// lookup must never cross tenants. IDs with no matching user are simply
// absent from the returned map, so the use case renders them with no
// label rather than failing the whole listing.
type UserDirectory interface {
	LabelsByID(ctx context.Context, tenantID uuid.UUID, ids []uuid.UUID) (map[uuid.UUID]string, error)
}
