package tenantusers

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// Service holds the four tenant user-management use cases. It depends only
// on the ports (Repository, PasswordHasher, Auditor) so the domain stays
// free of storage / transport concerns.
type Service struct {
	repo   Repository
	hasher PasswordHasher
	audit  Auditor // optional; nil disables audit emission
	nowFn  func() time.Time
}

// NewService validates and wires a Service. repo and hasher are required;
// audit is optional (nil = fail-soft, no emission). A nil clock defaults to
// time.Now (UTC).
func NewService(repo Repository, hasher PasswordHasher, audit Auditor) (*Service, error) {
	if repo == nil {
		return nil, errors.New("tenantusers: repository is required")
	}
	if hasher == nil {
		return nil, errors.New("tenantusers: password hasher is required")
	}
	return &Service{
		repo:   repo,
		hasher: hasher,
		audit:  audit,
		nowFn:  func() time.Time { return time.Now().UTC() },
	}, nil
}

// CreateResult is returned by CreateUser. TempPassword is the one-time
// system-generated plaintext the caller shows to the gerente exactly once;
// it is never persisted in plaintext, logged, or put in a URL.
type CreateResult struct {
	User         *User
	TempPassword string
}

// ListUsers returns every user of the tenant, ordered by email.
func (s *Service) ListUsers(ctx context.Context, tenantID uuid.UUID) ([]*User, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	return s.repo.List(ctx, tenantID)
}

// CreateUser creates a tenant user with a system-generated temporary
// password. role must be assignable (anti-escalation); tenantID is the
// authenticated gerente's tenant. On success it returns the persisted user
// and the one-time plaintext, and emits a tenant.user.created audit event.
func (s *Service) CreateUser(ctx context.Context, tenantID, actorID uuid.UUID, email string, role iam.Role) (*CreateResult, error) {
	if tenantID == uuid.Nil {
		return nil, ErrInvalidTenant
	}
	// Validate the role up front so a forged/escalated value is rejected
	// before we spend an Argon2 derivation. New re-checks defensively.
	if !RoleAssignable(role) {
		return nil, ErrRoleNotAssignable
	}
	temp, err := GenerateTempPassword()
	if err != nil {
		return nil, err
	}
	hash, err := s.hasher.Hash(temp)
	if err != nil {
		return nil, err
	}
	u, err := New(tenantID, email, role, hash)
	if err != nil {
		return nil, err
	}
	if err := s.repo.Create(ctx, u); err != nil {
		return nil, err
	}
	if s.audit != nil && actorID != uuid.Nil {
		s.audit.UserCreated(ctx, actorID, tenantID, u.ID, u.Email, u.Role)
	}
	return &CreateResult{User: u, TempPassword: temp}, nil
}

// UpdateUserRole changes a user's role. newRole must be assignable
// (anti-escalation). Demoting the last active gerente is refused
// (ErrLastGerente). A no-op change (role unchanged) returns nil without an
// audit line. On a real change it emits tenant.user.role_changed.
func (s *Service) UpdateUserRole(ctx context.Context, tenantID, actorID, targetID uuid.UUID, newRole iam.Role) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	if !RoleAssignable(newRole) {
		return ErrRoleNotAssignable
	}
	u, err := s.repo.Get(ctx, tenantID, targetID)
	if err != nil {
		return err
	}
	if u.Role == newRole {
		return nil // idempotent no-op
	}
	// Anti-lockout: demoting an active gerente is only allowed while another
	// active gerente remains.
	if u.Active && u.Role == iam.RoleTenantGerente && newRole != iam.RoleTenantGerente {
		if err := s.guardLastGerente(ctx, tenantID); err != nil {
			return err
		}
	}
	if err := s.repo.UpdateRole(ctx, tenantID, targetID, newRole); err != nil {
		return err
	}
	if s.audit != nil && actorID != uuid.Nil {
		s.audit.UserRoleChanged(ctx, actorID, tenantID, targetID, u.Role, newRole)
	}
	return nil
}

// DeactivateUser soft-deletes a user (active=false). Already-inactive is an
// idempotent no-op (nil, no audit). Deactivating the last active gerente is
// refused (ErrLastGerente). On a real deactivation it emits
// tenant.user.deactivated.
func (s *Service) DeactivateUser(ctx context.Context, tenantID, actorID, targetID uuid.UUID) error {
	if tenantID == uuid.Nil {
		return ErrInvalidTenant
	}
	u, err := s.repo.Get(ctx, tenantID, targetID)
	if err != nil {
		return err
	}
	if !u.Active {
		return nil // already inactive — idempotent
	}
	if u.Role == iam.RoleTenantGerente {
		if err := s.guardLastGerente(ctx, tenantID); err != nil {
			return err
		}
	}
	if err := s.repo.SetActive(ctx, tenantID, targetID, false); err != nil {
		return err
	}
	if s.audit != nil && actorID != uuid.Nil {
		s.audit.UserDeactivated(ctx, actorID, tenantID, targetID, u.Role)
	}
	return nil
}

// guardLastGerente returns ErrLastGerente when the tenant has one or zero
// active gerentes — i.e. the target being deactivated/demoted is the last
// one. The count includes the target (still active/gerente at this point),
// so "last" means count <= 1.
//
// TOCTOU note: the count and the subsequent write are not one transaction,
// so two concurrent deactivations of the last two gerentes could in theory
// both pass the guard. The window is small (a single tenant-admin acting)
// and the failure mode (a tenant with zero active gerentes) is recoverable
// by re-activating via master impersonation. A DB-level trigger is the
// hardening path if this proves insufficient (flagged for the SecurityEngineer
// follow-up).
func (s *Service) guardLastGerente(ctx context.Context, tenantID uuid.UUID) error {
	n, err := s.repo.CountActiveGerentes(ctx, tenantID)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastGerente
	}
	return nil
}
