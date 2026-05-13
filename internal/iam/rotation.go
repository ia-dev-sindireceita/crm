package iam

// RotationTrigger names an event that, per ADR 0073 D3, MUST cause the
// session id (and CSRF token, per D1) to rotate. Mint a new session id,
// invalidate the old one, re-mint the CSRF token; the cookie shipped on
// the response carries the new id.
//
// The enum is shipped as a small int rather than the string forms used
// in metrics labels so a typo at the call site is a compile error, not
// a runtime "no rotation happened".
type RotationTrigger int

// Rotation triggers. Order MUST be stable — these values are not
// persisted, but adding a value at the end keeps switch defaults safe.
const (
	// RotateUnknown is the zero value; never treated as a rotation event.
	RotateUnknown RotationTrigger = iota
	// RotateLogin: user just authenticated. New session, new CSRF.
	RotateLogin
	// RotateLogout: explicit user logout. Old id invalidated; the user
	// has no current session to preserve.
	RotateLogout
	// RotateRoleChange: elevation, demotion, or any RBAC change that
	// affects the session's authorised actions.
	RotateRoleChange
	// RotateTwoFactorSuccess: 2FA verify succeeded. Swaps the pre-MFA
	// session id for a post-MFA id so a passive observer who saw the
	// pre-MFA cookie cannot ride it past MFA.
	RotateTwoFactorSuccess
)

// String returns a stable label suitable for metrics or audit log.
// RotateUnknown returns "unknown" — surface it as a programmer-bug
// signal in dashboards.
func (t RotationTrigger) String() string {
	switch t {
	case RotateLogin:
		return "login"
	case RotateLogout:
		return "logout"
	case RotateRoleChange:
		return "role_change"
	case RotateTwoFactorSuccess:
		return "twofactor_success"
	}
	return "unknown"
}

// ShouldRotate reports whether t is a known rotation event. Useful guard
// at the call boundary when the trigger is deserialised from external
// input (e.g., a webhook claim) — the unknown case is a no-op rather
// than an accidental rotation.
func ShouldRotate(t RotationTrigger) bool {
	switch t {
	case RotateLogin, RotateLogout, RotateRoleChange, RotateTwoFactorSuccess:
		return true
	}
	return false
}
