package mastermfa

import (
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// MasterSessionRecentMFA is the request-scoped read-only slice of
// master session storage the RequireRecentMFA middleware needs.
// Implementations return the timestamp at which the *current* master
// session last completed an MFA challenge; a zero time signals "never
// verified in this session" (e.g. session created at /m/login but the
// user has not yet submitted /m/2fa/verify).
//
// The middleware passes this value to the freshness check; it is
// deliberately not collapsed into a bool ("verified within window")
// at the port boundary because tests need to assert on the exact
// timestamp surface, and sibling middlewares may want to consult the
// raw value for other policies (e.g. step-up to a stricter window for
// a single action, ADR 0073 §D3).
//
// The shape is request-scoped on purpose: the implementation reads
// the master session id off the request cookie and resolves the row
// itself, mirroring MasterSessionMFA. The id-scoped storage port lives
// in session_store.go (mastermfa.MasterSessionVerifiedAt); a request
// adapter wraps it so the middleware never has to plumb the cookie.
//
// ErrSessionNotFound (declared in session_store.go alongside the
// id-scoped port) is the canonical sentinel for "no master session
// is bound to this request" — the middleware treats it as a
// not-verified path so a stale cookie redirects to /m/2fa/verify
// rather than 500ing.
type MasterSessionRecentMFA interface {
	VerifiedAt(r *http.Request) (time.Time, error)
}

// RequireRecentMFAConfig is the constructor input.
//
// MaxAge is the freshness window. ADR 0073 §D3 lists the canonical
// window for the three sensitive master actions
// (master.grant_courtesy / master.impersonate.request /
// master.feature_flag.write) as 15 minutes. Callers MUST supply a
// non-zero value — passing zero panics at wire time so a misconfig
// cannot accidentally accept a session verified hours ago.
//
// VerifyPath defaults to "/m/2fa/verify" when empty (ADR 0074 §3),
// matching RequireMasterMFA's default.
//
// Now is the clock source; nil falls back to time.Now. Tests inject
// a frozen clock to assert the boundary deterministically.
type RequireRecentMFAConfig struct {
	Sessions   MasterSessionRecentMFA
	MaxAge     time.Duration
	VerifyPath string
	Logger     *slog.Logger
	Now        func() time.Time
}

// RequireRecentMFA returns a middleware that gates the wrapped route
// on a freshly-verified master MFA assertion within MaxAge.
//
// Order assumption: this middleware MUST run AFTER RequireMasterMFA in
// the chain. RequireMasterMFA already enforces "session exists +
// enrolled + at least one MFA verification in this login"; this
// middleware adds a tighter "verified within MaxAge" cap on top.
// Composing them in the other order would let an unverified session
// pass through the fresh-check (zero time + zero MaxAge edge case).
//
// Behaviour:
//
//  1. Master not in context → 401. Same shape as RequireMasterMFA's
//     deny-by-default, since this middleware's contract requires an
//     authenticated master upstream.
//  2. VerifiedAt returns ErrSessionNotFound or zero time → 303 to
//     VerifyPath with the original URL preserved in `?return=`.
//  3. VerifiedAt returns a real error → 500. Storage outages are
//     surfaced as 500 so monitoring catches them.
//  4. now - verifiedAt > MaxAge → 303 to VerifyPath (same shape as
//     case 2 — a stale verify and a missing verify look identical to
//     the user, which is correct).
//  5. Otherwise → next.ServeHTTP.
//
// `?return=` validation reuses the same isSafeReturnPath gate as
// RequireMasterMFA so an open-redirect via Referer/Host injection is
// blocked at both layers.
func RequireRecentMFA(cfg RequireRecentMFAConfig) func(http.Handler) http.Handler {
	if cfg.Sessions == nil {
		panic("mastermfa: RequireRecentMFA: Sessions is nil")
	}
	if cfg.MaxAge <= 0 {
		panic("mastermfa: RequireRecentMFA: MaxAge must be > 0")
	}
	verifyPath := cfg.VerifyPath
	if verifyPath == "" {
		verifyPath = "/m/2fa/verify"
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			master, ok := MasterFromContext(r.Context())
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			verifiedAt, err := cfg.Sessions.VerifiedAt(r)
			switch {
			case errors.Is(err, ErrSessionNotFound):
				redirectWithReturn(w, r, verifyPath)
				return
			case err != nil:
				logger.ErrorContext(r.Context(), "mastermfa: recent verify lookup failed",
					slog.String("user_id", master.ID.String()),
					slog.String("error", err.Error()),
				)
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			if verifiedAt.IsZero() {
				redirectWithReturn(w, r, verifyPath)
				return
			}

			if now().Sub(verifiedAt) > cfg.MaxAge {
				redirectWithReturn(w, r, verifyPath)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
