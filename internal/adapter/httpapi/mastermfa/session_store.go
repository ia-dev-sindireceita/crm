package mastermfa

// SIN-62385 master-session storage ports. PR1 of the SIN-62381
// decomposition (see plan document on the parent issue).
//
// The mastermfa package owns the master-side authn surface. PR1
// defines:
//
//   - Session                 — value object the adapter returns.
//   - SessionStore            — read/write port login + verify use.
//   - MasterSessionVerifiedAt — read-only port the RequireRecentMFA
//                               middleware (PR3) uses to gate
//                               sensitive master actions.
//   - ErrSessionNotFound, ErrSessionExpired — sentinels every
//     adapter MUST translate from its underlying error.
//
// PR2 will add the master-auth middleware that calls
// SessionStore.Get on every request to load Master into ctx, and the
// /m/login + /m/logout handlers that call Create / Delete.
// PR3's middleware reads through the narrow MasterSessionVerifiedAt
// port and never sees the rest of the SessionStore surface.

import (
	"context"
	"errors"
	"net"
	"time"

	"github.com/google/uuid"
)

// Session is the master-side equivalent of the tenant `sessions` row
// from migration 0006. ID is opaque to callers — the cookie carries
// it and the master-auth middleware (PR2) hands it back to Get to
// load the row. UserID is the master operator (users.is_master =
// true). MFAVerifiedAt is the source of truth for the re-MFA gate
// (RequireRecentMFA, PR3): NULL means "this session has only
// completed password auth" and the gate redirects to /m/2fa/verify;
// non-NULL means "the operator passed TOTP at this instant" and the
// gate compares now-verifiedAt against the configured maxAge.
type Session struct {
	ID            uuid.UUID
	UserID        uuid.UUID
	CreatedAt     time.Time
	ExpiresAt     time.Time
	MFAVerifiedAt *time.Time
	IP            net.IP
	UserAgent     string
}

// SessionStore is the read/write port over the master_session table
// (migration 0010). The Postgres adapter in
// internal/adapter/db/postgres/mastersession is the production
// binding; tests in this package and in PR2 inject a fake satisfying
// the same contract.
//
// Every call MUST be ctx-aware — adapters wire the context through
// to the underlying transaction so callers can cancel cleanly.
//
// Callers MUST check for ErrSessionNotFound and ErrSessionExpired via
// errors.Is and treat them as authentication-deny outcomes. Every
// other error is a transient storage failure and SHOULD page (the
// master console deny-by-default contract from ADR 0073 §D3 means a
// storage outage MUST NOT silently grant access).
type SessionStore interface {
	// Create inserts a fresh master session for userID with
	// expires_at = now + ttl. mfa_verified_at is left NULL — the
	// verify handler (PR2) calls MarkVerified after a successful
	// TOTP / recovery code submission. Returns the freshly-created
	// row so the caller can write the session id into the cookie.
	Create(ctx context.Context, userID uuid.UUID, ttl time.Duration) (Session, error)

	// Get loads the session row by id. Returns ErrSessionNotFound if
	// no row exists, ErrSessionExpired if the row exists but its
	// expires_at is in the past. Both outcomes deny access.
	Get(ctx context.Context, sessionID uuid.UUID) (Session, error)

	// Delete removes the session row. Used by /m/logout (PR2) to
	// invalidate the cookie. A missing row is NOT an error — the
	// caller has already cleared the cookie and the desired post-
	// condition (no live row for this id) is satisfied.
	Delete(ctx context.Context, sessionID uuid.UUID) error

	// MarkVerified stamps mfa_verified_at = now() on the session row
	// and returns the timestamp written so the caller can echo it in
	// observability. Returns ErrSessionNotFound if no row exists.
	// The verify handler (PR2) calls this exactly once per successful
	// TOTP or recovery code submission.
	MarkVerified(ctx context.Context, sessionID uuid.UUID) (time.Time, error)

	// Touch extends expires_at to now + idleTTL on the session row,
	// implementing the idle-bump pattern from ADR 0073 §D3. The
	// master-auth middleware (PR2) calls this on every request so
	// active operators do not get logged out mid-session. Returns
	// ErrSessionNotFound if no row exists.
	Touch(ctx context.Context, sessionID uuid.UUID, idleTTL time.Duration) error
}

// MasterSessionVerifiedAt is the read-only slice of master session
// storage the RequireRecentMFA middleware (PR3, ADR 0073 §D3 + ADR
// 0074 §4) needs. The middleware never writes — it only asks "what
// is the mfa_verified_at on this session id?" and decides whether
// the answer is fresh enough.
//
// Adapters MUST satisfy both SessionStore and MasterSessionVerifiedAt
// via the same backing store so a write through MarkVerified is
// immediately visible to a subsequent VerifiedAt read.
//
// VerifiedAt returns the stored mfa_verified_at value (which may be
// the zero time if the session has only completed password auth) or
// ErrSessionNotFound if no row exists. Any other error is a transient
// storage failure and the middleware MUST 500 — it is forbidden to
// silently let the request through on a storage error (deny-by-
// default for sensitive master actions).
type MasterSessionVerifiedAt interface {
	VerifiedAt(ctx context.Context, sessionID uuid.UUID) (time.Time, error)
}

// ErrSessionNotFound is the canonical sentinel adapters return when
// no master session row exists for the requested id. Both
// SessionStore.Get and MasterSessionVerifiedAt.VerifiedAt MUST
// translate the underlying "no row" outcome into this sentinel so
// callers can errors.Is without depending on pgx (Hexagonal rule
// from ADR 0074).
var ErrSessionNotFound = errors.New("mastermfa: master session not found")

// ErrSessionExpired is the sentinel SessionStore.Get returns when the
// row exists but its expires_at is in the past. The master-auth
// middleware (PR2) treats it the same as ErrSessionNotFound (clear
// cookie, redirect to /m/login) but distinguishes the two for
// observability — an "expired" outcome is benign churn, while a
// "not found" outcome on a cookie that the server signed is more
// suspicious.
var ErrSessionExpired = errors.New("mastermfa: master session expired")
