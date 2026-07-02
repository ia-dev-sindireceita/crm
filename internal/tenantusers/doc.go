// Package tenantusers is the domain core for tenant-scoped user management
// (SIN-66496, child of SIN-66493 / board SIN-66492). A tenant "gerente"
// (RoleTenantGerente) lists, creates, changes the role of, and soft-
// deactivates users WITHIN THEIR OWN TENANT.
//
// Hexagonal boundaries (this package is pure domain):
//   - It imports NO database/sql, HTTP framework, or vendor SDK. Storage
//     lives behind the Repository port (adapter: internal/adapter/db/
//     postgres/tenantusers). Password hashing and audit are consumed via
//     the narrow Hasher / Auditor interfaces.
//
// Security decisions consumed from the gate SIN-66494 (do NOT re-invent):
//   - Initial credential = OPTION A (invite-by-link). No usable server-side
//     password ever exists; the account is seeded with a random argon2 hash
//     and an invite Token (single-use, 72h TTL, only its SHA-256 stored).
//   - G1 allowlist: a gerente may only assign RoleTenantGerente or
//     RoleTenantAtendente. master / is_master / arbitrary values are
//     rejected (deny-by-default). is_master is never accepted from input.
//   - G2 tenant isolation: TenantID always comes from the authenticated
//     session (Actor), never from client input. RLS + the code path both
//     enforce it (defense in depth).
//   - G4 anti-lockout: a gerente cannot demote or deactivate the last
//     active gerente of the tenant (including themselves). Enforced without
//     TOCTOU by a guarded conditional UPDATE in the adapter.
//   - G5 soft-deactivate (desativar-não-deletar): DeactivatedAt is set;
//     the row is preserved; login rejects deactivated users.
package tenantusers
