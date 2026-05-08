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

// ErrSessionMFAState is the wrapper sentinel callers can use with
// errors.Is on storage-layer failures from MasterSessionMFA. It is
// not returned by the adapter directly — the adapter wraps its
// underlying error with this so the middleware can distinguish
// "transient storage failure" from "session positively reports
// not-verified".
var ErrSessionMFAState = errors.New("mastermfa: session mfa state read failed")
