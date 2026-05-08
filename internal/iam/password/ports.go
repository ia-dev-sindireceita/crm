// Package password is the pure-domain helper for hashing, verifying, and
// policy-checking user passwords. It is the implementation of ADR 0070
// (docs/adr/0070-password-hashing.md) — the contract for parameters,
// stored format, and re-hash semantics is fixed there, NOT in this code.
//
// Hexagonal contract: this package does not import database/sql, net/http,
// or any vendor SDK. The HIBP / breach-corpus check is a port
// (PwnedPasswordChecker) wired by the caller to an adapter. The clock is
// implicit (no time-based logic lives here).
package password

import (
	"context"
	"errors"
)

// Hasher derives a stored password representation from a plaintext.
// Implementations MUST embed the parameters into the returned string so
// Verify can decode and re-derive without consulting a global current-
// params constant — see ADR 0070 §2 / §3.
type Hasher interface {
	Hash(plain string) (string, error)
}

// Verifier checks plaintext against a stored representation produced by a
// (possibly older) Hasher.
//
//   - ok:          plaintext matches stored.
//   - needsRehash: the stored parameters differ from the current Hasher
//     parameters (caller should re-hash on the next
//     successful login per ADR 0070 §3).
//   - err:         decoding or computation failure. A simple mismatch is
//     ok=false, err=nil — never an error.
type Verifier interface {
	Verify(stored, plain string) (ok bool, needsRehash bool, err error)
}

// PolicyChecker validates a plaintext against the password policy
// (ADR 0070 §5: length bounds, identity check, breach-corpus screening).
// It returns nil on pass, or a *PolicyError naming the first failed rule
// so callers can render a localized message.
type PolicyChecker interface {
	PolicyCheck(ctx context.Context, plain string, pctx PolicyContext) error
}

// PolicyContext carries per-request values needed for the policy check.
// Email, Username and TenantName drive the identity-equality rule. Pepper
// is the optional server-side secret reserved by ADR 0070 §4 — not used
// by the current Hasher/Verifier but accepted in the contract so adoption
// later is a no-signature-change.
type PolicyContext struct {
	Email      string
	Username   string
	TenantName string
	Pepper     string
}

// PwnedPasswordChecker is the port the policy uses to consult the breach
// corpus. The production adapter is internal/adapter/hibp; tests inject
// fakes. Returning ErrPwnedCheckUnavailable means the upstream service is
// degraded — the policy MUST then fall through to the bundled local list
// per ADR 0070 §5 ("fail-open with guardrails: HIBP outage doesn't let
// known-top-100k passwords through").
type PwnedPasswordChecker interface {
	IsPwned(ctx context.Context, plain string) (bool, error)
}

// ErrPwnedCheckUnavailable is the sentinel a PwnedPasswordChecker returns
// when the upstream HIBP API is unavailable AND the local fallback was
// not consulted by the adapter. The policy treats this as "degraded" and
// runs its own local-list lookup — passing only if the password is not
// in the bundled top-100k.
var ErrPwnedCheckUnavailable = errors.New("password: pwned-check unavailable")
