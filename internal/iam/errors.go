// Package iam holds the Identity & Access Management domain (SIN-62213).
//
// This package owns the user-authentication flow: hashing/verifying passwords
// (argon2id), establishing and validating sessions, and the TOTP port stub.
// Storage, transport, and external systems live behind ports defined here;
// concrete adapters (e.g. internal/adapter/db/postgres) depend on iam, never
// the other way round.
//
// Sentinel errors are exported as package-level variables so callers can
// distinguish failure modes via errors.Is. Crucially, ErrInvalidCredentials
// is a SINGLE shared instance returned for every credential-mismatch path
// (unknown email, wrong password, unknown host) so the caller cannot tell
// which side of the lookup failed — anti-enumeration by construction.
package iam

import "errors"

// ErrInvalidCredentials is returned by Login whenever any part of the
// credential check fails: tenant not found for the host, user not found in
// the tenant, or password does not match. The same instance is returned in
// every case so callers (and an attacker on the wire) cannot distinguish
// "unknown email" from "wrong password" from "unknown host". Use errors.Is
// to test.
var ErrInvalidCredentials = errors.New("iam: invalid credentials")

// ErrSessionExpired is returned by ValidateSession when the session row
// exists but its expires_at is in the past. Callers should clear the
// session cookie and redirect to login.
var ErrSessionExpired = errors.New("iam: session expired")

// ErrSessionNotFound is returned by SessionStore.Get and ValidateSession
// when the session id is unknown to the current tenant. Cross-tenant
// probes also collapse to this error (instead of an RLS-specific signal)
// so a malicious tenant cannot enumerate session ids belonging to other
// tenants.
var ErrSessionNotFound = errors.New("iam: session not found")

// ErrInvalidEncoding is returned by VerifyPassword when the encoded hash
// does not match the $argon2id$… PHC format. Treat as a corrupted or
// foreign hash; never panic on adversarial input.
var ErrInvalidEncoding = errors.New("iam: invalid password encoding")

// ErrAccountLocked is returned by Login when the lockout pre-check
// (or the failure-counter trip on the current attempt) finds a
// durable account_lockout row whose locked_until is in the future.
// The HTTP layer maps this to 429 with a Retry-After header derived
// from the locked_until timestamp the Lockouts port returns. Use
// errors.Is to test.
//
// ErrAccountLocked is intentionally distinct from
// ErrInvalidCredentials: a locked-out attacker still gets the same
// uniform 401 timing on credential probes (the lockout pre-check
// runs AFTER the user-found check, so unknown emails never reach
// this branch). The asymmetry only surfaces to a real authenticated
// principal whose account has been locked, which is the audience
// that needs the differentiated message ("come back in 12 minutes",
// not "wrong password").
var ErrAccountLocked = errors.New("iam: account locked")
