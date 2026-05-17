package dunning

// Policy is the per-plan dunning configuration. All values are days
// past the invoice due date at which the corresponding state should
// be entered. Values must be strictly increasing positive integers;
// Validate enforces that.
//
// Default values come from D1 ([SIN-62204](/SIN/issues/SIN-62204)):
// {1, 7, 30, 90}. Plans may override individual thresholds (e.g. an
// enterprise plan that grants 14 days of grace before the warn banner)
// without a migration; the dunning row simply reads the plan's policy
// at evaluation time.
type Policy struct {
	// WarnDays is the threshold at which State transitions to warn.
	// Default 1.
	WarnDays int

	// OutboundBlockDays is the threshold at which outbound sends are
	// blocked (State suspended_outbound). Default 7.
	OutboundBlockDays int

	// ReadonlyDays is the threshold at which the tenant becomes
	// read-only (State suspended_full). Default 30.
	ReadonlyDays int

	// CancelDays is the threshold at which the subscription is
	// auto-cancelled (State cancelled). Default 90.
	CancelDays int
}

// DefaultPolicy is the {1, 7, 30, 90} schedule ratified in D1
// ([SIN-62204](/SIN/issues/SIN-62204)). Callers fall back to this when
// a plan does not customise its dunning thresholds.
var DefaultPolicy = Policy{
	WarnDays:          1,
	OutboundBlockDays: 7,
	ReadonlyDays:      30,
	CancelDays:        90,
}

// Validate reports whether the policy is internally consistent. All
// thresholds must be positive and strictly increasing
// (warn < outbound < readonly < cancel) — overlapping thresholds would
// make StateForDaysPastDue ambiguous and inverting them would make the
// state machine go backwards as time progresses.
func (p Policy) Validate() error {
	if p.WarnDays <= 0 ||
		p.OutboundBlockDays <= 0 ||
		p.ReadonlyDays <= 0 ||
		p.CancelDays <= 0 {
		return ErrInvalidPolicy
	}
	if !(p.WarnDays < p.OutboundBlockDays &&
		p.OutboundBlockDays < p.ReadonlyDays &&
		p.ReadonlyDays < p.CancelDays) {
		return ErrInvalidPolicy
	}
	return nil
}

// StateForDaysPastDue returns the most severe state whose threshold is
// at or below days. days is the number of full days the invoice is
// past its due date (DunningState.Escalate computes this as
// floor((now - dueDate) / 24h), clamped to 0).
//
// The thresholds are inclusive: at exactly WarnDays the state becomes
// warn, at exactly CancelDays the state becomes cancelled.
//
// Callers should pass a validated policy; if the policy is invalid
// (e.g. CancelDays < WarnDays) the result is the highest threshold
// satisfied and may surprise — Escalate calls Validate first as a
// guard.
func (p Policy) StateForDaysPastDue(days int) State {
	switch {
	case days >= p.CancelDays:
		return StateCancelled
	case days >= p.ReadonlyDays:
		return StateSuspendedFull
	case days >= p.OutboundBlockDays:
		return StateSuspendedOutbound
	case days >= p.WarnDays:
		return StateWarn
	default:
		return StateCurrent
	}
}
