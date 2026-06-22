package usermfa

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/mfa"
)

// TenantSessionActor is the server-derived identity a full post-login
// tenant session (__Host-sess-tenant) resolves to. Both ids come from
// the validated session row — never from request-supplied form/query
// input (OWASP A01 Broken Access Control / deny-by-default).
type TenantSessionActor struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
}

// ErrNoTenantSession is the sentinel a TenantSessionResolver returns when
// the presented session id does not resolve to a live session for the
// tenant — missing, malformed, expired, or a cross-tenant probe. Every
// failure mode collapses to this single value so a caller (and a hostile
// probe) cannot distinguish "no session" from "wrong tenant".
var ErrNoTenantSession = errors.New("usermfa: no tenant session")

// TenantSessionResolver validates a __Host-sess-tenant cookie value
// against the host-resolved tenant scope and returns the server-derived
// actor. It is the second access predicate for GET/POST /admin/2fa/setup
// (the voluntary post-login path; the __Host-mfa-pending cookie is the
// first, mid-login path).
//
// Read-narrow / return-narrow (hexagonal accept-broad/return-narrow):
// the handler supplies the tenant id (from the host gate) and the session
// id parsed from the cookie; the resolver never trusts a user- or
// tenant-id taken from request input, and it returns only the two ids the
// setup flow needs. Implementations MUST return ErrNoTenantSession for
// every non-resolving case so the handler can fall through to the pending
// predicate without leaking which check failed.
type TenantSessionResolver interface {
	ResolveTenantSession(ctx context.Context, tenantID, sessionID uuid.UUID) (TenantSessionActor, error)
}

// Enroller is the slice of mfa.Service the setup handler depends on.
type Enroller interface {
	Enroll(ctx context.Context, userID uuid.UUID, label string) (mfa.EnrollResult, error)
}

// Verifier is the slice of mfa.Service the verify handler depends on
// for TOTP submission.
type Verifier interface {
	Verify(ctx context.Context, userID uuid.UUID, code string) error
}

// RecoveryConsumer is the slice of mfa.Service the verify handler
// depends on for recovery-code submission.
type RecoveryConsumer interface {
	ConsumeRecovery(ctx context.Context, userID uuid.UUID, submitted string, reqCtx mfa.RequestContext) error
}

// RecoveryRegenerator is the slice of mfa.Service the regenerate
// handler depends on.
type RecoveryRegenerator interface {
	RegenerateRecovery(ctx context.Context, userID uuid.UUID, reqCtx mfa.RequestContext) ([]string, error)
}

// PendingStore is the persistence port the verify handler uses to
// resolve the pending-MFA cookie back to a (userID, tenantID, next)
// triple and to delete the row on successful verify or lockout trip.
type PendingStore interface {
	Get(ctx context.Context, id uuid.UUID) (Pending, error)
	Delete(ctx context.Context, id uuid.UUID) error
}

// Pending is the minimal projection of a pending-MFA row the verify
// handler needs.
type Pending struct {
	ID        uuid.UUID
	UserID    uuid.UUID
	TenantID  uuid.UUID
	ExpiresAt time.Time
	NextPath  string
}

// IsExpired reports whether now is at or past ExpiresAt.
func (p Pending) IsExpired(now time.Time) bool {
	return !now.Before(p.ExpiresAt)
}

// EnrollmentChecker is the read port the verify handler uses to
// decide whether the user has completed enrolment yet. A pending
// cookie holder who has never enrolled is redirected to /admin/2fa/setup
// instead of being rejected.
type EnrollmentChecker interface {
	IsEnrolled(ctx context.Context, userID uuid.UUID) (bool, error)
}

// Reenroller is the write port the verify handler uses to force a
// re-enrolment when the stored seed ciphertext is unreadable under
// the current SeedCipher key. Calling MarkReenrollRequired flips the
// row so the next IsEnrolled check returns false and the password-only
// login lands the user on /admin/2fa/setup for a fresh enrolment (the
// same self-heal path the recovery-code flow already uses).
type Reenroller interface {
	MarkReenrollRequired(ctx context.Context, userID uuid.UUID) error
}

// SessionMinter creates the post-verify tenant session row and returns
// it. The handler writes the session cookie + CSRF cookie from the
// fields on the returned iam.Session.
type SessionMinter interface {
	MintTenantSession(ctx context.Context, tenantID, userID uuid.UUID, ipAddr, userAgent string) (iam.Session, error)
}

// FailureCounter is the per-pending-session invalid-submission counter
// the verify handler increments on every wrong code. Identical
// contract to mastermfa.VerifyFailureCounter — see that interface for
// the rationale.
type FailureCounter interface {
	Increment(ctx context.Context, pendingID uuid.UUID) (int, error)
	Reset(ctx context.Context, pendingID uuid.UUID) error
}

// AuditEmitter is the slice the handlers call to record bypass attempts
// and lockout events. mfa.AuditLogger satisfies it; the
// usermfa.TenantAuditLogger adapter writes to audit_log_security.
type AuditEmitter interface {
	LogMFARequired(ctx context.Context, userID uuid.UUID, route, reason string) error
}

// UserLabelReader returns the display label the otpauth:// URI shows
// under the issuer string (typically the user's email).
type UserLabelReader interface {
	LookupLabel(ctx context.Context, tenantID, userID uuid.UUID) (string, error)
}

// remoteIP strips the port off r.RemoteAddr and parses the host
// portion as a best-effort. Returns "" when the address is missing or
// unparseable.
func remoteIP(r *http.Request) string {
	if r == nil || r.RemoteAddr == "" {
		return ""
	}
	addr := r.RemoteAddr
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}
