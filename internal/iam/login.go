package iam

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/ratelimit"
)

// TenantResolver resolves a request host (e.g. "acme.crm.local") to the
// tenant uuid that owns it. PR5 implements the postgres-backed adapter;
// SIN-62213 only depends on the interface. ErrTenantNotFound is the
// canonical "host is unknown" signal — Login translates it to
// ErrInvalidCredentials so a hostile client cannot enumerate which hosts
// are configured.
type TenantResolver interface {
	ResolveByHost(ctx context.Context, host string) (uuid.UUID, error)
}

// UserCredentialReader is the slice of the user-store the login flow needs
// — just enough to fetch (id, password_hash) by email, scoped to a
// resolved tenant. The postgres adapter is responsible for running the
// SELECT inside WithTenant; the implementation contract is documented on
// LookupCredentials.
//
// The interface is deliberately narrow ("accept broad / return narrow"):
// the rest of the user lifecycle (create, list, update password) is not
// SIN-62213's problem.
type UserCredentialReader interface {
	// LookupCredentials returns the user id and the encoded password hash
	// for the email within the given tenant. If no row matches, the
	// implementation MUST return (uuid.Nil, "", nil) — a nil error with
	// zero values, NOT an error. Login distinguishes "not found" from
	// "DB error" purely by the zero-id sentinel. This makes the timing-
	// equivalent dummy-verify branch unambiguous.
	LookupCredentials(ctx context.Context, tenantID uuid.UUID, email string) (uuid.UUID, string, error)
}

// Service is the IAM use-case façade. It composes a tenant resolver, the
// user-credential read port, and a session store. cmd/api wires the
// concrete adapters at bootstrap; tests wire fakes.
//
// WithTenant composition (SIN-62213 design V1, [SIN-62212]'s WithTenant is
// non-composable — calling it nested would open two separate transactions
// on two pool connections):
//
//   - LookupCredentials runs inside its OWN WithTenant (the postgres
//     adapter handles that).
//   - argon2id verify runs OUTSIDE any DB transaction (it is CPU-bound;
//     holding a tx during ~100ms of hashing is a connection-pool
//     anti-pattern).
//   - SessionStore.Create opens its OWN WithTenant.
//
// The two transactions are sequential, never nested. If user lookup
// succeeds and session insert fails, the user lookup was read-only —
// no integrity concern.
type Service struct {
	Tenants  TenantResolver
	Users    UserCredentialReader
	Sessions SessionStore
	TTL      time.Duration

	// Now is the clock source. nil falls back to time.Now. Tests inject a
	// frozen clock to assert expiry boundaries deterministically.
	Now func() time.Time

	// Logger is the structured log sink. nil falls back to slog.Default.
	// The login flow logs only tenant_id, user_id (success only), session
	// id prefix (success only, see below), and a string reason on failure.
	// It NEVER logs email, password, or password_hash — see
	// docs/security/passwords.md for the full no-log policy.
	Logger *slog.Logger

	// Lockouts is the durable account-lockout port (SIN-62341, ADR 0073
	// §D4). Pre-checked BEFORE VerifyPassword: if the principal has an
	// active account_lockout row, Login returns ErrAccountLocked
	// immediately without touching the password hash. Nil disables the
	// lockout flow entirely — existing tests that construct a bare
	// Service literal continue to work.
	Lockouts ratelimit.Lockouts

	// Limiter is the failure counter for the SIN-62341 lockout policy.
	// Each VerifyPassword(false) records a hit in the
	// "failed_login:email:<sha256(email)>" sliding-window bucket; when
	// LoginPolicy.Lockout.Threshold is exceeded the lockout row is
	// written and Login returns ErrAccountLocked. Nil disables the
	// counter (no lockout writes); the IsLocked pre-check still runs if
	// Lockouts is wired.
	Limiter ratelimit.RateLimiter

	// LoginPolicy carries the threshold + duration + alert flag the
	// failure-counter / lockout flow consults. Zero (the natural value)
	// disables the lockout writes; existing tests need not set it.
	// Master logins use a separate Service with a Policy whose
	// AlertOnLock is true so the synchronous Slack notification fires
	// (acceptance criterion #3).
	LoginPolicy ratelimit.Policy

	// Alerter is the synchronous notification port. Wired only on the
	// master Service; tenant Service leaves it nil. When the lockout
	// trips and LoginPolicy.Lockout.AlertOnLock is true, Login calls
	// Notify before returning. A non-nil Alerter error is logged but
	// does NOT abort the lockout — the persisted account_lockout row
	// is the authoritative penalty.
	Alerter ratelimit.Alerter
}

// dummyHash is a precomputed argon2id hash used to make the latency of
// "user not found" indistinguishable from "user found, wrong password".
// Without it, an attacker who can time the response can enumerate which
// emails exist in a tenant: a fast 401 means "user does not exist" and a
// slow 401 means "user exists, password wrong". Verifying the supplied
// password against this hash on the not-found path closes that channel.
//
// Computed in init() so the cost is paid once at startup, never on the
// request path. The plaintext is intentionally never matched in
// production.
var dummyHash string

func init() {
	h, err := HashPassword("invariant-dummy-do-not-match")
	if err != nil {
		panic(fmt.Errorf("iam: precompute dummy hash: %w", err))
	}
	dummyHash = h
}

// Login authenticates a user against (host, email, password) and, on
// success, persists a fresh session and returns it.
//
// All credential-mismatch failures collapse to ErrInvalidCredentials:
//
//   - Host does not resolve to a tenant.
//   - Tenant has no user with that email.
//   - Stored hash does not match the supplied password.
//
// Internal errors (DB outages, salt-gen failure, etc.) propagate so the
// caller can return a 5xx instead of a 4xx.
//
// ipAddr / userAgent are stamped onto the Session for audit trail; they
// are optional (pass nil / "" if unknown).
func (s *Service) Login(ctx context.Context, host, email, password string, ipAddr net.IP, userAgent string) (Session, error) {
	logger := s.logger()

	tenantID, err := s.Tenants.ResolveByHost(ctx, host)
	if err != nil {
		// Errors here include ErrTenantNotFound and infra failures.
		// We collapse only the not-found-shaped errors to
		// ErrInvalidCredentials; infra errors propagate so the HTTP
		// layer can return 5xx rather than mislead the user with 401.
		if isLookupNotFound(err) {
			// Anti-enumeration: same dummy-verify as the unknown-email
			// path below. Without this, "unknown host" returns in ~µs
			// while "known host, unknown email" takes ~100 ms (one
			// argon2id derivation). An on-the-wire attacker can use the
			// 3-orders-of-magnitude gap to enumerate which hosts are
			// configured tenants — i.e. the customer list of the SaaS.
			// Verifying the supplied password against dummyHash here
			// equalises wall-clock cost across all credential-mismatch
			// branches. See docs/security/passwords.md.
			_, _ = VerifyPassword(password, dummyHash)
			logger.WarnContext(ctx, "login: host did not resolve to a tenant", slog.String("reason", "invalid_credentials"))
			return Session{}, ErrInvalidCredentials
		}
		return Session{}, fmt.Errorf("iam: resolve host: %w", err)
	}

	userID, encoded, err := s.Users.LookupCredentials(ctx, tenantID, email)
	if err != nil {
		return Session{}, fmt.Errorf("iam: lookup credentials: %w", err)
	}

	// Anti-enumeration: when the user is not found, run a verification
	// against dummyHash so the wall-clock cost is the same as the
	// verified path. The result is discarded.
	if userID == uuid.Nil {
		_, _ = VerifyPassword(password, dummyHash)
		logger.WarnContext(ctx, "login: rejected", slog.String("reason", "invalid_credentials"), slog.String("tenant_id", tenantID.String()))
		return Session{}, ErrInvalidCredentials
	}

	// Pre-check the durable lockout BEFORE running VerifyPassword. The
	// SIN-62341 contract is "lockout vence o counter" — once the
	// account_lockout row exists, no password verification happens, so
	// a Redis flush cannot reset the penalty (AC #2). Order matters:
	// the IsLocked branch runs only after the user-found check above,
	// so unknown emails never reach this path and cannot enumerate
	// which accounts have locked siblings.
	if s.Lockouts != nil {
		locked, until, err := s.Lockouts.IsLocked(ctx, userID)
		if err != nil {
			return Session{}, fmt.Errorf("iam: lockout pre-check: %w", err)
		}
		if locked {
			// Run a dummy VerifyPassword anyway so the wall-clock cost
			// of the locked branch is indistinguishable from the
			// verify-then-fail branch. Without it an attacker timing
			// the response could distinguish "locked" from "wrong
			// password" and learn that an account is in lockout state
			// (AC #4 timing window applies here too).
			_, _ = VerifyPassword(password, dummyHash)
			logger.WarnContext(ctx, "login: rejected",
				slog.String("reason", "account_locked"),
				slog.String("tenant_id", tenantID.String()),
				slog.Time("locked_until", until),
			)
			return Session{}, &AccountLockedError{Until: until}
		}
	}

	ok, err := VerifyPassword(password, encoded)
	if err != nil || !ok {
		// Failed verify: record the hit in the sliding-window failure
		// counter. If the threshold is exceeded, write the durable
		// lockout row and (for master endpoints) fire the synchronous
		// alert. The user still sees ErrAccountLocked on the
		// trip-attempt because the persisted row is the truth source.
		if until, locked := s.recordLoginFailure(ctx, tenantID, userID, email); locked {
			return Session{}, &AccountLockedError{Until: until}
		}
		logger.WarnContext(ctx, "login: rejected", slog.String("reason", "invalid_credentials"), slog.String("tenant_id", tenantID.String()))
		return Session{}, ErrInvalidCredentials
	}

	// Successful verify: best-effort lockout reset. Clear is idempotent
	// (no-op when no row exists) so the call is unconditional. A Clear
	// failure does NOT abort the login: the user has authenticated
	// successfully and the lockout row, if any, is stale by definition.
	if s.Lockouts != nil {
		if err := s.Lockouts.Clear(ctx, userID); err != nil {
			logger.WarnContext(ctx, "login: clear lockout failed",
				slog.String("tenant_id", tenantID.String()),
				slog.String("user_id", userID.String()),
				slog.String("err", err.Error()),
			)
		}
	}

	id, err := NewSessionID()
	if err != nil {
		return Session{}, fmt.Errorf("iam: new session id: %w", err)
	}
	now := s.now()
	sess := Session{
		ID:        id,
		UserID:    userID,
		TenantID:  tenantID,
		ExpiresAt: now.Add(s.ttl()),
		CreatedAt: now,
		IPAddr:    ipAddr,
		UserAgent: userAgent,
	}
	if err := s.Sessions.Create(ctx, sess); err != nil {
		return Session{}, fmt.Errorf("iam: create session: %w", err)
	}

	// session_id_prefix is the first 8 hex chars (32 bits) of the 128-bit
	// session UUID. It is intentionally correlatable across log lines so
	// ops/incident response can follow a session through the access log
	// without dragging the full id (which is what the cookie holds).
	// Future reviewer: do NOT remove this field thinking it is leaked
	// PII. 32 bits is far below brute-force feasibility (90+ bits remain
	// secret), and prefix alone cannot be used to hijack the session.
	logger.InfoContext(ctx, "login: ok",
		slog.String("tenant_id", tenantID.String()),
		slog.String("user_id", userID.String()),
		slog.String("session_id_prefix", sess.ID.String()[:8]),
	)
	return sess, nil
}

// Logout deletes the session row, scoped to the resolved tenant. A delete
// of a non-existent session is NOT an error — the operation is idempotent
// so a stale cookie doesn't surface a 5xx.
func (s *Service) Logout(ctx context.Context, tenantID, sessionID uuid.UUID) error {
	if err := s.Sessions.Delete(ctx, tenantID, sessionID); err != nil {
		return fmt.Errorf("iam: delete session: %w", err)
	}
	return nil
}

// ValidateSession looks up the session and returns it iff:
//
//   - The session id is known to the tenant (else ErrSessionNotFound).
//   - ExpiresAt is in the future (else ErrSessionExpired).
//
// The TTL check happens BEFORE any further use of the session value, so a
// caller cannot accidentally trust an expired row's UserID.
func (s *Service) ValidateSession(ctx context.Context, tenantID, sessionID uuid.UUID) (Session, error) {
	sess, err := s.Sessions.Get(ctx, tenantID, sessionID)
	if err != nil {
		return Session{}, err
	}
	if sess.IsExpired(s.now()) {
		return Session{}, ErrSessionExpired
	}
	return sess, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Service) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return defaultSessionTTL
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// ErrTenantNotFound is the sentinel a TenantResolver implementation should
// return to signal "host is unknown" without ambiguity. Login collapses
// this (via isLookupNotFound) to ErrInvalidCredentials so the HTTP layer
// returns a uniform 401.
var ErrTenantNotFound = errors.New("iam: tenant not found")

func isLookupNotFound(err error) bool {
	return errors.Is(err, ErrTenantNotFound)
}

// failedLoginKey returns the Redis sliding-window bucket key for a
// failed-login event. The email is sha256-hashed so PII never lands
// in the limiter logs / metric labels — the only place the plain
// email can appear is the WARN log line, which the logger config
// controls. lower-trim normalises "Alice@x" and "alice@x" onto the
// same counter so trivial casing variants cannot bypass the lockout.
func failedLoginKey(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return "failed_login:email:" + hex.EncodeToString(sum[:])
}

// recordLoginFailure increments the sliding-window failure counter
// for email and, when the policy threshold is exceeded, writes the
// durable account_lockout row and fires the synchronous Slack alert
// for master endpoints. Returns the locked_until timestamp and true
// if the call resulted in a lockout being written for the current
// attempt — the caller uses this to build the typed
// *AccountLockedError so the HTTP layer can derive a Retry-After
// header. Returns (zero, false) on every other path, including
// counter-only throttles and write failures.
//
// Failure paths are intentionally swallowed (with a WARN log): a
// Limiter outage MUST NOT make every login look like a 401 to the
// user, and a Lockouts write-failure does not change the credential
// verdict for the current attempt.
func (s *Service) recordLoginFailure(ctx context.Context, tenantID, userID uuid.UUID, email string) (time.Time, bool) {
	logger := s.logger()
	if s.Limiter == nil || !s.LoginPolicy.LockoutEnabled() {
		return time.Time{}, false
	}
	allowed, _, err := s.Limiter.Allow(
		ctx,
		failedLoginKey(email),
		s.LoginPolicy.Lockout.Duration,
		s.LoginPolicy.Lockout.Threshold,
	)
	if err != nil {
		logger.WarnContext(ctx, "login: failure-counter error",
			slog.String("tenant_id", tenantID.String()),
			slog.String("err", err.Error()),
		)
		return time.Time{}, false
	}
	if allowed {
		return time.Time{}, false
	}
	// Threshold tripped. Write the durable lockout row.
	if s.Lockouts == nil {
		// Counter trip but no Lockouts wired — return zero so the
		// caller still emits the standard ErrInvalidCredentials. This
		// matches the "Lockouts nil disables the lockout flow" rule
		// documented on Service.Lockouts.
		return time.Time{}, false
	}
	until := s.now().Add(s.LoginPolicy.Lockout.Duration)
	reason := fmt.Sprintf("ratelimit: %d failed login attempts", s.LoginPolicy.Lockout.Threshold)
	if err := s.Lockouts.Lock(ctx, userID, until, reason); err != nil {
		logger.WarnContext(ctx, "login: write lockout failed",
			slog.String("tenant_id", tenantID.String()),
			slog.String("user_id", userID.String()),
			slog.String("err", err.Error()),
		)
		// Lock write failed; still treat the attempt as a normal
		// credential rejection so the response stays uniform. A
		// retry on the next attempt will re-trip the threshold and
		// try the write again.
		return time.Time{}, false
	}
	logger.WarnContext(ctx, "login: locked",
		slog.String("tenant_id", tenantID.String()),
		slog.String("user_id", userID.String()),
		slog.Time("locked_until", until),
		slog.String("reason", reason),
	)
	if s.Alerter != nil && s.LoginPolicy.Lockout.AlertOnLock {
		// Synchronous notify (acceptance criterion #3). The Slack
		// adapter caps the round-trip with its own per-call deadline,
		// so a slow webhook does not stall the login response.
		if err := s.Alerter.Notify(ctx, fmt.Sprintf(
			"account locked: policy=%s user=%s tenant=%s until=%s",
			s.LoginPolicy.Name, userID, tenantID, until.UTC().Format(time.RFC3339),
		)); err != nil {
			logger.WarnContext(ctx, "login: alerter notify failed",
				slog.String("tenant_id", tenantID.String()),
				slog.String("user_id", userID.String()),
				slog.String("err", err.Error()),
			)
		}
	}
	return until, true
}
