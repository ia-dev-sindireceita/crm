package mastermfa

import (
	"errors"
	"net/http"
)

// MasterSessionMFA is the slice of master-session storage that the
// RequireMasterMFA middleware and the verify handler need. The
// production adapter (lands with the master-auth PR; not in this
// repo yet) will bind these against a per-session record carrying
// `mfa_verified_at`. Tests in this package inject a fake implementing
// the same two methods.
//
// IsVerified reports whether the *current* master session has been
// MFA-verified within the same login. The middleware calls this on
// every gated request.
//
// MarkVerified flips the bit. The verify handler calls this exactly
// once on a successful TOTP or recovery code submission. It is
// allowed to write a Set-Cookie header (typical for an opaque
// session-side bit) — that is why the request response writer is
// passed in.
type MasterSessionMFA interface {
	IsVerified(r *http.Request) (bool, error)
	MarkVerified(w http.ResponseWriter, r *http.Request) error
}

// MasterSessionRotator is the cookie+storage-aware rotation surface
// the verify handler uses on a successful TOTP / recovery-code
// submission to swap the pre-MFA session id for a fresh post-MFA id
// (ADR 0073 §D3, SIN-62377 / FAIL-4). The production adapter
// (HTTPSession) implements both MasterSessionMFA and this; tests that
// don't exercise rotation behaviour leave the verify handler's
// Rotator field nil and fall through to the legacy MarkVerified path.
//
// RotateAndMarkVerified MUST:
//
//  1. Read the current __Host-sess-master cookie to get the old id.
//  2. Atomically swap the master_session row for a fresh CSPRNG id
//     (preserving created_at, expires_at, mfa_verified_at, ip,
//     user_agent).
//  3. Stamp mfa_verified_at = now() on the new row.
//  4. Re-issue __Host-sess-master with the new id (same flags).
//
// A storage failure or missing cookie returns ErrSessionMFAState
// (wrapped) so the verify handler surfaces a 500 — silently leaving
// the bit unflipped or the cookie unrotated would defeat the fix.
type MasterSessionRotator interface {
	RotateAndMarkVerified(w http.ResponseWriter, r *http.Request) error
}

// ErrSessionMFAState is the wrapper sentinel callers can use with
// errors.Is on storage-layer failures from MasterSessionMFA. It is
// not returned by the adapter directly — the adapter wraps its
// underlying error with this so the middleware can distinguish
// "transient storage failure" from "session positively reports
// not-verified".
var ErrSessionMFAState = errors.New("mastermfa: session mfa state read failed")
