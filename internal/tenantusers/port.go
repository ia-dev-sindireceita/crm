package tenantusers

import (
	"context"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
	"github.com/pericles-luz/crm/internal/iam/audit"
)

// Repository is the storage port for tenant user management. Every method is
// tenant-scoped: the adapter MUST run inside the tenant's RLS envelope
// (app.tenant_id) so a cross-tenant id resolves to not-found, never to
// another tenant's row.
type Repository interface {
	// List returns the tenant's users (active first, then by email).
	List(ctx context.Context, tenantID uuid.UUID) ([]User, error)

	// Get returns one user by id within the tenant, or ErrUserNotFound.
	Get(ctx context.Context, tenantID, id uuid.UUID) (User, error)

	// Create inserts the user and its invite token atomically. passwordHash
	// is a random, un-guessable argon2 hash (the account is un-loginable
	// until the invite token is consumed). Returns ErrEmailTaken on a
	// unique-email violation within the tenant.
	Create(ctx context.Context, u User, passwordHash string, tok Token) error

	// UpdateRole sets the user's role, guarded against anti-lockout (G4):
	// demoting the last active gerente returns ErrLastGerente. Returns the
	// role the user had before the change. ErrUserNotFound if absent.
	UpdateRole(ctx context.Context, tenantID, id uuid.UUID, newRole iam.Role) (before iam.Role, err error)

	// Deactivate soft-deactivates the user (sets deactivated_at), guarded
	// against anti-lockout (G4): deactivating the last active gerente returns
	// ErrLastGerente. Returns the user's role. ErrUserNotFound if absent or
	// already deactivated.
	Deactivate(ctx context.Context, tenantID, id uuid.UUID) (role iam.Role, err error)

	// Reactivate clears deactivated_at. Returns the user's role.
	// ErrUserNotFound if absent or already active.
	Reactivate(ctx context.Context, tenantID, id uuid.UUID) (role iam.Role, err error)
}

// Hasher hashes a plaintext password (argon2id). Satisfied by
// internal/iam/password.Argon2idHasher.
type Hasher interface {
	Hash(plain string) (string, error)
}

// Auditor writes a security audit event. Satisfied by
// internal/iam/audit.SplitLogger. Kept narrow so the domain depends only on
// what it uses (accept-broad / return-narrow).
type Auditor interface {
	WriteSecurity(ctx context.Context, ev audit.SecurityAuditEvent) error
}
