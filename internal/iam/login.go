package iam

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/google/uuid"
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

	ok, err := VerifyPassword(password, encoded)
	if err != nil || !ok {
		logger.WarnContext(ctx, "login: rejected", slog.String("reason", "invalid_credentials"), slog.String("tenant_id", tenantID.String()))
		return Session{}, ErrInvalidCredentials
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
