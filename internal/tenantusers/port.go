package tenantusers

import (
	"context"

	"github.com/google/uuid"

	"github.com/pericles-luz/crm/internal/iam"
)

// Repository is the storage port for User aggregates. The concrete adapter
// lives in internal/adapter/db/postgres/tenantusers.
//
// Every method is tenant-scoped. The Postgres adapter runs each call inside
// postgres.WithTenant, which sets the app.tenant_id GUC so the RLS policies
// on users (migration 0005) restrict the visible rows to the resolved
// tenant. Callers MUST pass the tenant derived from the authenticated
// session — NEVER from request input; a uuid.Nil tenant is a clean error,
// never a cross-tenant leak. Master rows (is_master=true, tenant_id NULL)
// are invisible to these tenant-scoped reads/writes by construction.
type Repository interface {
	// List returns every tenant user, ordered by email for a stable table.
	List(ctx context.Context, tenantID uuid.UUID) ([]*User, error)

	// Get returns the user with id under the tenant scope, or ErrNotFound
	// when no row matches (including RLS-hidden / master rows).
	Get(ctx context.Context, tenantID, id uuid.UUID) (*User, error)

	// Create persists a brand-new tenant user. The adapter always writes
	// is_master=false. Returns ErrEmailConflict when the INSERT would
	// violate UNIQUE(tenant_id, email).
	Create(ctx context.Context, u *User) error

	// UpdateRole changes the role of the user identified by (tenantID, id).
	// Returns ErrNotFound when no tenant row matches. The caller (Service)
	// guarantees role is assignable; the adapter refuses master rows.
	UpdateRole(ctx context.Context, tenantID, id uuid.UUID, role iam.Role) error

	// SetActive flips the active flag of the user identified by
	// (tenantID, id). Deactivation is soft (the row + history stay intact).
	// Returns ErrNotFound when no tenant row matches.
	SetActive(ctx context.Context, tenantID, id uuid.UUID, active bool) error

	// CountActiveGerentes returns how many active tenant_gerente users the
	// tenant currently has. Service uses it for the last-gerente anti-lockout
	// guardrail before a deactivate or a demotion.
	CountActiveGerentes(ctx context.Context, tenantID uuid.UUID) (int, error)
}

// PasswordHasher derives the encoded password hash for a freshly created
// user. It is the narrow slice of the ADR 0070 hasher the create use case
// needs; the composition root wires password.Default().
type PasswordHasher interface {
	Hash(plain string) (string, error)
}

// Auditor is the write-only port the use cases call to record a
// user-management privilege event (SIN-66499). Each create / role-change /
// deactivate is a privilege change OWASP A09 requires be audit-logged. The
// composition root routes each call into audit_log_security via the shared
// audit.SplitLogger; this port stays free of that vocabulary so the domain
// only speaks ids / emails / roles. Implementations are best-effort by
// contract: an audit-write failure is logged by the adapter and never
// surfaces back here (the mutation already committed). A nil Auditor
// disables emission (fail-soft wiring), so every method must tolerate the
// no-op path — the Service nil-guards before calling.
//
// actor is the authenticated gerente performing the change; tenant is the
// gerente's (and target's) tenant.
type Auditor interface {
	// UserCreated records that a user with the given email and role was
	// created.
	UserCreated(ctx context.Context, actor, tenant, targetUser uuid.UUID, email string, role iam.Role)
	// UserRoleChanged records that targetUser's role changed from -> to.
	UserRoleChanged(ctx context.Context, actor, tenant, targetUser uuid.UUID, from, to iam.Role)
	// UserDeactivated records that targetUser (carrying role) was
	// deactivated.
	UserDeactivated(ctx context.Context, actor, tenant, targetUser uuid.UUID, role iam.Role)
}
