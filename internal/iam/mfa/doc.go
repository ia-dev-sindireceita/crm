// Package mfa is the pure-domain core for master multi-factor
// authentication. It implements the primitives and ports defined by
// ADR 0074 (docs/adr/0074-master-mfa-phase0.md): RFC 6238 TOTP, single-
// use recovery codes hashed at rest with the Argon2id helper from
// ADR 0070, and the contracts the HTTP / storage adapters wire up.
//
// Hexagonal contract: this package does NOT import database/sql,
// net/http, or any vendor SDK. Persistence, transport, encryption,
// audit, and Slack alerting are all ports (see ports.go) wired by the
// caller (cmd/server) to concrete adapters in their own packages.
//
// Boring-tech budget: TOTP is implemented against the standard library
// alone (crypto/hmac, crypto/sha1, encoding/base32) so we don't take a
// new dependency for a 50-line algorithm.
package mfa
