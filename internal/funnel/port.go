package funnel

import (
	"context"

	"github.com/google/uuid"
)

// StageRepository is the storage port for funnel_stage rows. The
// concrete adapter lives in internal/adapter/db/postgres/funnel; the
// service depends only on this interface so unit tests can substitute
// an in-memory fake.
type StageRepository interface {
	// FindByKey returns the stage with key under the given tenant
	// scope, or ErrNotFound when no row matches. The (tenant_id, key)
	// uniqueness from migration 0093 guarantees at most one row.
	FindByKey(ctx context.Context, tenantID uuid.UUID, key string) (*Stage, error)
}

// TransitionRepository is the storage port for funnel_transition rows.
type TransitionRepository interface {
	// LatestForConversation returns the most-recent transition for the
	// conversation (highest transitioned_at), or ErrNotFound when the
	// conversation has never been moved yet (= "no current stage").
	LatestForConversation(ctx context.Context, tenantID, conversationID uuid.UUID) (*Transition, error)

	// Create persists a brand-new transition. The caller MUST construct
	// it with a fresh uuid and a TransitionedAt timestamp from the
	// service clock — the repository never invents either.
	Create(ctx context.Context, t *Transition) error
}

// EventPublisher fan-outs domain events to downstream consumers. The
// funnel service emits funnel.conversation_moved on every successful
// transition. Publish errors are returned to the caller wrapped so
// callers can decide whether to retry; the persistent ledger row is
// the source of truth, the event is a best-effort notification.
type EventPublisher interface {
	Publish(ctx context.Context, eventName string, payload any) error
}
