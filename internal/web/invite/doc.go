// Package invite serves the PUBLIC, unauthenticated set-password page that
// consumes the credential token minted by the tenant user-management flow
// (SIN-66510, child of SIN-66493; base SIN-66496 minted the token but never
// consumed it, so invited accounts were un-loginable).
//
// The token IS the credential: there is no session on these routes. The
// handler validates the token by HASH (never by plaintext compare), renders
// a set-password form on a valid token, and on POST hands off to the
// tenantusers.CredentialService which runs the reused ADR 0070 §5 password
// policy, hashes with the reused argon2id hasher, and consumes the token
// atomically (single-use, no replay). Invalid / expired / consumed tokens
// all render ONE generic error so the response is not an enumeration oracle.
//
// Security envelope (mounted in the tenanted group, OUTSIDE the authed
// sub-group — same shape as the public campaign / privacy routes):
//   - Host → tenant is resolved by middleware.TenantScope before the handler
//     runs, so the credential lookup runs inside the tenant's RLS envelope.
//   - A per-IP + per-token-prefix rate limit (G6) wraps the mux at the wire
//     layer (cmd/server/invite_wire.go) to blunt token brute-force.
//   - html/template auto-escapes all output; the only inline <style>/<script>
//     carry the request CSP nonce.
//   - The plaintext token is never logged.
package invite
