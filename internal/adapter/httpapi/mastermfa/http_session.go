package mastermfa

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/adapter/httpapi/sessioncookie"
)

// HTTPSession is the production binding of the three master-session
// HTTP ports: MasterSessionMFA (read + write of mfa_verified_at via
// (w, r)), MasterSessionVerifiedAtStore (read by sessionID — the
// id-scoped freshness port), and the cookie-IO surface the verify
// handler exercises indirectly via MarkVerified. The request-scoped
// freshness port (MasterSessionVerifiedAt, declared in
// require_recent.go and consumed by RequireRecentMFA) is satisfied by
// the sibling RecentReader adapter, not by HTTPSession, because Go
// does not allow two methods named VerifiedAt with different
// signatures on the same struct.
//
// Owning all three behind one struct keeps the cookie-name and cookie-
// value parsing logic in one place. The constructor takes a
// SessionStore so the same backing store is used for IsVerified /
// MarkVerified / VerifiedAt — a write through MarkVerified is
// immediately visible to a subsequent VerifiedAt read.
type HTTPSession struct {
	store      SessionStore
	cookieMaxA int // Max-Age for the master cookie on rotation, in seconds
}

// NewHTTPSession returns the adapter. Nil store panics at wire time
// per the project convention (consistent with EnrollHandler /
// VerifyHandler shape).
//
// The adapter writes a fresh master cookie on RotateAndMarkVerified
// (SIN-62377), so it needs the max-age the login handler used. cmd/server
// passes the same cookieMaxAge to NewHTTPSession that it passed to the
// login handler. A zero value falls back to the ADR 0073 §D3 master
// hard-cap default (DefaultMasterHardTTL = 4h).
func NewHTTPSession(store SessionStore) *HTTPSession {
	if store == nil {
		panic("mastermfa: NewHTTPSession: store is nil")
	}
	return &HTTPSession{store: store, cookieMaxA: int(DefaultMasterHardTTL.Seconds())}
}

// WithCookieMaxAge returns a copy of a with the master cookie max-age
// set to maxAge seconds. Used by cmd/server to align the rotation
// cookie with whatever HardTTL was passed to the login handler. A
// non-positive value resets to the DefaultMasterHardTTL fallback.
func (a *HTTPSession) WithCookieMaxAge(maxAge int) *HTTPSession {
	cp := *a
	if maxAge > 0 {
		cp.cookieMaxA = maxAge
	} else {
		cp.cookieMaxA = int(DefaultMasterHardTTL.Seconds())
	}
	return &cp
}

// IsVerified satisfies MasterSessionMFA. Reads __Host-sess-master,
// parses the value as a uuid, and reports whether the row's
// MFAVerifiedAt is non-nil.
//
// A missing / empty / unparseable cookie returns (false, nil): from
// the verify-handler's POV the session has not yet completed MFA, so
// the form should render; the upstream master-auth middleware is what
// gates "no session at all" with a redirect-to-login. Treating the
// missing-cookie path as a verifier error here would conflate two
// concerns.
//
// A storage error (other than ErrSessionNotFound / ErrSessionExpired)
// wraps ErrSessionMFAState so callers can errors.Is on the canonical
// sentinel and emit a 500 from the request path.
func (a *HTTPSession) IsVerified(r *http.Request) (bool, error) {
	sessionID, ok, err := readMasterSessionID(r)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	if !ok {
		return false, nil
	}
	sess, err := a.store.Get(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionExpired) {
			return false, nil
		}
		return false, fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	return sess.MFAVerifiedAt != nil, nil
}

// MarkVerified satisfies MasterSessionMFA. Reads __Host-sess-master,
// parses the value, and stamps mfa_verified_at = now() on the session
// row.
//
// A missing / empty / unparseable cookie returns ErrSessionMFAState —
// MarkVerified is only reachable via POST /m/2fa/verify, which the
// upstream master-auth middleware has already gated, so a missing
// cookie at this point is a wiring bug, not normal traffic. Surfacing
// the failure as an error makes the bug loud (500 in the verify
// handler) instead of silently leaving the bit unflipped.
//
// The (w, r) signature is preserved from the MasterSessionMFA contract
// even though this adapter does not write any Set-Cookie header — a
// future server-side flag rotation might. Keeping the surface stable
// across adapter implementations avoids churning the verify handler.
func (a *HTTPSession) MarkVerified(_ http.ResponseWriter, r *http.Request) error {
	sessionID, ok, err := readMasterSessionID(r)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	if !ok {
		return fmt.Errorf("%w: missing master session cookie", ErrSessionMFAState)
	}
	if _, err := a.store.MarkVerified(r.Context(), sessionID); err != nil {
		return fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	return nil
}

// RotateAndMarkVerified satisfies MasterSessionRotator. On a
// successful TOTP / recovery-code submission it:
//
//  1. Reads the current __Host-sess-master cookie and parses the
//     pre-MFA session id.
//  2. Calls SessionStore.RotateID to atomically swap the master_session
//     row for a fresh CSPRNG id (CreatedAt / ExpiresAt /
//     MFAVerifiedAt / IP / UserAgent are inherited).
//  3. Stamps mfa_verified_at = now() on the new row.
//  4. Re-issues __Host-sess-master with the new id and the same
//     cookie flags as login (HttpOnly, Secure, SameSite=Strict, host-
//     locked __Host- prefix). Max-Age matches the value cmd/server
//     passed at construction.
//
// All failure modes wrap ErrSessionMFAState so the verify handler
// surfaces a 500 — silently leaving the cookie unrotated would
// re-introduce the FAIL-4 vulnerability.
//
// SIN-62377 (FAIL-4) / ADR 0073 §D3.
func (a *HTTPSession) RotateAndMarkVerified(w http.ResponseWriter, r *http.Request) error {
	sessionID, ok, err := readMasterSessionID(r)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	if !ok {
		return fmt.Errorf("%w: missing master session cookie", ErrSessionMFAState)
	}
	rotated, err := a.store.RotateID(r.Context(), sessionID)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	if _, err := a.store.MarkVerified(r.Context(), rotated.ID); err != nil {
		return fmt.Errorf("%w: %v", ErrSessionMFAState, err)
	}
	maxAge := a.cookieMaxA
	if maxAge <= 0 {
		maxAge = int(DefaultMasterHardTTL.Seconds())
	}
	sessioncookie.SetMaster(w, rotated.ID.String(), maxAge)
	return nil
}

// VerifiedAt satisfies MasterSessionVerifiedAtStore. Looks up the session
// row by id and returns its MFAVerifiedAt timestamp, or the zero time
// when the session has only completed password auth. ErrSessionNotFound
// is propagated unchanged so the PR3 RequireRecentMFA middleware can
// distinguish "no row" from "row exists but not verified yet" via
// errors.Is.
//
// ErrSessionExpired is also propagated as ErrSessionNotFound: from the
// re-MFA gate's POV an expired session is indistinguishable from a
// missing one — both deny access — and the upstream master-auth
// middleware would have already bounced the request, so this branch is
// defensive.
func (a *HTTPSession) VerifiedAt(ctx context.Context, sessionID uuid.UUID) (time.Time, error) {
	sess, err := a.store.Get(ctx, sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionExpired) {
			return time.Time{}, ErrSessionNotFound
		}
		return time.Time{}, err
	}
	if sess.MFAVerifiedAt == nil {
		return time.Time{}, nil
	}
	return *sess.MFAVerifiedAt, nil
}

// readMasterSessionID extracts the master session id from the request
// cookie. Returns (id, true, nil) on a parsed value, (uuid.Nil, false,
// nil) when the cookie is absent, and (uuid.Nil, false, err) on a
// present-but-unparseable value.
func readMasterSessionID(r *http.Request) (uuid.UUID, bool, error) {
	raw, err := sessioncookie.Read(r, sessioncookie.NameMaster)
	if err != nil {
		if errors.Is(err, sessioncookie.ErrCookieMissing) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}
