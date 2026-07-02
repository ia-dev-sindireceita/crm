package tenantusers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam/audit"
	"github.com/pericles-luz/crm/internal/iam/password"
)

// ErrTokenInvalid is the single, cause-hiding error the credential-token
// consumption flow returns when a token cannot be used: not found, wrong
// tenant, expired, or already consumed. It deliberately does NOT distinguish
// which of the three it was — the public set-password page renders one
// generic error so an attacker cannot use the response to enumerate valid
// tokens or probe expiry (SIN-66510 §1 "não vazar qual das três").
var ErrTokenInvalid = errors.New("tenantusers: credential token invalid, expired, or already used")

// Invite is the still-valid invite resolved from a credential token. It
// carries only what the set-password flow needs: the target user, their
// email (for the ADR 0070 §5 identity rule and the audit target), and the
// token purpose. It NEVER carries password material or the token itself.
type Invite struct {
	UserID  uuid.UUID
	Email   string
	Purpose Purpose
}

// CredentialTokenRepository is the storage port for consuming credential
// tokens (invite / reset). It is deliberately SEPARATE from Repository so
// that the mint-side fakes need no new methods — the two concerns (manage
// users vs. consume a token) have different callers and lifetimes.
//
// Every method is tenant-scoped: the adapter MUST run inside the tenant's
// RLS envelope (app.tenant_id) so a token minted for another tenant resolves
// to ErrTokenInvalid, never to a cross-tenant row. Lookup is ALWAYS by hash
// (token_sha256) — the plaintext is never compared or stored.
type CredentialTokenRepository interface {
	// LookupToken validates a token by hash within the tenant and returns the
	// Invite when the token exists, is unexpired (expires_at > now) and
	// unconsumed (consumed_at IS NULL); otherwise ErrTokenInvalid. This is a
	// read used to render the GET page and to fetch the email for the policy
	// identity check — it is NOT the single-use gate (see ConsumeToken).
	LookupToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte, now time.Time) (Invite, error)

	// ConsumeToken atomically marks the token consumed AND writes the user's
	// new password hash in ONE tenant-scoped transaction. This is the
	// single-use gate: the UPDATE is guarded on consumed_at IS NULL so two
	// concurrent consumers cannot both win (no TOCTOU / replay). A token that
	// is already consumed or expired at commit time returns ErrTokenInvalid.
	// Returns the id of the user whose password was set.
	ConsumeToken(ctx context.Context, tenantID uuid.UUID, tokenHash []byte, now time.Time, newPasswordHash string) (uuid.UUID, error)
}

// PasswordPolicy validates a plaintext password against ADR 0070 §5 (length
// bounds, identity equality, breach-corpus screening). Satisfied by
// *password.Policy — reused, not re-rolled (SIN-66510 lens). Returns a
// *password.PolicyError naming the first failed rule, or nil on pass.
type PasswordPolicy interface {
	PolicyCheck(ctx context.Context, plain string, pctx password.PolicyContext) error
}

// CredentialService is the use case behind the public set-password page. It
// depends only on ports (CredentialTokenRepository), the reused argon2id
// Hasher, the reused password Policy, and the Auditor — no database/sql, no
// HTTP.
type CredentialService struct {
	repo    CredentialTokenRepository
	hasher  Hasher
	policy  PasswordPolicy
	auditor Auditor
	logger  *slog.Logger
	now     func() time.Time
}

// CredentialConfig configures a CredentialService. Repo, Hasher, Policy and
// Auditor are required; Logger and Now default to production values.
type CredentialConfig struct {
	Repo    CredentialTokenRepository
	Hasher  Hasher
	Policy  PasswordPolicy
	Auditor Auditor
	Logger  *slog.Logger
	Now     func() time.Time
}

// NewCredentialService validates deps and returns a CredentialService.
func NewCredentialService(cfg CredentialConfig) (*CredentialService, error) {
	if cfg.Repo == nil {
		return nil, errors.New("tenantusers: credential Repo is required")
	}
	if cfg.Hasher == nil {
		return nil, errors.New("tenantusers: credential Hasher is required")
	}
	if cfg.Policy == nil {
		return nil, errors.New("tenantusers: credential Policy is required")
	}
	if cfg.Auditor == nil {
		return nil, errors.New("tenantusers: credential Auditor is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	return &CredentialService{
		repo:    cfg.Repo,
		hasher:  cfg.Hasher,
		policy:  cfg.Policy,
		auditor: cfg.Auditor,
		logger:  cfg.Logger,
		now:     cfg.Now,
	}, nil
}

// Resolve validates a token for the GET set-password page: it hashes the
// plaintext and looks it up by hash within the tenant. Any failure — empty
// token, not found, expired, consumed — collapses to ErrTokenInvalid so the
// page renders one generic error. The plaintext token is never logged.
func (s *CredentialService) Resolve(ctx context.Context, tenantID uuid.UUID, plaintextToken string) (Invite, error) {
	if plaintextToken == "" {
		return Invite{}, ErrTokenInvalid
	}
	return s.repo.LookupToken(ctx, tenantID, HashToken(plaintextToken), s.now())
}

// SetPassword consumes an invite/reset token and sets the user's password:
//
//  1. Resolve the token by hash (read) — ErrTokenInvalid if unusable.
//  2. Run the ADR 0070 §5 policy check with the resolved email as the
//     identity value. On failure the token is NOT consumed, so the invitee
//     can retry with a stronger password (good UX, no token burn).
//  3. Hash the accepted password with the reused argon2id Hasher.
//  4. Atomically consume the token AND write the hash in one transaction —
//     the single-use gate. A lost race returns ErrTokenInvalid.
//  5. Emit a password_reset security audit event (self-service: the actor
//     IS the user who set their own password).
//
// pctx carries the per-request identity/tenant values; SetPassword overrides
// pctx.Email with the resolved invite email so the identity rule cannot be
// bypassed by a crafted request body. On policy failure it returns the
// *password.PolicyError so the boundary renders a localized message.
func (s *CredentialService) SetPassword(ctx context.Context, tenantID uuid.UUID, plaintextToken, newPassword string, pctx password.PolicyContext) (Invite, error) {
	if plaintextToken == "" {
		return Invite{}, ErrTokenInvalid
	}
	hash := HashToken(plaintextToken)

	inv, err := s.repo.LookupToken(ctx, tenantID, hash, s.now())
	if err != nil {
		return Invite{}, err
	}

	pctx.Email = inv.Email
	if err := s.policy.PolicyCheck(ctx, newPassword, pctx); err != nil {
		return Invite{}, err
	}

	encoded, err := s.hasher.Hash(newPassword)
	if err != nil {
		return Invite{}, fmt.Errorf("tenantusers: hash password: %w", err)
	}

	userID, err := s.repo.ConsumeToken(ctx, tenantID, hash, s.now(), encoded)
	if err != nil {
		return Invite{}, err
	}

	s.auditPasswordReset(ctx, tenantID, userID, inv.Purpose)
	return Invite{UserID: userID, Email: inv.Email, Purpose: inv.Purpose}, nil
}

// auditPasswordReset writes the tenant-scoped password_reset event. The actor
// is the user themselves (self-service consumption). Audit failure is logged
// but does not roll back the already-committed password write — consistent
// with the existing tenantusers audit callers.
func (s *CredentialService) auditPasswordReset(ctx context.Context, tenantID, userID uuid.UUID, purpose Purpose) {
	tid := tenantID
	if err := s.auditor.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       audit.SecurityEventPasswordReset,
		ActorUserID: userID,
		TenantID:    &tid,
		Target: map[string]any{
			"user_id": userID.String(),
			"purpose": string(purpose),
			"via":     "credential_token",
		},
		OccurredAt: s.now(),
	}); err != nil {
		s.logger.Error("tenantusers: password_reset audit write failed", "err", err)
	}
}
