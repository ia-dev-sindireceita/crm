package dunning

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Override is the value type that projects an active
// CourtesyGrant.kind=free_subscription_period grant onto the dunning
// domain. Adapters construct it from a wallet.MasterGrant; the dunning
// state machine consumes it via DunningState.Escalate.
//
// A nil *Override means "no override". A non-nil *Override with Until
// in the past is also treated as "no override" by Escalate — adapters
// may either filter such grants out at query time (preferred) or pass
// them through and let the state machine ignore them.
type Override struct {
	// Until is the inclusive end of the override window: escalation
	// is paused while Until > now. After Until, the cron resumes
	// based on the actual past-due window.
	Until time.Time

	// Reason mirrors the wallet.MasterGrant.Reason for audit. ≥10
	// chars by construction (the wallet domain enforces this).
	Reason string
}

// DunningRepository is the persistence port for DunningState. There
// is exactly one row per subscription (UNIQUE constraint on
// subscription_id in migration 0102), so Save is an upsert keyed on
// SubscriptionID rather than the row id.
//
// Reads are tenant-scoped (app_runtime role with RLS). Writes require
// the master_ops role and record actorID in the audit trail
// (master_ops_audit_trigger on subscription_dunning_states fires the
// row insert into master_ops_audit_log).
//
// Implementations MUST translate:
//
//   - "no rows"            → ErrNotFound
//   - CHECK constraint violations on state → ErrInvalidTransition
//     (defence in depth — the domain should have caught it first).
type DunningRepository interface {
	// GetBySubscription returns the dunning row for subscriptionID,
	// or ErrNotFound. Returns ErrZeroSubscription for uuid.Nil.
	GetBySubscription(ctx context.Context, subscriptionID uuid.UUID) (*DunningState, error)

	// Save inserts or updates the dunning row. actorID is recorded in
	// the master_ops audit trail. Implementations MUST run inside
	// WithMasterOps so the audit trigger fires.
	Save(ctx context.Context, d *DunningState, actorID uuid.UUID) error
}

// CourtesyOverride is the read-only port the dunning cron worker uses
// to fetch the active free_subscription_period grant for a tenant.
//
// Implementations live next to the wallet adapter; they consult
// master_grant (migration 0097) for rows where:
//
//   - kind = 'free_subscription_period'
//   - revoked_at IS NULL
//   - consumed_at IS NULL (or consumed_at IS NOT NULL but the override
//     window encoded in payload still extends past now — adapter call)
//   - payload-derived until > now
//
// Returns ErrNoActiveOverride when no qualifying grant exists. The
// dunning state machine treats ErrNoActiveOverride as "no override"
// rather than as an error.
type CourtesyOverride interface {
	// ActiveFor returns the live override for tenantID at now, or
	// ErrNoActiveOverride. Returns ErrZeroTenant for uuid.Nil.
	ActiveFor(ctx context.Context, tenantID uuid.UUID, now time.Time) (Override, error)
}
