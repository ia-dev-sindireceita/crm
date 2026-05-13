package iam

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/google/uuid"
)

// SESSION_TTL bounds. Reject anything outside [1m, 30d] at bootstrap so a
// typo in env (e.g. SESSION_TTL=24 missing the unit, parsed as 24ns by
// time.ParseDuration) cannot silently issue effectively-permanent or
// effectively-zero sessions.
const (
	defaultSessionTTL = 24 * time.Hour
	minSessionTTL     = 1 * time.Minute
	maxSessionTTL     = 30 * 24 * time.Hour
)

// EnvSessionTTL is the env var name read by ParseSessionTTL.
const EnvSessionTTL = "SESSION_TTL"

// Session is the in-memory shape of a row in the sessions table. ID is a
// version-4 UUID drawn from crypto/rand — see NewSessionID. The sessions
// table column is uuid-typed (migrations/0006), so the type matches the
// column without round-tripping through hex.
//
// ExpiresAt is the absolute deadline; renewal is by re-login only, not by
// sliding window (SIN-62213 keeps the model simple; sliding-window TTLs are
// a future PR).
type Session struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TenantID  uuid.UUID
	ExpiresAt time.Time
	CreatedAt time.Time
	IPAddr    net.IP
	UserAgent string

	// LastActivity is the timestamp of the most recent authenticated
	// request that landed against this session. The activity middleware
	// (internal/adapter/httpapi/middleware/activity.go) updates this on
	// every request, and CheckActivity reads it as the lastActivity
	// argument to enforce the per-role idle window (ADR 0073 §D3,
	// migration 0011_session_activity).
	//
	// SessionStore.Create persists the value; the column has DEFAULT
	// now() at the schema level, so a zero value at Create time is
	// translated to CreatedAt by the adapter (a fresh session is by
	// definition active "now").
	LastActivity time.Time

	// Role is the iam.Role of the session principal, denormalised onto
	// the session row at login time so the activity middleware can pick
	// the right idle/hard pair from TimeoutsForRole without a join.
	// Adapters MUST translate an empty value to RoleTenantCommon so a
	// caller that constructs Session directly (legacy tests, in-memory
	// fakes) does not write an empty role and trip the schema CHECK
	// constraint added in migration 0011_session_activity.
	Role Role

	// CSRFToken is the per-session CSPRNG token enforced on every
	// state-changing request (ADR 0073 §D1). Mint-on-login writes a
	// fresh value via iam/csrf.GenerateToken; rotation is per-session
	// (D1) — not per-request — to dodge the HTMX hx-swap race. The
	// HTTP layer mirrors this string into the __Host-csrf cookie, the
	// <meta name="csrf-token"> tag, and the X-CSRF-Token header.
	CSRFToken string
}

// IsExpired reports whether ExpiresAt is at or before now. ValidateSession
// uses this; tests use it to assert TTL boundaries.
func (s Session) IsExpired(now time.Time) bool {
	return !now.Before(s.ExpiresAt)
}

// SessionStore is the port for session persistence. The postgres adapter in
// internal/adapter/db/postgres implements this; in-memory fakes for tests
// can satisfy the interface without dragging pgx in.
//
// Each method is responsible for its own tenant scoping. Implementations
// MUST NOT trust caller-supplied tenant ids beyond what is already in the
// Session value: Create reads s.TenantID, Get/Delete take an explicit
// tenantID argument because the caller has already resolved it from the
// request host. Cross-tenant probes MUST collapse to ErrSessionNotFound so
// session ids cannot be enumerated across tenants via timing or error type.
type SessionStore interface {
	Create(ctx context.Context, s Session) error
	Get(ctx context.Context, tenantID, sessionID uuid.UUID) (Session, error)
	Delete(ctx context.Context, tenantID, sessionID uuid.UUID) error
	DeleteExpired(ctx context.Context, tenantID uuid.UUID) (int64, error)

	// Touch bumps Session.LastActivity to the supplied timestamp. The
	// activity middleware (SIN-62377 / FAIL-4) calls this on every
	// authenticated request that passes the per-role idle/hard window
	// check, so a session that is still in use does not become
	// idle-expired between requests. Returns ErrSessionNotFound when no
	// row matches the (tenantID, sessionID) pair — the caller then
	// clears the cookie and redirects to /login, matching the
	// "session vanished mid-flight" branch of the auth middleware.
	Touch(ctx context.Context, tenantID, sessionID uuid.UUID, lastActivity time.Time) error
}

// NewSessionID returns a fresh version-4 UUID drawn from crypto/rand.
// 122 effective bits of entropy (the v4 RFC reserves 6 bits for version
// + variant); well beyond brute-force reach. The uuid form matches the
// sessions.id column type (migrations/0006) so no on-the-wire reformatting
// is needed; cookies and logs hold the canonical hyphenated form.
func NewSessionID() (uuid.UUID, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return uuid.Nil, fmt.Errorf("iam: read session id: %w", err)
	}
	// Stamp v4 + RFC 4122 variant bits explicitly so we never depend on
	// uuid.NewRandom's internal entropy source.
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return uuid.UUID(b), nil
}

// ParseSessionTTL reads SESSION_TTL via the supplied getenv (typically
// os.Getenv) and returns a sanity-bounded duration. An empty value yields
// the 24h default; an unparseable value or one outside [1m, 30d] returns
// an error so the caller can log.Fatal at startup. Bounds reject the
// "SESSION_TTL=24 missing the unit" foot-gun (24ns) and the "SESSION_TTL=
// forever" misconfig.
func ParseSessionTTL(getenv func(string) string) (time.Duration, error) {
	raw := ""
	if getenv != nil {
		raw = getenv(EnvSessionTTL)
	}
	if raw == "" {
		return defaultSessionTTL, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("iam: parse %s=%q: %w", EnvSessionTTL, raw, err)
	}
	if d < minSessionTTL || d > maxSessionTTL {
		return 0, fmt.Errorf("iam: %s=%s out of range [%s, %s]", EnvSessionTTL, d, minSessionTTL, maxSessionTTL)
	}
	return d, nil
}

// MustParseSessionTTL is the convenience wrapper used by cmd/server (or its
// successor cmd/api after the SIN-62208 follow-up rename). Calls
// ParseSessionTTL and exits the process via the supplied fatal callback if
// the env value is invalid. Tests inject a custom fatal so they don't kill
// the test binary.
func MustParseSessionTTL(getenv func(string) string, fatal func(format string, args ...any)) time.Duration {
	if fatal == nil {
		fatal = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
			os.Exit(1)
		}
	}
	d, err := ParseSessionTTL(getenv)
	if err != nil {
		fatal("%v", err)
	}
	return d
}
