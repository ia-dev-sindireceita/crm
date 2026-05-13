// Package csrf is the pure-domain helper for CSRF token generation and
// verification per ADR 0073 (docs/adr/0073-csrf-and-session.md). The token
// is rotated PER SESSION (not per request) to avoid the HTMX hx-swap race
// described in §D1 of the ADR; this package owns Generate and Verify only.
//
// Hexagonal contract: this package does not import net/http, html/template,
// or any vendor SDK. The cookie wiring lives in
// internal/adapter/httpapi/sessioncookie; the middleware in
// internal/adapter/httpapi/csrf; the templ helpers there too. Persisting
// session.csrf_token is the storage adapter's job.
package csrf

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
)

// TokenBytes is the entropy budget for a CSRF token. ADR 0073 D1 fixes 32
// bytes of CSPRNG output. The base64 (RawURLEncoding) string form is 43
// characters, fits any cookie / header without further encoding.
const TokenBytes = 32

// ErrTokenMissing means neither the X-CSRF-Token header nor the form
// _csrf field carried a value. ADR 0073 D1 step 4.
var ErrTokenMissing = errors.New("csrf: token missing")

// ErrTokenMismatch means the presented token did not match the
// session.csrf_token under constant-time compare. ADR 0073 D1 step 6.
var ErrTokenMismatch = errors.New("csrf: token mismatch")

// ErrSessionTokenMissing means the caller passed an empty sessionToken.
// Defensive guard: an unset session token is a programmer bug — never
// silently accept "presented matches empty session", since that would let
// any request pass on a half-wired session.
var ErrSessionTokenMissing = errors.New("csrf: session has no token")

// GenerateToken returns a fresh 32-byte CSPRNG token base64-encoded with
// no padding. It is the value that is mirrored to session.csrf_token, the
// __Host-csrf cookie, and the <meta name="csrf-token"> tag.
func GenerateToken() (string, error) {
	var b [TokenBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("csrf: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// Verify reports whether the request-side tokens match sessionToken. The
// header takes precedence over the form value because HTMX writes the
// header and a stale form (re-rendered with an old token) should not
// override the live header.
//
// Returns ErrTokenMissing if both presented values are empty,
// ErrSessionTokenMissing if the session has no token (caller bug), or
// ErrTokenMismatch on a constant-time compare miss.
func Verify(sessionToken, headerToken, formToken string) error {
	presented := headerToken
	if presented == "" {
		presented = formToken
	}
	if presented == "" {
		return ErrTokenMissing
	}
	if sessionToken == "" {
		return ErrSessionTokenMissing
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(sessionToken)) != 1 {
		return ErrTokenMismatch
	}
	return nil
}
