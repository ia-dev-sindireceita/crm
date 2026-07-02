// Package users is the HTMX admin surface for tenant user management
// (SIN-66496, child of SIN-66493). A tenant gerente (RoleTenantGerente) lists,
// creates, changes the role of, and soft-deactivates users of their own
// tenant at /settings/users.
//
// Server-rendered HTML with HTMX partial swaps (no SPA). Follows the
// /settings/ai-policy and /settings/privacy pattern: one Deps struct, an
// html/template inlined in templates.go, htmx loaded on the page under the
// CSP nonce.
//
// Security posture (consumed from SIN-66494, enforced by the router's
// RequireAction gate + the tenantusers domain):
//   - The whole surface is gerente-only (G3). RequireAction denies atendente
//     / common with 403 before the handler runs.
//   - TenantID + UserID come from the authenticated Principal (session),
//     never from client input (G2).
//   - Role selectors offer only Atendente / Gerente — never master (G1); the
//     domain re-validates (deny-by-default) so a forged form is rejected.
//   - Deactivate / demote of the last active gerente is blocked server-side
//     (G4, ErrLastGerente → 409); the UI only anticipates it.
package users
