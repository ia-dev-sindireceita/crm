package mastermfa

import (
	"errors"
	"fmt"
	"net/http"
	"time"
)

// RecentReader is the request-scoped adapter that the
// RequireRecentMFA middleware consumes. It reads the master session
// id from the cookie and delegates to a SessionStore for the
// mfa_verified_at timestamp, translating storage sentinels to the
// shape the middleware expects:
//
//   - cookie absent / unparseable / row missing / row expired →
//     ErrSessionNotFound (the middleware redirects to /m/2fa/verify).
//   - row exists with NULL mfa_verified_at → (zero time, nil).
//   - row exists with a verified_at timestamp → (timestamp, nil).
//   - any other storage error → wrapped %w error (the middleware 500s).
//
// HTTPSession already exposes the id-scoped MasterSessionVerifiedAtStore
// port, but Go forbids two methods named VerifiedAt with different
// signatures on the same struct. Splitting the request-scoped surface
// into its own adapter keeps both ports satisfiable from the same
// SessionStore without naming collisions.
type RecentReader struct {
	store SessionStore
}

// NewRecentReader returns the adapter. nil store panics at wire time
// per the project convention (consistent with HTTPSession,
// EnrollHandler, VerifyHandler shape).
func NewRecentReader(store SessionStore) *RecentReader {
	if store == nil {
		panic("mastermfa: NewRecentReader: store is nil")
	}
	return &RecentReader{store: store}
}

// VerifiedAt satisfies MasterSessionRecentMFA. See package doc.
func (a *RecentReader) VerifiedAt(r *http.Request) (time.Time, error) {
	sessionID, ok, err := readMasterSessionID(r)
	if err != nil {
		// Cookie present but unparseable — treat as no session, which
		// matches RequireMasterAuth's redirect-to-login posture: the
		// upstream master-auth middleware would already have bounced
		// the request, so reaching this branch means a wiring bug or a
		// race. Surfacing ErrSessionNotFound keeps the middleware on
		// the safe redirect path.
		return time.Time{}, ErrSessionNotFound
	}
	if !ok {
		return time.Time{}, ErrSessionNotFound
	}
	sess, err := a.store.Get(r.Context(), sessionID)
	if err != nil {
		if errors.Is(err, ErrSessionNotFound) || errors.Is(err, ErrSessionExpired) {
			return time.Time{}, ErrSessionNotFound
		}
		return time.Time{}, fmt.Errorf("mastermfa: recent reader: get session: %w", err)
	}
	if sess.MFAVerifiedAt == nil {
		return time.Time{}, nil
	}
	return *sess.MFAVerifiedAt, nil
}
