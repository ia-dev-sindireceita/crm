package tenantusers

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// Service holds the tenant user-management use cases. It depends only on
// ports (Repository) and narrow collaborators (Hasher, Auditor) — no
// database/sql, no HTTP.
type Service struct {
	repo    Repository
	hasher  Hasher
	auditor Auditor
	logger  *slog.Logger
	now     func() time.Time
	randn   func([]byte) (int, error)
}

// Config configures a Service. Repo, Hasher and Auditor are required; Now,
// Rand and Logger default to production values.
type Config struct {
	Repo    Repository
	Hasher  Hasher
	Auditor Auditor
	Logger  *slog.Logger
	Now     func() time.Time
	Rand    func([]byte) (int, error)
}

// NewService validates deps and returns a Service.
func NewService(cfg Config) (*Service, error) {
	if cfg.Repo == nil {
		return nil, errors.New("tenantusers: Repo is required")
	}
	if cfg.Hasher == nil {
		return nil, errors.New("tenantusers: Hasher is required")
	}
	if cfg.Auditor == nil {
		return nil, errors.New("tenantusers: Auditor is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return time.Now().UTC() }
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Read
	}
	return &Service{
		repo:    cfg.Repo,
		hasher:  cfg.Hasher,
		auditor: cfg.Auditor,
		logger:  cfg.Logger,
		now:     cfg.Now,
		randn:   cfg.Rand,
	}, nil
}

// List returns the acting gerente's tenant users. Tenant scope comes from the
// Actor (session), never from input.
func (s *Service) List(ctx context.Context, actor Actor) ([]User, error) {
	return s.repo.List(ctx, actor.TenantID)
}

// Get returns one tenant user by id.
func (s *Service) Get(ctx context.Context, actor Actor, id uuid.UUID) (User, error) {
	return s.repo.Get(ctx, actor.TenantID, id)
}

// Create provisions a new tenant user under the invite-by-link flow
// (SIN-66494 §1 Option A). The role is validated against the G1 allowlist;
// the account is seeded with a random un-guessable password hash and an
// invite token is minted. The returned Token.Plaintext is used to build the
// invite link and is never persisted. TenantID comes from the Actor (G2).
func (s *Service) Create(ctx context.Context, actor Actor, rawEmail string, role iam.Role) (User, Token, error) {
	email, err := normalizeEmail(rawEmail)
	if err != nil {
		return User{}, Token{}, err
	}
	if !AssignableRole(role) {
		// Deny-by-default: master / is_master / arbitrary values never pass.
		return User{}, Token{}, ErrInvalidRole
	}

	hash, err := s.randomPasswordHash()
	if err != nil {
		return User{}, Token{}, err
	}
	now := s.now()
	tok, err := GenerateToken(s.randn, PurposeInvite, now, InviteTTL)
	if err != nil {
		return User{}, Token{}, err
	}

	u := User{
		ID:        uuid.New(),
		TenantID:  actor.TenantID,
		Email:     email,
		Role:      role,
		CreatedAt: now,
	}
	if err := s.repo.Create(ctx, u, hash, tok); err != nil {
		return User{}, Token{}, err
	}

	s.audit(ctx, actor, audit.SecurityEventUserCreate, map[string]any{
		"target_user_id": u.ID.String(),
		"role":           string(role),
		"invited":        true,
	})
	return u, tok, nil
}

// UpdateRole changes a user's role. The new role is validated against the G1
// allowlist and the adapter enforces anti-lockout (G4).
func (s *Service) UpdateRole(ctx context.Context, actor Actor, id uuid.UUID, newRole iam.Role) (User, error) {
	if !AssignableRole(newRole) {
		return User{}, ErrInvalidRole
	}
	before, err := s.repo.UpdateRole(ctx, actor.TenantID, id, newRole)
	if err != nil {
		return User{}, err
	}
	s.audit(ctx, actor, audit.SecurityEventRoleChange, map[string]any{
		"target_user_id": id.String(),
		"before_role":    string(before),
		"after_role":     string(newRole),
	})
	return s.repo.Get(ctx, actor.TenantID, id)
}

// Deactivate soft-deactivates a user. The adapter enforces anti-lockout (G4):
// the last active gerente (including self) cannot be deactivated.
func (s *Service) Deactivate(ctx context.Context, actor Actor, id uuid.UUID) (User, error) {
	before, err := s.repo.Deactivate(ctx, actor.TenantID, id)
	if err != nil {
		return User{}, err
	}
	s.audit(ctx, actor, audit.SecurityEventUserDeactivate, map[string]any{
		"target_user_id": id.String(),
		"before_role":    string(before),
	})
	return s.repo.Get(ctx, actor.TenantID, id)
}

// Reactivate clears a user's deactivation.
func (s *Service) Reactivate(ctx context.Context, actor Actor, id uuid.UUID) (User, error) {
	if _, err := s.repo.Reactivate(ctx, actor.TenantID, id); err != nil {
		return User{}, err
	}
	s.audit(ctx, actor, audit.SecurityEventUserReactivate, map[string]any{
		"target_user_id": id.String(),
	})
	return s.repo.Get(ctx, actor.TenantID, id)
}

// randomPasswordHash produces an argon2 hash of a random, un-guessable secret.
// No one — not even the creating gerente — knows the plaintext, so the account
// cannot be logged into until the invite token is consumed and a real
// password is set (Option A: no usable server-side password ever exists).
func (s *Service) randomPasswordHash() (string, error) {
	b := make([]byte, 32)
	if _, err := s.randn(b); err != nil {
		return "", fmt.Errorf("tenantusers: random password: %w", err)
	}
	hash, err := s.hasher.Hash(base64.RawURLEncoding.EncodeToString(b))
	if err != nil {
		return "", fmt.Errorf("tenantusers: hash placeholder password: %w", err)
	}
	return hash, nil
}

// audit writes a tenant-scoped security event. Actor and tenant come from the
// session. Audit failure is logged but does not roll back the (already
// committed) mutation — consistent with the existing security-audit callers.
func (s *Service) audit(ctx context.Context, actor Actor, ev audit.SecurityEvent, target map[string]any) {
	tenantID := actor.TenantID
	if err := s.auditor.WriteSecurity(ctx, audit.SecurityAuditEvent{
		Event:       ev,
		ActorUserID: actor.UserID,
		TenantID:    &tenantID,
		Target:      target,
		OccurredAt:  s.now(),
	}); err != nil {
		s.logger.Error("tenantusers: audit write failed", "event", string(ev), "err", err)
	}
}
