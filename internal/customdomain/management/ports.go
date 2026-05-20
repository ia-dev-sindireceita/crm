package management

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Store is the persistence port for tenant_custom_domains. Implemented
// by internal/adapter/store/postgres.CustomDomainStore in production
// and an in-memory fake in tests.
//
// The contract is intentionally CRUD-flavoured but does not leak SQL —
// every method returns the canonical Domain struct or a typed error.
//
// ErrNotFound is returned when no row matches the given id (or the row
// is soft-deleted on lookups that exclude deleted rows).
type Store interface {
	List(ctx context.Context, tenantID uuid.UUID) ([]Domain, error)
	GetByID(ctx context.Context, id uuid.UUID) (Domain, error)
	Insert(ctx context.Context, d Domain) (Domain, error)
	// MarkVerified flips verified_at iff the row at id still has
	// verification_token = expectedToken (compare-and-swap, SIN-63104).
	// Returns ErrTokenRotated when the row exists but the token differs,
	// ErrStoreNotFound when the row is missing or soft-deleted.
	MarkVerified(ctx context.Context, id uuid.UUID, expectedToken string, at time.Time, withDNSSEC bool, dnsLogID *uuid.UUID) (Domain, error)
	// RotateToken replaces verification_token with newToken and stamps
	// token_issued_at = issuedAt iff the row at id is still unverified
	// (verified_at IS NULL) and not soft-deleted (SIN-63125). Verified
	// rows surface ErrAlreadyVerified — the use-case refuses to rotate a
	// token that has already proven domain ownership. Missing or soft-
	// deleted rows surface ErrStoreNotFound.
	//
	// Only the verification_token + token_issued_at + updated_at columns
	// move. Audit lineage (dns_resolution_log_id) is preserved so
	// forensics can reconstruct "old token never propagated → regenerated
	// → new token succeeded" without joining the audit log.
	RotateToken(ctx context.Context, id uuid.UUID, newToken string, issuedAt time.Time) (Domain, error)
	SetPaused(ctx context.Context, id uuid.UUID, pausedAt *time.Time) (Domain, error)
	SoftDelete(ctx context.Context, id uuid.UUID, at time.Time) (Domain, error)
}

// ErrStoreNotFound is the sentinel implementations return when the row
// does not exist. The use-case maps it to ReasonNotFound.
var ErrStoreNotFound = errors.New("management: domain not found")

// EnrollmentGate is the per-tenant quota and circuit-breaker check.
// Defined here as a narrow port so tests can pass a fake; production
// wires *enrollment.UseCase behind a thin adapter.
type EnrollmentGate interface {
	Allow(ctx context.Context, tenantID uuid.UUID) EnrollmentDecision
}

// EnrollmentDecision is the management-package projection of the
// underlying enrollment.Result. Fewer fields, no leakage of the
// underlying type.
type EnrollmentDecision struct {
	Allowed    bool
	Reason     Reason
	RetryAfter time.Duration
	Err        error
}

// HostValidator is the FQDN + IP-literal + private-network rejection
// gate that lives in customdomain/validation when SIN-62242 lands. The
// management layer accepts any implementation so the validator can be
// upgraded without touching the orchestrator.
type HostValidator interface {
	Validate(ctx context.Context, host string) error
}

// DNSChecker resolves the TXT record at _crm-verify.<host> and
// confirms it carries the expected token. The boundary translates
// errors.Is(err, ErrTokenMismatch) → ReasonTokenMismatch, and the
// equivalents for other sentinels.
type DNSChecker interface {
	Check(ctx context.Context, host, expectedToken string) (DNSCheckResult, error)
}

// DNSCheckResult is the outcome of a DNS verification attempt. WithDNSSEC
// flips the badge to "verified with DNSSEC" in the UI; LogID points to
// the dns_resolution_log row the checker writes.
type DNSCheckResult struct {
	WithDNSSEC bool
	LogID      *uuid.UUID
}

// SlugReleaser is the slug-reservation port the delete path consults.
// Wired to *slugreservation.Service in production. Returning an error
// rolls back the soft-delete attempt.
type SlugReleaser interface {
	ReleaseSlug(ctx context.Context, slug string, byTenantID uuid.UUID) error
}

// AuditLogger emits one event per management action so the audit trail
// captures who did what. nil-safe: the use-case skips the call when
// nil. Contract is fire-and-forget; implementations must not block.
type AuditLogger interface {
	LogManagement(ctx context.Context, ev AuditEvent)
}

// AuditEvent is a flat structure the slog adapter renders to JSON.
type AuditEvent struct {
	TenantID uuid.UUID
	DomainID uuid.UUID
	Host     string
	Action   string // "enroll", "verify", "pause", "resume", "delete"
	Outcome  string // "ok", "denied:<reason>", "error"
	Reason   Reason
	At       time.Time
	// TokenFingerprint is a non-reversible identifier for the verification
	// token bound to this event — first 16 hex chars of SHA-256(token).
	// Populated by Verify on every token-bound outcome (success, expired,
	// rotated, mismatch); empty on actions that don't bind a token.
	// Never carries the raw token. SIN-63104.
	TokenFingerprint string
}

// TokenGenerator returns the verification token the tenant must place in
// the TXT record. Tests inject deterministic values; production uses a
// crypto/rand-backed generator.
type TokenGenerator func() (string, error)

// Clock is the wall-clock injection used across the use-case. Defaults
// to time.Now when nil.
type Clock func() time.Time
