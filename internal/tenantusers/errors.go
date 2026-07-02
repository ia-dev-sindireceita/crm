package tenantusers

import "errors"

var (
	// ErrInvalidTenant is returned when a tenant id is uuid.Nil. Every
	// operation is tenant-scoped; a nil tenant is a programmer error, never
	// a cross-tenant read.
	ErrInvalidTenant = errors.New("tenantusers: tenant id is required")

	// ErrInvalidEmail is returned when the supplied email is empty or not a
	// syntactically valid address.
	ErrInvalidEmail = errors.New("tenantusers: invalid email")

	// ErrRoleNotAssignable is returned when the requested role is outside
	// the closed assignable set {tenant_gerente, tenant_atendente}. This is
	// the anti-escalation guard: master / is_master / legacy / forged values
	// are all rejected here.
	ErrRoleNotAssignable = errors.New("tenantusers: role must be tenant_gerente or tenant_atendente")

	// ErrEmptyPasswordHash is returned by New when the encoded password hash
	// is empty — a user must always be created with a hash so the row is
	// never loginnable with an empty credential.
	ErrEmptyPasswordHash = errors.New("tenantusers: password hash is required")

	// ErrEmailConflict is returned by Repository.Create when a user with the
	// same email already exists in the tenant (UNIQUE(tenant_id, email)).
	ErrEmailConflict = errors.New("tenantusers: a user with that email already exists")

	// ErrNotFound is returned when no user matches (tenant, id) under the
	// tenant scope — including rows hidden by RLS.
	ErrNotFound = errors.New("tenantusers: user not found")

	// ErrLastGerente is the anti-lockout guardrail: deactivating or demoting
	// the last active gerente of a tenant is refused so the tenant can never
	// lose its only admin.
	ErrLastGerente = errors.New("tenantusers: cannot deactivate or demote the last active gerente")
)
