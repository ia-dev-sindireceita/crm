package iam

import (
	"fmt"
	"os"
)

// TOTPVerifier is the port for second-factor verification. Phase 6 ships
// the real TOTP implementation; Phase 0 ships only the NoopTOTP stub so
// the rest of the IAM surface can be wired and tested.
type TOTPVerifier interface {
	Verify(secret, code string) bool
}

// NoopTOTP accepts any non-empty code as valid. It exists to unblock
// development and tests; it MUST NOT run in production. Use
// AssertProductionSafe at bootstrap to fail closed if a deploy is
// misconfigured.
type NoopTOTP struct{}

// Verify returns true iff code is non-empty. The empty-string check exists
// only so a missing field on a form doesn't accidentally pass 2FA in dev.
func (NoopTOTP) Verify(_, code string) bool {
	return code != ""
}

// EnvIsProduction reports whether the supplied getenv (typically
// os.Getenv) names a production environment. The check is intentionally
// strict: only the literal string "production" counts. Values like
// "prod" or "PROD" are NOT treated as production so a misnamed env var
// fails closed (i.e. the assert below trips).
func EnvIsProduction(getenv func(string) string) bool {
	if getenv == nil {
		return false
	}
	return getenv("ENV") == "production"
}

// AssertProductionSafe panics with a clear bootstrap-time message if the
// supplied verifier is the NoopTOTP stub AND the environment is
// production. Call this from cmd/api (or whichever binary owns the wire-
// up) BEFORE the HTTP server starts so a misconfigured deploy aborts
// before serving the first request rather than the millionth.
//
// The function takes os.Exit so tests can pass a no-op exit and inspect
// stderr instead of killing the test binary. Production callers pass
// os.Exit directly.
func AssertProductionSafe(verifier TOTPVerifier, getenv func(string) string, exit func(int)) {
	if !EnvIsProduction(getenv) {
		return
	}
	if _, ok := verifier.(NoopTOTP); !ok {
		return
	}
	if exit == nil {
		exit = os.Exit
	}
	fmt.Fprintln(os.Stderr, "iam: NoopTOTP is not allowed when ENV=production; refuse to start")
	exit(1)
}
