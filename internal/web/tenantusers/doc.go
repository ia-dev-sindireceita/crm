// Package tenantusers serves the SIN-66499 tenant user-management admin
// surface (/settings/users). A tenant gerente lists the users of their own
// tenant, creates new ones (as gerente or atendente), changes a user's role,
// and deactivates (soft-deletes) a user.
//
// The package is the HTMX transport for the internal/tenantusers domain: it
// resolves the tenant from the request context (never from input), calls the
// domain Service, and renders server-side HTML with OOB swaps under the
// shared app-shell. All routes are gated to RoleTenantGerente at the router
// via RequireAction(ActionTenantUser*); this package assumes the gate ran.
package tenantusers
