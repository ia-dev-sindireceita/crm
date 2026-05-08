package mfa

import "errors"

// Sentinel errors for the MFA primitives. Domain callers compare with
// errors.Is; HTTP adapters map them to status codes / template flags.
var (
	// ErrInvalidCode is returned when a TOTP or recovery code does not
	// match the stored secret / hash. Callers MUST treat this as the
	// generic "wrong code" outcome and never leak which factor failed.
	ErrInvalidCode = errors.New("mfa: invalid code")

	// ErrCodeConsumed is returned by RecoveryConsumer when the submitted
	// code has already been used (single-use, ADR 0074 §2). The HTTP
	// layer maps this to the same generic "wrong code" message as
	// ErrInvalidCode to avoid revealing whether a code ever existed.
	ErrCodeConsumed = errors.New("mfa: recovery code already consumed")

	// ErrSeedTooShort is returned by NewSecret/EncodeSecret when the
	// supplied seed is shorter than the RFC 6238 minimum (160 bits = 20
	// bytes). ADR 0074 §1 pins the production size at 32 bytes; this
	// error exists to fail closed if a misconfigured caller passes a
	// truncated buffer.
	ErrSeedTooShort = errors.New("mfa: seed shorter than RFC 6238 minimum (160 bits)")

	// ErrCodeFormat is returned by the recovery normaliser when the
	// submitted plaintext can not be reduced to a 10-character base32
	// codeword (wrong length after dash-strip, illegal alphabet, etc.).
	ErrCodeFormat = errors.New("mfa: recovery code format invalid")

	// ErrNotEnrolled is returned by SeedRepository.LoadSeed when no
	// master_mfa row exists for the requested userID. Adapters MUST
	// translate their storage-layer "no rows" error into this domain
	// sentinel so the Service layer compares without importing the
	// adapter's package (Hexagonal rule from ADR 0074).
	ErrNotEnrolled = errors.New("mfa: master not enrolled")
)
