package engine

import (
	"context"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/funnel/rules"
)

// RuleResolver is the cascade resolver the engine reads rules from.
// The narrow interface mirrors *rules.Resolver so the engine never
// pulls a concrete dependency in its tests — the package's own resolver
// satisfies it directly.
type RuleResolver interface {
	Resolve(ctx context.Context, in rules.ResolveInput) ([]rules.ResolvedRule, error)
}

// ApplicationsRepo is the storage port for funnel_rule_applications.
// IsApplied short-circuits the apply path before the action dispatches;
// Record persists the row after a successful apply.
//
// The two-step shape (check, then record) lets the engine apply the
// action between the two calls without holding a transaction across
// the action — the funnel.Service.MoveConversation call has its own
// transaction boundary.
type ApplicationsRepo interface {
	// IsApplied returns true when funnel_rule_applications already
	// carries a row for (ruleID, messageID) under the tenant. Lookup
	// errors propagate; the engine treats them as transient and
	// returns the error so JetStream redelivers.
	IsApplied(ctx context.Context, tenantID, ruleID, messageID uuid.UUID) (bool, error)

	// Record inserts the application row. Returns [ErrAlreadyApplied]
	// on a UNIQUE conflict; the engine accepts that as a successful
	// no-op (a concurrent delivery on a different replica won the
	// race; the action ran at least once).
	Record(ctx context.Context, app Application) error
}

// StageMover is the action port. The production adapter wraps
// funnel.Service.MoveConversation; the test adapter records calls in a
// slice. Keeping the port in this package (rather than depending on
// funnel.Service directly) shields the engine from future changes to
// the funnel service signature and keeps the test surface narrow.
//
// The contract mirrors funnel.Service.MoveConversation: idempotent on
// a no-op move (already at the target stage), returns wrapped errors
// for missing stages or persistence failures.
type StageMover interface {
	MoveConversation(
		ctx context.Context,
		tenantID, conversationID uuid.UUID,
		toStageKey string,
		byUserID uuid.UUID,
		reason string,
	) error
}
