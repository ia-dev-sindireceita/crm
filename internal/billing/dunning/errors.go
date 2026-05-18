package dunning

import "errors"

var (
	// ErrZeroTenant is returned when uuid.Nil is passed as a tenant id.
	ErrZeroTenant = errors.New("dunning: tenant id must not be uuid.Nil")

	// ErrZeroSubscription is returned when uuid.Nil is passed as a
	// subscription id.
	ErrZeroSubscription = errors.New("dunning: subscription id must not be uuid.Nil")

	// ErrZeroInvoice is returned by Escalate when a transition would
	// fire but the supplied invoice id is uuid.Nil. We require a
	// non-nil invoice so the row's last_invoice_id audit chain stays
	// intact.
	ErrZeroInvoice = errors.New("dunning: invoice id must not be uuid.Nil on escalation")

	// ErrInvalidTransition is returned when a mutation is rejected by
	// the state machine (e.g. MarkPaid on a cancelled row, or
	// ApplyOverride on a cancelled row).
	ErrInvalidTransition = errors.New("dunning: invalid state transition")

	// ErrInvalidPolicy is returned by Policy.Validate when thresholds
	// are non-positive or not strictly increasing.
	ErrInvalidPolicy = errors.New("dunning: policy thresholds must be strictly increasing positive days")

	// ErrOverrideReasonTooShort is returned by ApplyOverride when the
	// supplied reason is shorter than 10 characters (mirrors the DB
	// CHECK constraint subscription_dunning_states_override_consistency).
	ErrOverrideReasonTooShort = errors.New("dunning: override reason must be at least 10 characters")

	// ErrOverrideUntilInPast is returned by ApplyOverride when the
	// supplied until timestamp is not strictly after now.
	ErrOverrideUntilInPast = errors.New("dunning: override until must be in the future")

	// ErrNotFound is returned by the DunningRepository port when no
	// row exists for the requested subscription. Adapters MUST
	// translate "no rows" to this sentinel so callers can match with
	// errors.Is without importing pgx.
	ErrNotFound = errors.New("dunning: not found")

	// ErrNoActiveOverride is returned by the CourtesyOverride port
	// when the tenant has no active free_subscription_period grant.
	// Callers treat this as "no override" rather than as an error;
	// the sentinel is only used to disambiguate empty result from
	// connection errors.
	ErrNoActiveOverride = errors.New("dunning: no active courtesy override")
)
