package dunning

// State is the dunning lifecycle state of a subscription. Values match
// migration 0102's subscription_dunning_states.state CHECK constraint
// exactly — adding a new state requires a new migration AND a new
// constant here AND a new entry in Severity().
type State string

const (
	// StateCurrent is the baseline: invoice paid (or grace period still
	// open). No banner, no restriction.
	StateCurrent State = "current"

	// StateWarn means an invoice is past due by at least the policy's
	// warn threshold (default D+1). The UI renders a yellow banner.
	// No functional restriction yet.
	StateWarn State = "warn"

	// StateSuspendedOutbound means the tenant can still receive
	// inbound messages but outbound sends are blocked (default D+7).
	// Protects COGS on channel + LLM spend while keeping incident
	// conversations alive.
	StateSuspendedOutbound State = "suspended_outbound"

	// StateSuspendedFull means the tenant is read-only — no sends, no
	// edits (default D+30). The UI surfaces the dunning banner on
	// every page.
	StateSuspendedFull State = "suspended_full"

	// StateCancelled means the subscription has been auto-cancelled
	// (default D+90). Terminal in this domain — no further
	// escalation, no override. A fresh subscription must be created
	// elsewhere (billing/subscription) to resume service.
	StateCancelled State = "cancelled"
)

// Severity returns the ordering rank used to compare states. Higher
// rank = more restricted. Used by the state machine to ensure
// Escalate only moves the row strictly more severe; downgrade happens
// via MarkPaid.
//
// Unknown states return -1 so callers can detect data corruption
// (hydrated rows whose state column no longer matches the canonical
// set) without panicking.
func (s State) Severity() int {
	switch s {
	case StateCurrent:
		return 0
	case StateWarn:
		return 1
	case StateSuspendedOutbound:
		return 2
	case StateSuspendedFull:
		return 3
	case StateCancelled:
		return 4
	default:
		return -1
	}
}

// IsTerminal reports whether the state allows further escalation. Only
// StateCancelled is terminal in the dunning domain.
func (s State) IsTerminal() bool { return s == StateCancelled }

// IsKnown reports whether s is one of the five canonical values. Used
// by Policy.Validate-equivalent checks at hydration time when adapters
// want to defensively reject corrupted rows.
func (s State) IsKnown() bool { return s.Severity() >= 0 }
