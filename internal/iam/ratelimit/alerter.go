package ratelimit

import "context"

// Alerter is the synchronous notification port used to surface
// security-relevant rate-limit events. The only producer in SIN-62341
// is the master-endpoint lockout path: when an account_lockout row is
// written for a master user, Login fires Notify before returning so an
// operator sees the event in real time (acceptance criterion #3).
//
// The contract is deliberately tiny — one method, no severity / channel
// /metadata fields — so the domain stays storage- and transport-
// agnostic. Slack is the only adapter today (internal/adapter/notify/
// slack); routing, formatting, and rate-limiting of alerts belong in
// the adapter, not here.
//
// Implementations MUST honour the supplied context: the login flow
// passes a context with a tight deadline so a slow webhook does not
// stall the user-facing response. A returning Notify error is logged
// by the caller but does NOT abort the lockout itself — the persisted
// account_lockout row is the authoritative penalty; the alert is the
// notification side-effect.
type Alerter interface {
	Notify(ctx context.Context, msg string) error
}
