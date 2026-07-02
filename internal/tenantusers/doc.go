// Package tenantusers is the pure domain for the tenant user-management
// surface (/settings/users, SIN-66499, parent SIN-66492 / SIN-66493).
//
// A tenant gerente manages the users of their OWN tenant: list them,
// create new ones (as tenant_gerente or tenant_atendente), change a user's
// role, and deactivate (soft-delete) a user. The package holds:
//
//   - the User aggregate (user.go) with its invariants;
//   - the storage port Repository, the PasswordHasher port, and the
//     Auditor port (port.go);
//   - the four use cases on Service (service.go): ListUsers, CreateUser,
//     UpdateUserRole, DeactivateUser.
//
// It is pure domain: no database/sql, no net/http, no vendor SDKs. The
// concrete Postgres adapter lives in
// internal/adapter/db/postgres/tenantusers and the HTMX surface in
// internal/web/tenantusers.
//
// Two safety invariants are enforced in the use cases (belt to the RLS +
// CHECK-constraint braces in the adapter/schema):
//
//   - Anti-escalation: the input role is restricted to the closed set
//     {tenant_gerente, tenant_atendente}. master / is_master / any other
//     value is rejected. is_master is never settable through this package
//     — the User aggregate has no such field and the adapter always writes
//     is_master = false.
//   - Anti-lockout: the last active gerente of a tenant can be neither
//     deactivated nor demoted, so a tenant can never lock itself out of
//     its own admin surface.
package tenantusers
