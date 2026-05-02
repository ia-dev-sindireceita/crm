package grant

import "time"

// Default cap and threshold constants (SIN-62241 acceptance criteria).
//
// Window definitions:
//   - SubscriptionCap is enforced over a rolling 90 day window per
//     subscription. Sum of granted+approved tokens in that window plus the
//     pending request must not exceed SubscriptionCap.
//   - MasterCap is enforced over a rolling 365 day window per master. Sum
//     of granted+approved tokens in that window plus the pending request
//     must not exceed MasterCap.
//   - AlertThreshold triggers a Slack alert for any single grant whose
//     amount strictly exceeds the threshold (regardless of decision).
const (
	DefaultSubscriptionCap90Days int64         = 10_000_000
	DefaultMasterCap365Days      int64         = 100_000_000
	DefaultAlertThreshold        int64         = 1_000_000
	SubscriptionWindow           time.Duration = 90 * 24 * time.Hour
	MasterWindow                 time.Duration = 365 * 24 * time.Hour
)

// Caps are the absolute limits applied by MasterGrantPolicy.
type Caps struct {
	SubscriptionCap90Days int64
	MasterCap365Days      int64
	AlertThreshold        int64
}

// DefaultCaps returns the production caps documented in SIN-62241.
func DefaultCaps() Caps {
	return Caps{
		SubscriptionCap90Days: DefaultSubscriptionCap90Days,
		MasterCap365Days:      DefaultMasterCap365Days,
		AlertThreshold:        DefaultAlertThreshold,
	}
}

// CapBreach identifies which cap was breached. A request can breach both
// caps in the same evaluation; both are reported.
type CapBreach struct {
	Subscription bool
	Master       bool
}

// Any reports whether any cap was breached.
func (b CapBreach) Any() bool { return b.Subscription || b.Master }

// Reasons returns human-readable strings for each breached cap, suitable
// for inclusion in audit log payloads and Slack alerts.
func (b CapBreach) Reasons() []string {
	out := make([]string, 0, 2)
	if b.Subscription {
		out = append(out, "subscription_cap_exceeded")
	}
	if b.Master {
		out = append(out, "master_cap_exceeded")
	}
	return out
}

// Decision is the outcome of MasterGrantPolicy.Evaluate. It does not
// contain side effects: callers (the service layer) decide what to persist
// and which adapters to invoke.
type Decision struct {
	Status      Status
	Breach      CapBreach
	AlertWorthy bool
}

// MasterGrantPolicy is the pure-domain policy applying caps and the
// approval-gate decision. It has no I/O.
type MasterGrantPolicy struct {
	caps            Caps
	approvalEnabled bool
}

// NewPolicy constructs a MasterGrantPolicy with the given caps. When
// approvalEnabled is true (F6 ratify-flow available), above-cap requests
// are parked as StatusPendingApproval; otherwise they are denied with
// StatusDeniedCapExceeded.
func NewPolicy(caps Caps, approvalEnabled bool) MasterGrantPolicy {
	return MasterGrantPolicy{caps: caps, approvalEnabled: approvalEnabled}
}

// Caps returns a copy of the configured caps.
func (p MasterGrantPolicy) Caps() Caps { return p.caps }

// ApprovalEnabled reports whether the F6 ratify-flow is wired in.
func (p MasterGrantPolicy) ApprovalEnabled() bool { return p.approvalEnabled }

// Evaluate applies the caps to the proposed request given the current
// rolling-window sums. The sums must NOT include the proposed amount; the
// policy adds it itself when comparing against the cap.
func (p MasterGrantPolicy) Evaluate(req Request, subscriptionWindowSum, masterWindowSum int64) Decision {
	breach := CapBreach{
		Subscription: subscriptionWindowSum+req.Amount > p.caps.SubscriptionCap90Days,
		Master:       masterWindowSum+req.Amount > p.caps.MasterCap365Days,
	}

	status := StatusGranted
	if breach.Any() {
		if p.approvalEnabled {
			status = StatusPendingApproval
		} else {
			status = StatusDeniedCapExceeded
		}
	}

	return Decision{
		Status:      status,
		Breach:      breach,
		AlertWorthy: req.Amount > p.caps.AlertThreshold,
	}
}
