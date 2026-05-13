package iam

import (
	"errors"
	"time"
)

// Timeouts pairs the idle and hard windows per role. Both are checked on
// every authenticated request; the stricter of the two wins.
//
// Idle = inactivity since the session's last request. Hard = wall-clock
// from session creation regardless of activity. ADR 0073 D3 fixes both.
type Timeouts struct {
	Idle time.Duration
	Hard time.Duration
}

// ErrSessionIdleTimeout is returned by CheckActivity when now-lastActivity
// reaches or exceeds Timeouts.Idle. Caller MUST clear the session cookie
// and redirect to login.
var ErrSessionIdleTimeout = errors.New("iam: session idle timeout")

// ErrSessionHardTimeout is returned by CheckActivity when now-createdAt
// reaches or exceeds Timeouts.Hard. Same caller action as the idle case;
// the distinct sentinel exists so metrics can split the two reasons.
var ErrSessionHardTimeout = errors.New("iam: session hard timeout")

// ErrUnknownRole is returned by TimeoutsForRole when r is not one of the
// four ADR 0073 D3 roles. Treat as fail-closed.
var ErrUnknownRole = errors.New("iam: unknown role")

// TimeoutsForRole returns the ADR 0073 D3 idle/hard pair for r. Future
// per-tenant overrides (the D5 reversibility hatch — auth.session.timeout.*
// config keys) belong in a separate config layer that wraps this function;
// the wrapper falls back to TimeoutsForRole's defaults so an unset key
// never opens a longer-than-default window.
func TimeoutsForRole(r Role) (Timeouts, error) {
	switch r {
	case RoleMaster:
		return Timeouts{Idle: 15 * time.Minute, Hard: 4 * time.Hour}, nil
	case RoleTenantGerente:
		return Timeouts{Idle: 30 * time.Minute, Hard: 8 * time.Hour}, nil
	case RoleTenantAtendente:
		return Timeouts{Idle: 60 * time.Minute, Hard: 12 * time.Hour}, nil
	case RoleTenantCommon:
		return Timeouts{Idle: 30 * time.Minute, Hard: 8 * time.Hour}, nil
	}
	return Timeouts{}, ErrUnknownRole
}

// CheckActivity validates a session against the per-role idle/hard
// windows. Returns nil when the session is still valid. Order of checks
// is hard-first because the hard cap is the absolute floor — once it
// trips, refreshing activity does not help; surfacing it lets the caller
// distinguish "user idle too long" from "device may be stolen".
//
// Boundary semantics: a session reaching exactly its window edge is
// REJECTED (>= Hard / >= Idle). The choice favours over-aggressive
// expiry over a single second of leeway; the user re-logs in once.
func CheckActivity(r Role, createdAt, lastActivity, now time.Time) error {
	t, err := TimeoutsForRole(r)
	if err != nil {
		return err
	}
	if now.Sub(createdAt) >= t.Hard {
		return ErrSessionHardTimeout
	}
	if now.Sub(lastActivity) >= t.Idle {
		return ErrSessionIdleTimeout
	}
	return nil
}
